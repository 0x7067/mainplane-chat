package main

import (
	"cmp"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mainplane-ai/mainplane/pkg/provider"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// Keep a continuation small even after a very large thinking block or tool
// result. Reserve space for the actual requests separately from recent work.
const maxHandoff = 16 << 10

const autoContinueText = "The session before ran out of context. Carry on where it stopped."

const logFormatNote = "The log is NOT JSONL: each record is a JSON header line, exactly header.len bytes of body, then a newline. Do not grep it as one JSON object per line or read the whole log."

const sessionContextPrefix = "(Session context from the chat bridge:"

// The headings of a handoff, written where the message is built and read
// back where the next continuation parses it: one wording, one place.
const (
	requestsHeading    = "Current requests:\n"
	progressHeading    = "\n\nRecent progress"
	instructionHeading = "\n\nContinuation instruction:\n"
)

// The bridge owns this mapping. Do not make the model infer its own identity
// by guessing from whichever session mainplane sessions happens to list first.
func sessionContext(th, id string) string {
	note := sessionContextPrefix + " your current Mainplane session is " + id + "."
	if platform(th) == "slack" {
		channel, ts := threadParts(th)
		note += " This Slack conversation is channel " + channel + ", thread " + ts + "; the bridge routes your replies there."
		note += " " + slackDeliveryNote
	}
	return note + " Other Mainplane sessions are separate conversations, not Slack destinations. Use mainplane sessions to discover them and mainplane info <id> to inspect current status; do not guess your current id from the newest session. Sending mainplane message <id> adds model input to that session, not a chat reply. The bridge handles chat delivery and continuations.)"
}

// Do not spin up an unbounded chain when even a fresh handoff does not fit,
// or the model spends its entire new window thinking without doing anything.
func stalledContinuation(recs []statefile.Record) bool {
	auto := false
	for _, r := range recs {
		if r.Kind == statefile.Message && strings.HasPrefix(r.IdempotencyKey, fullKey) {
			auto = true
		}
		if auto && (r.Kind == statefile.Result || r.Kind == statefile.Step && provider.Normal(r.Header)) {
			return false
		}
	}
	return auto
}

// continuationContext carries facts already in the records, not an invented
// summary. Thinking and binary input never enter it. Assistant claims and tool
// results stay attributed so the next model can distinguish a plan from work.
func continuationContext(recs []statefile.Record) string {
	var requests, recent []string
	var framing, inherited string
	for _, r := range recs {
		if r.Kind != statefile.Message || !strings.HasPrefix(r.Type, "text/plain") || r.Via == "retry" || r.Via == "job" {
			continue
		}
		body := string(r.Body)
		if strings.HasPrefix(body, sessionContextPrefix) {
			_, body, _ = strings.Cut(body, "\n\n")
		}
		if strings.HasPrefix(body, continuePrefix) {
			// Carry the requests, not another copy of an entire handoff.
			if _, rest, ok := strings.Cut(body, requestsHeading); ok {
				inherited, _, _ = strings.Cut(rest, progressHeading)
			}
			if _, instruction, ok := strings.Cut(body, instructionHeading); ok && instruction != autoContinueText {
				requests = append(requests, handoffExcerpt(instruction, 2000))
			}
			continue
		}
		if strings.HasPrefix(body, slackNotePrefix) {
			if note, rest, ok := strings.Cut(body, "\n\n"); ok {
				framing, body = note, rest
			}
		}
		requests = append(requests, handoffExcerpt(body, 2000))
	}
	if inherited != "" {
		requests = append([]string{handoffExcerpt(inherited, 3000)}, requests...)
	}
	// Newest requests win the budget; keep their original order and authors.
	var kept []string
	budget := 6000
	for i := len(requests) - 1; i >= 0 && budget > 0; i-- {
		s := handoffExcerpt(requests[i], min(2000, budget))
		kept = append(kept, s)
		budget -= len(s) + 2
	}
	slices.Reverse(kept)
	// A call and its result retain their ids, worker, exit and truncation
	// metadata. Head AND tail keep artifact paths after a long script/output.
	budget = 8000
	for i := len(recs) - 1; i >= 0 && budget > 0; i-- {
		r := recs[i]
		switch r.Kind {
		case statefile.Text, statefile.Call, statefile.Error:
		case statefile.Result:
			if r.Type != "" && !strings.HasPrefix(r.Type, "text/") && r.Type != "application/json" {
				continue
			}
		default:
			continue
		}
		label := fmt.Sprintf("[%s #%d id=%s", r.Kind, r.N, r.ID)
		if r.For != "" {
			label += " for=" + r.For
		}
		if r.Exit != nil {
			label += fmt.Sprintf(" exit=%d", *r.Exit)
		}
		if r.Truncated {
			label += " truncated"
		}
		s := label + "]\n" + handoffExcerpt(string(r.Body), 1000)
		if len(s)+2 > budget {
			break
		}
		recent = append(recent, s)
		budget -= len(s) + 2
	}
	slices.Reverse(recent)
	out := requestsHeading + strings.Join(kept, "\n\n") + progressHeading + " (excerpts, not proof that an assistant's claims succeeded):\n" + strings.Join(recent, "\n\n")
	if framing != "" {
		out = handoffExcerpt(framing, 1500) + "\n\n" + out
	}
	return handoffExcerpt(out, maxHandoff)
}

func handoffExcerpt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const gap = "\n[… excerpt …]\n"
	if limit <= len(gap) {
		return s[:runeStart(s, limit)]
	}
	half := (limit - len(gap)) / 2
	start := len(s) - half
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:runeStart(s, half)] + gap + s[start:]
}

