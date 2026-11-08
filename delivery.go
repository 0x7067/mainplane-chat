package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// maxAttach is the largest file an agent's "attach: /path" sends: Discord's
// upload limit, which is below Slack's.
const maxAttach = 20 << 20

// Correct a model that tried to upload by printing the attachment protocol
// from run. This is model feedback, NEVER permission to upload tool stdout.
const deliveryKey = "attachment-help-"

const deliveryCorrection = "Delivery correction from the chat bridge: the tool only printed an attachment instruction. No file was uploaded. Execute mainplane-chat slack post --file /absolute/path via run on the bridge worker for the user's requested TOP-LEVEL CHANNEL POST, or mainplane-chat slack reply --file /absolute/path if they requested a thread reply. MAINPLANE_SESSION selects this conversation. Read the returned JSON receipt; only say sent if ok:true and give its permalink. Do not echo commands, print attach: lines, or send input to Mainplane sessions."

// deliveryCorrectionAttach is the same correction where the chat takes the
// final-text attach: line, with no executed sender to run.
const deliveryCorrectionAttach = "Delivery correction from the chat bridge: the tool only printed an attachment instruction. No file was uploaded. Deliver the file by putting a standalone line attach: /absolute/path in your final reply text, outside code blocks; the bridge reads that path on its machine and uploads the file in place of the line. Do not print attach: inside tools, echo commands, or claim a file was sent without that line."

// deliveryCorrectionFor picks the correction for the thread's chat.
func deliveryCorrectionFor(th string) string {
	if platform(th) == "slack" {
		return deliveryCorrection
	}
	return deliveryCorrectionAttach
}

func bridgeMessage(key string) bool {
	return strings.HasPrefix(key, fullKey) || strings.HasPrefix(key, deliveryKey)
}

// attachment is the directive a line is, if any: the file to send and whether
// it goes to the channel's top level rather than the thread. A directive
// inside a code fence is an example, not a request.
func attachment(line string) (path string, topLevel bool, ok bool) {
	for _, prefix := range []string{"attach: ", "attach-channel: "} {
		if p, is := strings.CutPrefix(line, prefix); is {
			p = strings.TrimSpace(p)
			return p, prefix == "attach-channel: ", p != ""
		}
	}
	return "", false, false
}

// scanText walks text's lines with fn, telling it whether each line is
// inside a code fence, where an attach: directive is an example, not a
// request. Fence marker lines are inside neither.
func scanText(text string, fn func(line string, inFence bool)) {
	fence := ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if fence == "" {
				fence = t[:3]
			} else if fence == t[:3] {
				fence = ""
			}
			fn(l, false)
			continue
		}
		fn(l, fence != "")
	}
}

func hasAttachment(text string) bool {
	found := false
	scanText(text, func(line string, inFence bool) {
		if !inFence {
			if _, _, ok := attachment(strings.TrimSpace(line)); ok {
				found = true
			}
		}
	})
	return found
}

// splitAttachments divides the agent's text at its attach: directives,
// outside code fences: the lines to keep, the files the directives name
// (read from this machine, at most maxAttach), notes for the ones that
// failed, and their error. attach-channel: is Slack's; with slack false it
// stays an ordinary line.
func splitAttachments(text string, slack bool) (kept []string, files []file, notes []string, errs error) {
	var es []error
	scanText(text, func(l string, inFence bool) {
		if inFence {
			kept = append(kept, l)
			return
		}
		path, topLevel, ok := attachment(strings.TrimSpace(l))
		if ok && topLevel && !slack {
			ok = false
		}
		if !ok {
			kept = append(kept, l)
			return
		}
		data, err := os.ReadFile(path)
		if err == nil && len(data) > maxAttach {
			err = fmt.Errorf("%s is over %d MiB", path, maxAttach>>20)
		}
		if err != nil {
			notes = append(notes, "could not attach: "+err.Error())
			es = append(es, err)
			return
		}
		files = append(files, file{name: filepath.Base(path), size: len(data), fetch: func() ([]byte, error) { return data, nil }, topLevel: topLevel})
	})
	return kept, files, notes, errors.Join(es...)
}

