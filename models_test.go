package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/catalog"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// testCatalog has one connected preset (chatgpt), one whose provider the
// tests leave unconnected (anthropic), and chatgpt as the default.
var testCatalog = catalog.Catalog{
	Presets: map[string]catalog.Preset{
		"chatgpt/gpt-6.1-sol":         {Name: "GPT-6.1 Sol", Provider: "chatgpt", Model: "gpt-6.1-sol", Tools: "default", ContextLimit: 272000},
		"anthropic/claude-opus-5-5":   {Name: "Claude Opus 5.5", Provider: "anthropic", Model: "claude-opus-5-5", Tools: "default", ContextLimit: 1000000},
		"claude-code/claude-opus-5-5": {Name: "Claude Opus 5.5", Provider: "claude-code", Model: "claude-opus-5-5", Tools: "default", ContextLimit: 1000000},
	},
	Groups: map[string][]string{
		group:    {"anthropic/claude-opus-5-5", "claude-code/claude-opus-5-5", "chatgpt/gpt-6.1-sol"},
		defaults: {"chatgpt/gpt-6.1-sol"},
	},
}

var local = statefile.Conf{
	Model: "m4/qwen3.6-35b-a3b", Tools: "default", ContextLimit: 65536,
	Input: []string{"text"}, Params: map[string]json.RawMessage{"temperature": json.RawMessage("0.6")},
	Workers: []statefile.Worker{{Name: "old"}},
}

func TestStartConf(t *testing.T) {
	sol := presetConf("chatgpt/gpt-6.1-sol", testCatalog.Presets["chatgpt/gpt-6.1-sol"])
	localCopy := local
	localCopy.Workers = nil
	for name, c := range map[string]struct {
		providers []string
		base      *statefile.Conf
		want      statefile.Conf
		err       bool
	}{
		"local model is copied, but its workers":     {[]string{"chatgpt", "m4"}, &local, localCopy, false},
		"catalog model takes its preset":             {[]string{"chatgpt", "m4"}, &sol, sol, false},
		"model's provider gone: catalog default":     {[]string{"chatgpt"}, &local, sol, false},
		"nothing to copy: catalog default":           {[]string{"chatgpt", "m4"}, nil, sol, false},
		"no catalog provider, nothing to copy":       {[]string{"m4"}, nil, statefile.Conf{}, true},
		"no catalog provider, local model":           {[]string{"m4"}, &local, localCopy, false},
		"no catalog provider, model's provider gone": {[]string{"lj"}, &local, statefile.Conf{}, true},
	} {
		got, err := newChoices(testCatalog, c.providers, nil).startConf(c.base)
		if (err != nil) != c.err || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: startConf = %+v, %v; want %+v, error %v", name, got, err, c.want, c.err)
		}
	}
}

func TestStartBase(t *testing.T) {
	test := statefile.Conf{Model: "k3q1/lfm3-1.7b"}
	chat := statefile.Conf{Model: "m4/qwen3.6-35b-a3b"}
	recent := []info{{ID: "t1", Config: test}, {ID: "c1", Config: chat}, {ID: "c0", Config: test}}
	for name, c := range map[string]struct {
		shown []string
		want  *statefile.Conf
	}{
		"newest session a thread shows, over newer ones": {[]string{"c0", "c1"}, &chat},
		"no thread's session is recent: the newest":      {[]string{"old"}, &test},
		"no threads: the newest":                         {nil, &test},
	} {
		if got := startBase(recent, c.shown); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: startBase = %+v, want %+v", name, got, c.want)
		}
	}
	if got := startBase(nil, nil); got != nil {
		t.Errorf("startBase with no sessions = %+v, want nil", got)
	}
}

func TestStartConfHint(t *testing.T) {
	_, err := newChoices(testCatalog, []string{"m4"}, nil).startConf(nil)
	if err == nil || !strings.Contains(err.Error(), "mainplane new") {
		t.Errorf("startConf error = %v, want a hint to run mainplane new", err)
	}
}

func TestSwitchConf(t *testing.T) {
	ch := newChoices(testCatalog, []string{"chatgpt", "m4", "lj"}, []statefile.Conf{local})
	sol := presetConf("chatgpt/gpt-6.1-sol", testCatalog.Presets["chatgpt/gpt-6.1-sol"])
	sol.Workers = local.Workers
	lj := statefile.Conf{Model: "lj/millie-35B-A3B-9GB", Tools: "default", ContextLimit: 65536, Workers: local.Workers}
	m4 := statefile.Conf{Model: "m4/qwen3.6-35b-a3b", Tools: "default", ContextLimit: 65536, Workers: local.Workers}
	for s, want := range map[string]statefile.Conf{
		"GPT-6.1 Sol":           sol, // a catalog name
		"lj/millie-35B-A3B-9GB": lj,  // any model of a connected provider, not seen before
		"QWEN3.6-35B-A3B":       m4,  // a recent model, by its model part
		"m4/qwen3.6-35b-a3b":    m4,
	} {
		got, err := ch.switchConf(local, s)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("switchConf(%q) = %+v, %v; want %+v", s, got, err, want)
		}
	}
	for _, s := range []string{"claude opus 5.5" /* only on an unconnected provider here */, "k3q1/agents-a1-4b", "nope", "m4/"} {
		if got, err := ch.switchConf(local, s); err == nil {
			t.Errorf("switchConf(%q) = %+v, want an error", s, got)
		}
	}
}

func TestList(t *testing.T) {
	other := statefile.Conf{Model: "lj/millie-35B-A3B-9GB", Tools: "default", ContextLimit: 65536}
	gone := statefile.Conf{Model: "k3q1/agents-a1-4b", Tools: "default", ContextLimit: 65536}
	ch := newChoices(testCatalog, []string{"claude-code", "chatgpt", "m4", "lj"}, []statefile.Conf{local, gone, other, local})
	got := ch.list(other)
	want := []model{
		{ID: "lj/millie-35B-A3B-9GB", Name: "lj/millie-35B-A3B-9GB", Current: true},
		{ID: "claude-code/claude-opus-5-5", Name: "Claude Opus 5.5"},
		{ID: "chatgpt/gpt-6.1-sol", Name: "GPT-6.1 Sol"},
		{ID: "m4/qwen3.6-35b-a3b", Name: "m4/qwen3.6-35b-a3b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list =\n%+v\nwant\n%+v", got, want)
	}
	if got := newChoices(testCatalog, nil, nil).list(statefile.Conf{}); len(got) != 0 {
		t.Errorf("list with nothing connected = %+v, want none", got)
	}
}
