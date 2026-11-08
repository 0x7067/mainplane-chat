package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/provider"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// resume follows the session a thread shows from the last record relayed.
func (b *bridge) resume(th, id string) {
	b.mu.Lock()
	t, ok := b.st.Threads[th]
	b.mu.Unlock()
	if ok && t.Session == id {
		b.follow(th, id, t.N)
		return
	}
	if i, err := b.info(id); err == nil {
		b.follow(th, id, i.N)
	}
}

// follow makes the thread show the session and long-polls the session after
// record n, relaying what the agent says. Once the thread shows another
// session it goes on until this one has been quiet for a poll, so a reply to
// a message sent before a quick /model still arrives. It stops when the
// harness refuses the session, as for one deleted.
func (b *bridge) follow(th, id string, n int) {
	b.bind(th, id, n)
	b.mu.Lock()
	k := [2]string{th, id}
	running := b.following[k]
	b.following[k] = true
	saved := b.st.Threads[th]
	b.mu.Unlock()
	if running {
		return
	}
	go func() {
		defer func() { b.mu.Lock(); delete(b.following, k); b.mu.Unlock() }()
		t := turn{
			attachmentExpected: saved.AttachmentPending,
			deliveryRecovery:   saved.AttachmentRecovery,
			actionReceipt:      saved.ActionReceipt,
			actionFailed:       saved.ActionFailed,
			attaching:          setOf(saved.Attaching),
			sending:            setOf(saved.Sending),
		}
		for {
			// Calls not shown yet, held back by the status throttle, show after a
			// short poll at most.
			wait := "60s"
			if t.dirty {
				wait = "3s"
			}
			recs, err := b.records(id, n, wait)
			var refused *refusedError
			if errors.As(err, &refused) {
				log.Printf("session %s: %v; no longer following it", id, err)
				if refused.gone() && b.shows(th, id) {
					b.chat(th).close(th)
					b.forget(th)
				}
				return
			}
			if err != nil {
				log.Printf("session %s: %v", id, err)
				time.Sleep(5 * time.Second)
				continue
			}
			if len(recs) == 0 && !b.shows(th, id) {
				return
			}
			for _, r := range recs {
				n = r.N
				b.relay(th, id, &t, r)
				// Text waiting for its step is not relayed yet, so a restart
				// shows it again.
				if len(t.text) == 0 {
					b.advance(th, id, n)
				}
			}
			if t.dirty && time.Since(t.shown) >= 3*time.Second {
				t.dirty, t.shown = false, time.Now()
				b.chat(th).status(th, id, &t)
			}
		}
	}()
}

