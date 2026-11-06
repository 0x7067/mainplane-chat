package main

import (
	"fmt"
	"slices"
	"strings"
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
			if _, rest, ok := strings.Cut(body, "Current requests:\n"); ok {
				inherited, _, _ = strings.Cut(rest, "\n\nRecent progress")
			}
			if _, instruction, ok := strings.Cut(body, "\n\nContinuation instruction:\n"); ok && instruction != autoContinueText {
				requests = append(requests, handoffExcerpt(instruction, 2000))
			}
			continue
		}
		if strings.HasPrefix(body, "(Note from the Slack bridge:") {
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
	out := "Current requests:\n" + strings.Join(kept, "\n\n") + "\n\nRecent progress (excerpts, not proof that an assistant's claims succeeded):\n" + strings.Join(recent, "\n\n")
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
