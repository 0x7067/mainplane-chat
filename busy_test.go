package main

import (
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

func TestRunningBackgroundJobs(t *testing.T) {
	call := statefile.Record{Header: statefile.Header{Kind: statefile.Call, ID: "call1"}, Body: []byte(`{"name":"run","arguments":{"background":true,"worker":"worker"}}`)}
	start := statefile.Record{Header: statefile.Header{Kind: statefile.Result, For: "call1"}, Body: []byte("started job call1 on worker")}
	report := statefile.Record{Header: statefile.Header{Kind: statefile.Message, Via: "job", IdempotencyKey: "call1"}}
	lost := statefile.Record{Header: statefile.Header{Kind: statefile.System, Source: "job", IdempotencyKey: "call1"}}
	stop := statefile.Record{Header: statefile.Header{Kind: statefile.Error, Via: "slack"}}
	stopNote := statefile.Record{Header: statefile.Header{Kind: statefile.System, Source: "stop"}}
	for _, tc := range []struct {
		name    string
		records []statefile.Record
		want    int
	}{
		{"unacknowledged", []statefile.Record{call}, 0},
		{"running", []statefile.Record{call, start}, 1},
		{"reported", []statefile.Record{call, start, report}, 0},
		{"fast job report before start result", []statefile.Record{call, report, start}, 0},
		{"lost on restart", []statefile.Record{call, start, lost}, 0},
		{"stopped", []statefile.Record{call, start, stop}, 0},
		{"idle stop", []statefile.Record{call, start, stopNote}, 0},
		{"provider error does not kill job", []statefile.Record{call, start, {Header: statefile.Header{Kind: statefile.Error}}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runningBackgroundJobs(tc.records); got != tc.want {
				t.Errorf("jobs=%d want %d", got, tc.want)
			}
		})
	}
}

func TestBusyCountIsLocalAndExcludesSelf(t *testing.T) {
	b := &bridge{busy: map[string]bool{"self": true, "other": true, "idle": false}}
	// No harness/client at all: the status path must only read its snapshot.
	if got := b.busyCount("self"); got != 1 {
		t.Errorf("other busy=%d want 1", got)
	}
	if got := b.busyCount("unknown"); got != 2 {
		t.Errorf("busy=%d want 2", got)
	}
}

func TestBusyAuthorIsPersonNotConnector(t *testing.T) {
	b := &bridge{}
	for _, first := range []string{"[alice <@U01>]: hi", "(Session context note)\n\n(Note from the Slack bridge)\n\n[alice <@U01>]: hi"} {
		if got := b.extractAuthor("slack", first); got != "alice <@U01>" {
			t.Errorf("author=%q", got)
		}
	}
	if got := b.extractAuthor("cli", "hi"); got != "cli" {
		t.Errorf("connector fallback=%q", got)
	}
}
