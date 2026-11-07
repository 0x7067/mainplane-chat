package main

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mainplane-ai/mainplane/pkg/catalog"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"2", "", true}, {"10", "9", true}, {"9", "10", false}, {"5", "5", false}, {"123", "122", true}} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestPickModel(t *testing.T) {
	cat := catalog.Catalog{Presets: map[string]catalog.Preset{
		"chatgpt/gpt-6.1-sol":         {Name: "GPT-6.1 Sol", Provider: "chatgpt", Model: "gpt-6.1-sol"},
		"claude-code/claude-opus-5-5": {Name: "Claude Opus 5.5", Provider: "claude-code", Model: "claude-opus-5-5"},
	}}
	ids := []string{"claude-code/claude-opus-5-5", "chatgpt/gpt-6.1-sol"}
	for s, want := range map[string]string{
		"chatgpt/gpt-6.1-sol": "chatgpt/gpt-6.1-sol", "GPT-6.1-SOL": "chatgpt/gpt-6.1-sol",
		"claude opus 5.5": "claude-code/claude-opus-5-5", "nope": "",
	} {
		if got := pickModel(cat, ids, s); got != want {
			t.Errorf("pickModel(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestQuote(t *testing.T) {
	for _, c := range []struct{ via, body, want string }{
		{"cli", "fix the build", "> **cli:** fix the build"},
		{"tui", "line one\nline two\n", "> **tui:** line one\n> line two"},
	} {
		if got := quote(c.via, c.body); got != c.want {
			t.Errorf("quote(%q, %q) = %q, want %q", c.via, c.body, got, c.want)
		}
	}
}

func TestGone(t *testing.T) {
	for _, c := range []struct {
		err  refusedError
		want bool
	}{
		{refusedError{"400 Bad Request: open /var/lib/mainplane-server/sessions/x.state: no such file or directory"}, true},
		{refusedError{"400 Bad Request: session not found"}, false},
		{refusedError{"401 Unauthorized"}, false},
	} {
		if got := c.err.gone(); got != c.want {
			t.Errorf("gone(%q) = %v", c.err.msg, got)
		}
	}
}

func TestThreadTitle(t *testing.T) {
	long := strings.Repeat("word ", 40)
	for _, c := range []struct{ id, label, want string }{
		{"5b733adb", "Fix Discord rename limit", "5b733adb Fix Discord rename limit"},
		{"5b733adb", "  two\nlines ", "5b733adb two lines"},
	} {
		if got := threadTitle(c.id, c.label); got != c.want {
			t.Errorf("threadTitle(%q) = %q, want %q", c.label, got, c.want)
		}
	}
	if got := threadTitle("5b733adb", long); len([]rune(got)) > 100 || !strings.HasSuffix(got, "…") {
		t.Errorf("threadTitle(long) = %q (%d runes), want at most 100 ending in …", got, len([]rune(got)))
	}
}

func TestJobLine(t *testing.T) {
	got := jobLine("[job toolu_01X on admin exited 0 after 40s. Log: /home/u/.mainplane/jobs/toolu_01X.log]\nlots\nof log")
	if want := "job on admin exited 0 after 40s"; got != want {
		t.Errorf("jobLine = %q, want %q", got, want)
	}
}

func TestRunes(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	if got := firstWords(s, 5); got != "éé…" {
		t.Errorf("firstWords = %q", got)
	}
	for _, p := range split(s, 5) {
		if !utf8.ValidString(p) || len(p) > 5 {
			t.Errorf("split piece %q", p)
		}
	}
	if got := strings.Join(split(s, 5), ""); got != s {
		t.Errorf("split lost text: %q", got)
	}
}

func TestPartsPrefixOnlyWithContent(t *testing.T) {
	b := &bridge{} // no harness: a session lookup fails, so no file type is accepted as an image
	zip := file{name: "a.zip", ctype: "application/zip", size: 10}
	if ps, skipped := b.parts("s1", "[pedro <@U09>]: ", "", []file{zip}); len(ps) != 0 || len(skipped) != 1 {
		t.Errorf("file-only message with every file skipped: parts %d, skipped %v; want none sent", len(ps), skipped)
	}
	if ps, _ := b.parts("s1", "[pedro <@U09>]: ", "", nil); len(ps) != 0 {
		t.Errorf("empty message sent %d parts", len(ps))
	}
	ps, _ := b.parts("s1", "[pedro <@U09>]: ", "hi", nil)
	if len(ps) != 1 || string(ps[0].body) != "[pedro <@U09>]: hi" {
		t.Errorf("text message = %v, want one part with the prefix", ps)
	}
}

func TestReaction(t *testing.T) {
	for outcome, want := range map[string]string{"": done, "failed": failed, "stopped": failed, "continued": ""} {
		if got := reaction(outcome); got != want {
			t.Errorf("reaction(%q) = %q, want %q", outcome, got, want)
		}
	}
}

func TestMarks(t *testing.T) {
	mark, carry := marks(nil, []string{"m1"}, "continued")
	if len(mark) != 0 || !slices.Equal(carry, []string{"m1"}) {
		t.Errorf("continued: mark %v carry %v, want none and [m1]", mark, carry)
	}
	mark, carry = marks(carry, []string{"m2"}, "")
	if !slices.Equal(mark, []string{"m1", "m2"}) || carry != nil {
		t.Errorf("done after a continuation: mark %v carry %v, want [m1 m2] and none", mark, carry)
	}
	if mark, _ := marks([]string{"m1"}, nil, "failed"); !slices.Equal(mark, []string{"m1"}) {
		t.Errorf("failed after a continuation: mark %v, want [m1]", mark)
	}
}

func TestConsoleStatus(t *testing.T) {
	working := &turn{calls: 1, tasks: []task{{title: "run admin", status: "in_progress"}}}
	if got := consoleStatus("s1", working); got != "status: Working · 1 call\n  in_progress run admin" {
		t.Errorf("working = %q", got)
	}
	done := &turn{calls: 1, done: true, took: 19 * time.Second, used: -1, tasks: []task{{title: "run admin", status: "complete"}}}
	if got := consoleStatus("s1", done); got != "status: Done · 1 call · 19s · s1\n  complete run admin" {
		t.Errorf("done = %q", got)
	}
	var out strings.Builder
	c := &console{out: &out}
	c.status(consoleThread, "s1", &turn{done: true, took: time.Second, used: -1})
	if out.Len() != 0 {
		t.Errorf("a turn with no calls printed %q; Slack posts no status for it", out.String())
	}
}

func TestLogLines(t *testing.T) {
	got := logLines("a35d9dea", []statefile.Worker{{Name: "admin"}, {Name: "gone"}, {Name: "inventory-lj69443nvl"}},
		map[string]string{"admin": "/Users/pedrostestm4/.mainplane", "inventory-lj69443nvl": "/Users/pedrostest/.mainplane"})
	want := []string{"/Users/pedrostestm4/.mainplane/logs/a35d9dea.log on admin", "/Users/pedrostest/.mainplane/logs/a35d9dea.log on inventory-lj69443nvl"}
	if !slices.Equal(got, want) {
		t.Errorf("logLines = %v, want %v", got, want)
	}
	if continued(continuePrefix+"a35d9dea. Retrieve key context from it as you go. Its log: "+want[0]+".") != "a35d9dea" {
		t.Error("continued no longer reads the session id from a message that names its log")
	}
}
