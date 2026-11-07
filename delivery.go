package main

import (
	"encoding/json"
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

func hasAttachment(text string) bool {
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			if fence == "" {
				fence = line[:3]
			} else if fence == line[:3] {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if _, _, ok := attachment(line); ok {
			return true
		}
	}
	return false
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
	if err := b.post(th, id, deliveryKey+id+"-"+r.ID, "", deliveryCorrection, nil); err != nil {
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

// Persist only delivery facts, so restarting between a tool result and the
// final response cannot turn an unsent attachment into a successful turn.
func (b *bridge) deliveryState(th, id string, t *turn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current, ok := b.st.Threads[th]
	if !ok || current.Session != id || current.AttachmentPending == t.attachmentExpected && current.AttachmentRecovery == t.deliveryRecovery {
		return
	}
	current.AttachmentPending, current.AttachmentRecovery = t.attachmentExpected, t.deliveryRecovery
	b.st.Threads[th] = current
	b.save()
}
