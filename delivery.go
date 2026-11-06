package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// Correct a model that tried to upload by printing the attachment protocol
// from run. This is model feedback, NEVER permission to upload tool stdout.
const deliveryKey = "attachment-help-"

const deliveryCorrection = "Delivery correction from the chat bridge: your run call only printed an attachment instruction. No file was uploaded. Do not repeat the shell command or claim it was sent. Put the actual file's standalone attach: /absolute/path line directly in your assistant reply, with no code fence. If the user requested a top-level Slack message, use attach-channel: /absolute/path instead. The bridge performs and checks the upload. Reply with the directive now; do not use tools."

func bridgeMessage(key string) bool {
	return strings.HasPrefix(key, fullKey) || strings.HasPrefix(key, deliveryKey)
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
		for _, prefix := range []string{"attach: ", "attach-channel: "} {
			if path, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(path) != "" {
				return true
			}
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
	if !t.attachmentExpected || hasAttachment(strings.Join(t.text, "\n\n")) {
		b.finish(th, id, t, "")
		return
	}
	t.text = nil
	if t.deliveryRecovery || !fresh(r.Time, time.Now()) || !b.shows(th, id) {
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