func contextFull(body string) bool {
	return strings.HasPrefix(body, "context ") && strings.Contains(body, " exceeds limit ")
}

func providerFull(body string) bool {
	return strings.Contains(body, "exceed_context_size_error") || strings.Contains(body, "context_length_exceeded")
}

var (
	llamaPrompt = regexp.MustCompile(`"n_prompt_tokens"\s*:\s*(\d+)`)
	llamaCtx    = regexp.MustCompile(`"n_ctx"\s*:\s*(\d+)`)
	openaiCtx   = regexp.MustCompile(`maximum context length is (\d+) tokens`)
	openaiReq   = regexp.MustCompile(`(?:resulted in|you requested) (\d+) tokens`)
)

func overflowSizes(body string) (request, window int) {
	num := func(re *regexp.Regexp) int {
		if m := re.FindStringSubmatch(body); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	return cmp.Or(num(llamaPrompt), num(openaiReq)), cmp.Or(num(llamaCtx), num(openaiCtx))
}

func historyOverflow(body string, used int) bool {
	request, window := overflowSizes(body)
	return request > 0 && window > 0 && (request-used)*2 < window
}

func recent(at, now time.Time) bool {
	return now.Sub(at) < 10*time.Minute
}

func (b *bridge) endedFull(id, body string) bool {
	if contextFull(body) {
		return true
	}
	lengthStop := strings.HasPrefix(body, `The last step ended with stop "length"`) || strings.HasPrefix(body, `The last step ended with stop "max_tokens"`)
	if !providerFull(body) && !lengthStop {
		return false
	}
	i, err := b.info(id)
	if err != nil {
		return false
	}
	// ContextUsed counts only the prompt. A long thinking/reply block from
	// that step is now history too, even when no later step succeeded.
	recs, err := b.records(id, 0, "")
	if err != nil {
		return false
	}
	used := i.ContextUsed
	if step := statefile.LastStep(recs); step != nil && step.Usage != nil {
		used = step.Usage.Prompt() + step.Usage.Output
	}
	if lengthStop {
		return i.Config.ContextLimit > 0 && used >= i.Config.ContextLimit
	}
	return historyOverflow(body, used)
}

const fullKey = "context-full-"

func (b *bridge) continueFull(th, id string) bool {
	b.mu.Lock()
	taken := b.full[id]
	b.full[id] = true
	b.mu.Unlock()
	if taken {
		return true
	}
	text := autoContinueText
	if recs, err := b.records(id, 0, ""); err == nil {
		if stalledContinuation(recs) {
			b.mu.Lock()
			delete(b.full, id)
			b.mu.Unlock()
			b.say(th, msg{Text: "Automatic continuation stopped: the new session filled before making progress. Use !model to choose a larger context, or !continue with a shorter task."})
			return false
		}
	}
	if _, err := b.continueIn(th, id, fullKey+id, "", text); err != nil {
		b.mu.Lock()
		delete(b.full, id) // a transient failure must not poison later recovery
		b.mu.Unlock()
		b.say(th, msg{Text: "automatic /continue failed: " + err.Error()})
		return false
	}
	return true
}

const continuePrefix = "You are continuing session "

func continued(msg string) string {
	rest, ok := strings.CutPrefix(msg, continuePrefix)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, ".")
	if statefile.ValidID(id) {
		return id
	}
	return ""
}