func attachmentCall(body []byte) bool {
	var call struct {
		Name      string
		Arguments map[string]any
	}
	if json.Unmarshal(body, &call) != nil || call.Name != "run" {
		return false
	}
	for _, key := range []string{"code", "command"} {
		if code, ok := call.Arguments[key].(string); ok && (strings.Contains(code, "attach: ") || strings.Contains(code, "attach-channel: ")) {
			return true
		}
	}
	return false
}

// A shell echo is not a delivery. Suppress the unsupported final claim,
// carry the user's pending reaction, and allow ONE corrective model turn.
func (b *bridge) finishStep(th, id string, t *turn, r statefile.Record) {
	if t.actionFailed {
		t.text = nil
		b.finish(th, id, t, "failed")
		b.say(th, msg{Text: "⚠️ Slack delivery was not confirmed. The sender did not return a valid receipt; inspect its error or pending receipt before retrying.", Session: id, Retry: true})
		return
	}
	if t.actionReceipt != "" && hasAttachment(strings.Join(t.text, "\n\n")) {
		// The executed action already sent the file. Do not upload it again
		// just because the model also prints the old text protocol.
		t.text = []string{"Posted: " + t.actionReceipt}
	}
	if !t.attachmentExpected || hasAttachment(strings.Join(t.text, "\n\n")) {
		b.finish(th, id, t, "")
		return
	}
	t.text = nil
	if t.deliveryRecovery || !recent(r.Time, time.Now()) || !b.shows(th, id) {
		b.finish(th, id, t, "failed")
		b.say(th, msg{Text: "⚠️ No file was uploaded: the agent did not produce an assistant attachment. Automatic correction has stopped.", Session: id, Retry: true})
		return
	}
	t.deliveryRecovery = true
	b.finish(th, id, t, "continued")
	b.say(th, msg{Note: "No file was uploaded: the agent only printed an attachment line in a tool. Correcting delivery."})
	if err := b.post(th, id, deliveryKey+id+"-"+r.ID, "", deliveryCorrectionFor(th), nil); err != nil {
		b.finish(th, id, t, "failed")
	}
}

func slackActionCall(body []byte) bool {
	var call struct {
		Name      string
		Arguments map[string]any
	}
	if json.Unmarshal(body, &call) != nil || call.Name != "run" {
		return false
	}
	for _, key := range []string{"code", "command"} {
		if code, ok := call.Arguments[key].(string); ok && !strings.Contains(code, "--help") && !strings.Contains(code, " -h") && (strings.Contains(code, "mainplane-chat slack post") || strings.Contains(code, "mainplane-chat slack reply")) {
			return true
		}
	}
	return false
}

// Persist the turn's delivery facts, so restarting between a tool call and
// its result, or between a result and the final response, can neither turn
// an unsent attachment into a successful turn nor lose a sender receipt.
func (b *bridge) deliveryState(th, id string, t *turn) {
	attaching, sending := ids(t.attaching), ids(t.sending)
	b.mu.Lock()
	defer b.mu.Unlock()
	current, ok := b.st.Threads[th]
	if !ok || current.Session != id ||
		current.AttachmentPending == t.attachmentExpected && current.AttachmentRecovery == t.deliveryRecovery &&
			current.ActionReceipt == t.actionReceipt && current.ActionFailed == t.actionFailed &&
			slices.Equal(current.Attaching, attaching) && slices.Equal(current.Sending, sending) {
		return
	}
	current.AttachmentPending, current.AttachmentRecovery = t.attachmentExpected, t.deliveryRecovery
	current.Attaching, current.Sending = attaching, sending
	current.ActionReceipt, current.ActionFailed = t.actionReceipt, t.actionFailed
	b.st.Threads[th] = current
	b.save()
}

// ids is the sorted ids a set holds, nil when it holds none.
func ids(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// setOf is the set of ids.
func setOf(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}