// relay shows one record of session id in thread th.
func (b *bridge) relay(th, id string, t *turn, r statefile.Record) {
	if t.start.IsZero() && (r.Kind == statefile.Message || r.Kind == statefile.Text || r.Kind == statefile.Call) {
		t.start = r.Time
	}
	switch r.Kind {
	case statefile.Text:
		t.text = append(t.text, string(r.Body))
	case statefile.Step:
		if t.stepCalls > 0 {
			text := strings.Join(t.text, "\n\n")
			err := b.reply(th, text, "", false)
			t.deliveryErr = errors.Join(t.deliveryErr, err)
			if err == nil && hasAttachment(text) {
				t.attachmentExpected = false
				b.deliveryState(th, id, t)
			}
			t.text, t.stepCalls = nil, 0
		} else if provider.Normal(r.Header) {
			b.finishStep(th, id, t, r)
		}
	case statefile.Message:
		// A message the thread sent: the turn's reply answers it. One sent
		// from another thread of the same chat is quoted, as from elsewhere.
		b.mu.Lock()
		mine := r.Via == platform(th) && b.sentIn[r.IdempotencyKey] == th
		b.mu.Unlock()
		internal := bridgeMessage(r.IdempotencyKey)
		if strings.HasPrefix(r.IdempotencyKey, deliveryKey) {
			t.deliveryRecovery, t.attachmentExpected = true, true
			b.deliveryState(th, id, t)
		}
		if mine && r.IdempotencyKey != "" && !internal && r.IdempotencyKey != t.key {
			t.key = r.IdempotencyKey
			t.asked = append(t.asked, r.IdempotencyKey)
		}
		// The harness's own "Continue" from a retry is not a person's, and
		// a job's report is one line: its log tail floods the thread.
		switch {
		case r.Via == "job" && strings.HasPrefix(r.Type, "text/plain"):
			b.say(th, msg{Note: jobLine(string(r.Body))})
		case !mine && !internal && r.Via != "retry" && strings.HasPrefix(r.Type, "text/plain"):
			b.say(th, msg{Text: quote(r.Via, string(r.Body))})
		}
		// Every turn asked or retried, from anywhere, shows on the thread itself.
		if r.Via != "job" && strings.HasPrefix(r.Type, "text/plain") && !slices.Contains(t.asked, "") {
			t.asked = append(t.asked, "")
			b.chat(th).react(th, "", seen)
		}
	case statefile.Name:
		// The session's label, often set by its agent at the start: the thread
		// takes it.
		if b.shows(th, id) && len(r.Body) > 0 {
			b.rename(th, id, string(r.Body))
		}
	case statefile.Error:
		if !b.shows(th, id) {
			// An old request can fail after a model switch. Do not offer a
			// Retry for it, or change the new session's state or reactions.
			if t.status != "" {
				t.done, t.outcome, t.took = true, "continued", time.Since(t.start).Round(time.Second)
				b.chat(th).status(th, id, t)
			}
			*t = turn{}
			return
		}
		full := r.Via == "" && recent(r.Time, time.Now()) && b.shows(th, id) && b.endedFull(id, string(r.Body))
		text := "⚠️ " + string(r.Body)
		outcome := "failed"
		switch {
		case r.Via != "": // who stopped it
			outcome = "stopped"
		case full:
			// What the full session wrote goes out first, above the notice.
			if len(t.text) > 0 {
				b.reply(th, strings.Join(t.text, "\n\n"), t.key, time.Since(t.start) > notifyAfter)
				t.text = nil
			}
			// Carry reactions before starting the next follower: a fast new
			// turn can otherwise finish before these messages are carried.
			b.finish(th, id, t, "continued")
			b.say(th, msg{Text: text, Note: "the context is full: continuing in a new session"})
			b.advance(th, id, r.N)
			if b.continueFull(th, id) { // not a failure: the turn goes on in a new session
				return
			}
		}
		b.finish(th, id, t, outcome)
		switch {
		case full: // said above
		case r.Via != "": // a stop: on purpose, so no Retry to offer
			b.say(th, msg{Text: text})
		default:
			ms, err := b.modelList(id)
			if err != nil {
				log.Printf("models: %v", err)
			}
			b.say(th, msg{Text: text, Session: id, Retry: true, Models: ms})
		}
	case statefile.Call:
		t.calls++
		t.stepCalls++
		title, detail := callParts(r.Body)
		t.tasks = append(t.tasks, task{id: r.ID, title: title, detail: detail, status: "in_progress"})[max(0, len(t.tasks)+1-maxTasks):]
		if t.attaching == nil {
			t.attaching, t.sending = map[string]bool{}, map[string]bool{}
		}
		if attachmentCall(r.Body) {
			t.attaching[r.ID] = true
		}
		if slackActionCall(r.Body) {
			t.sending[r.ID] = true
		}
		b.deliveryState(th, id, t)
		t.dirty = true
	case statefile.Result:
		b.mu.Lock()
		request := b.st.Threads[th].Request
		b.mu.Unlock()
		receipt := confirmedSlackReceipt(r.Body, id, request)
		if receipt != nil {
			t.attachmentExpected = false
			t.actionReceipt = receipt.Permalink
			t.actionFailed = false
			b.deliveryState(th, id, t)
		}
		for i := range t.tasks {
			if t.tasks[i].id == r.For {
				t.tasks[i].status = "complete"
				if (r.Exit != nil && *r.Exit != 0) || (r.Exit == nil && bytes.HasPrefix(r.Body, []byte("error: "))) {
					t.tasks[i].status = "error"
				}
				t.dirty = true
			}
		}
		// Delivery facts follow the call itself, not the display list: a
		// call the list no longer shows still counts.
		if t.sending[r.For] && receipt == nil {
			t.actionFailed = true
		}
		if ok := (r.Exit != nil && *r.Exit == 0) || (r.Exit == nil && !bytes.HasPrefix(r.Body, []byte("error: "))); ok && t.attaching[r.For] && hasAttachment(string(r.Body)) {
			t.attachmentExpected = true
		}
		delete(t.sending, r.For)
		delete(t.attaching, r.For)
		b.deliveryState(th, id, t)
	}
}

// turn is what a follower shows of the agent's work since a person last
// spoke: one status message, edited as calls come, and then the reply.
type turn struct {
	start              time.Time
	key                string   // the chat message the reply answers, if any
	asked              []string // the chat messages of the turn, to mark done
	calls              int      // tool calls so far
	stepCalls          int      // calls of the step not yet closed
	tasks              []task   // the newest calls, at most maxTasks
	text               []string // text of the step not yet closed
	status             string   // the status message, once posted
	dirty              bool     // calls the status message does not show yet
	shown              time.Time
	attaching          map[string]bool // call ids whose run printed an attachment directive
	sending            map[string]bool // call ids that ran the Slack sender
	attachmentExpected bool            // a run call only printed the delivery directive
	deliveryRecovery   bool            // one corrective model step has already been requested
	deliveryErr        error           // any progress reply/attachment that failed to send
	actionReceipt      string          // confirmed executed Slack action, not an assistant assertion
	actionFailed       bool            // sender action returned no authoritative receipt

	// Set when the turn is done, for its last status.
	done    bool
	outcome string // "", "failed", "stopped" or "continued"
	took    time.Duration
	used    int // percent of the context used, or -1
}

