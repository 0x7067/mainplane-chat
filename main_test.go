package main

import (
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/catalog"
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