func (b *bridge) continueIn(th, old, msgID, model, text string) (string, error) {
	cur, err := b.info(old)
	if err != nil {
		return "", err
	}
	conf := cur.Config
	if model != "" {
		ch, _, err := b.choices()
		if err != nil {
			return "", err
		}
		if conf, err = ch.switchConf(cur.Config, model); err != nil {
			return "", err
		}
	}
	recs, err := b.records(old, 0, "")
	if err != nil {
		return "", fmt.Errorf("read continuation context: %w", err)
	}
	// Upgrade old pointer-only continuations too. Read their predecessor's
	// snapshot, not messages sent to it after this conversation had moved on.
	contextRecords, ancestor := recs, recs
	visited := map[string]bool{old: true}
	for range 3 {
		var first *statefile.Record
		for i := range ancestor {
			if ancestor[i].Kind == statefile.Message {
				first = &ancestor[i]
				break
			}
		}
		if first == nil || strings.Contains(string(first.Body), requestsHeading) {
			break
		}
		previous := continued(string(first.Body))
		if previous == "" || visited[previous] {
			break
		}
		visited[previous] = true
		previousRecords, readErr := b.records(previous, 0, "")
		if readErr != nil {
			log.Printf("older continuation %s: %v", previous, readErr)
			break
		}
		ancestor = slices.DeleteFunc(previousRecords, func(r statefile.Record) bool { return r.Time.After(first.Time) })
		contextRecords = append(slices.Clone(ancestor), contextRecords...)
	}
	m := continuePrefix + old + ". Continue the task using the handoff below; retrieve older context only if a specific fact is missing."
	m += "\n\n" + continuationContext(contextRecords)
	if paths := b.logPaths(old, cur.Config); len(paths) > 0 {
		m += "\nOlder records (only if needed): " + strings.Join(paths, ", ") + ". " + logFormatNote
	}
	if text != "" {
		m += instructionHeading + text
	}
	id, err := b.create(conf, platform(th))
	if err != nil {
		return "", err
	}
	b.say(th, msg{Note: "continuing in session " + id + " on " + conf.Model})
	others := b.threadsShowing(old)
	if err := b.post(th, id, msgID, "", m, nil); err != nil {
		// A lost response is not proof that the send failed. Delete only a
		// session confirmed empty; never destroy potentially accepted work.
		if pending, readErr := b.records(id, 0, ""); readErr == nil && !slices.ContainsFunc(pending, func(r statefile.Record) bool { return r.Kind == statefile.Message }) {
			if cleanup := b.call("DELETE", "/sessions/"+id, "", nil, nil); cleanup != nil {
				log.Printf("remove empty continuation %s: %v", id, cleanup)
			}
		} else {
			b.say(th, msg{Note: "Could not confirm delivery to session " + id + "; preserved it for inspection. The thread still follows " + old + "."})
		}
		return "", err
	}
	for _, o := range others {
		if o == th {
			continue
		}
		b.follow(o, id, 0)
		b.say(o, msg{Note: "continued in session " + id})
	}
	return id, nil
}