// task is one call of a turn as its status message shows it: its tool and
// worker, its command or path, and its status: in_progress, complete or error.
type task struct {
	id, title, detail, status string
}

// maxTasks is the most calls a turn keeps, as many as a Slack plan block holds.
const maxTasks = 50

// finish ends the turn: the status message says how it went, and the
// waiting text goes out as a reply to the message that began the turn. A
// reply after a long turn notifies its author. outcome is "", "failed",
// "stopped" or "continued" (the session was full and goes on in a new one).
func (b *bridge) finish(th, id string, t *turn, outcome string) {
	text, key, asked := strings.Join(t.text, "\n\n"), t.key, t.asked
	if text != "" {
		if err := b.reply(th, text, key, time.Since(t.start) > notifyAfter); err != nil && outcome == "" {
			outcome = "failed"
		}
	}
	if t.deliveryErr != nil && outcome == "" {
		outcome = "failed"
	}
	took := time.Since(t.start).Round(time.Second)
	t.done, t.outcome, t.took, t.used = true, outcome, took, -1
	if t.status != "" && outcome == "" {
		if i, err := b.info(id); err == nil && i.Config.ContextLimit > 0 {
			t.used = i.ContextUsed * 100 / i.Config.ContextLimit
		}
	}
	b.chat(th).status(th, id, t)
	if outcome != "continued" {
		t.attachmentExpected, t.deliveryRecovery = false, false
		t.actionReceipt, t.actionFailed = "", false
		clear(t.attaching)
		clear(t.sending)
	}
	b.deliveryState(th, id, t)
	*t = turn{}
	b.mu.Lock()
	asked, b.carried[th] = marks(b.carried[th], asked, outcome)
	b.mu.Unlock()
	for _, m := range asked {
		b.chat(th).react(th, m, reaction(outcome))
	}
}

// marks splits a thread's messages at the end of a turn: those to mark now,
// and those carried to its next turn. A continued turn marks nothing and
// carries its messages on; any other end marks them with those carried before.
func marks(carried, asked []string, outcome string) (mark, carry []string) {
	all := append(slices.Clone(carried), asked...)
	if outcome == "continued" {
		return nil, all
	}
	return all, nil
}

// reaction is the mark a turn's messages get when it ends: done, failed for a
// failure or a stop, and none when it is continued in a new session, since
// that is not where it ends.
func reaction(outcome string) string {
	switch outcome {
	case "":
		return done
	case "continued":
		return ""
	default:
		return failed
	}
}

// notifyAfter is how long a turn runs before its reply notifies the author
// of the message it answers.
const notifyAfter = 2 * time.Minute

// reply posts the agent's text. A line "attach: /path" sends that file from
// this machine with it, in place of the line.
func (b *bridge) reply(th, text, replyTo string, notify bool) error {
	kept, files, notes, errs := splitAttachments(text, platform(th) == "slack")
	text = strings.TrimSpace(strings.Join(kept, "\n"))
	if text == "" && len(notes) == 0 && len(files) == 0 {
		return nil
	}
	return errors.Join(errs, b.say(th, msg{Text: text, Note: strings.Join(notes, "\n"), ReplyTo: replyTo, Notify: notify, Files: files}))
}

// The bot's reaction on a message the session was sent: seen while the
// agent works on it, then done or failed.
const (
	seen   = "👀"
	done   = "✅"
	failed = "❌"
)

type title struct{ id, name string }

// rename gives thread th session id's name, in a goroutine of its own: a
// platform's rename limit (Discord's is about two per ten minutes) then holds
// back the rename only, and a rename that waits takes the newest name.
func (b *bridge) rename(th, id, name string) {
	b.mu.Lock()
	_, running := b.renames[th]
	b.renames[th] = &title{id, name}
	b.mu.Unlock()
	if running {
		return
	}
	go func() {
		for {
			b.mu.Lock()
			t := b.renames[th]
			if t == nil {
				delete(b.renames, th)
				b.mu.Unlock()
				return
			}
			b.renames[th] = nil
			b.mu.Unlock()
			b.chat(th).rename(th, t.id, t.name)
		}
	}()
}

// calls is "1 call" or "n calls".
func calls(n int) string {
	if n == 1 {
		return "1 call"
	}
	return fmt.Sprintf("%d calls", n)
}

// callParts says what tool runs where, e.g. "run on admin", and on what: the
// first line of its command, code or path.
func callParts(body []byte) (title, detail string) {
	var c struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if json.Unmarshal(body, &c) != nil {
		return "working…", ""
	}
	title = c.Name
	if w, ok := c.Arguments["worker"].(string); ok {
		title += " on " + w
	}
	for _, k := range []string{"command", "code", "path"} {
		if v, ok := c.Arguments[k].(string); ok {
			first, _, _ := strings.Cut(v, "\n")
			return title, firstWords(first, 200)
		}
	}
	return title, ""
}
