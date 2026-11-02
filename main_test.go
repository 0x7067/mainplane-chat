package main

import (
	"strings"
	"testing"
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

func TestContinued(t *testing.T) {
	for msg, want := range map[string]string{
		"You are continuing session 5b733adb. Retrieve key context from it as you go.":           "5b733adb",
		"You are continuing session 5b733adb. Retrieve key context from it as you go.\nnow do X": "5b733adb",
		"You are continuing session nope. x":                                                     "",
		"please continue session 5b733adb":                                                       "",
		"":                                                                                       "",
	} {
		if got := continued(msg); got != want {
			t.Errorf("continued(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestContextFull(t *testing.T) {
	for body, want := range map[string]bool{
		"context 250009 exceeds limit 250000":                      true,
		"context 8300 exceeds limit 8200":                          true,
		"The session was stopped during the last step by discord.": false,
		"anthropic: 529 overloaded":                                false,
	} {
		if got := contextFull(body); got != want {
			t.Errorf("contextFull(%q) = %v", body, got)
		}
	}
}

func TestLastUnanswered(t *testing.T) {
	rec := func(kind statefile.Kind, via, body string) statefile.Record {
		return statefile.Record{Header: statefile.Header{Kind: kind, Type: "text/plain", Via: via}, Body: []byte(body)}
	}
	for name, c := range map[string]struct {
		recs []statefile.Record
		want string
	}{
		"answered":          {[]statefile.Record{rec(statefile.Message, "discord", "hi"), rec(statefile.Text, "", "hello")}, ""},
		"unanswered":        {[]statefile.Record{rec(statefile.Text, "", "hello"), rec(statefile.Message, "discord", "now do X")}, "now do X"},
		"newest wins":       {[]statefile.Record{rec(statefile.Message, "discord", "a"), rec(statefile.Message, "cli", "b")}, "b"},
		"skip retry":        {[]statefile.Record{rec(statefile.Message, "discord", "do X"), rec(statefile.Message, "retry", "Continue")}, "do X"},
		"skip job":          {[]statefile.Record{rec(statefile.Message, "job", "job done")}, ""},
		"skip continuation": {[]statefile.Record{rec(statefile.Message, "discord", continuePrefix+"abcd1234. Retrieve…")}, ""},
	} {
		if got := lastUnanswered(c.recs); got != c.want {
			t.Errorf("%s: lastUnanswered = %q, want %q", name, got, c.want)
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
