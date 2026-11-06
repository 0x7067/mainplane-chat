package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

type testChat struct {
	messages  []msg
	statuses  []turn
	reactions []string
}

func (c *testChat) say(_ string, m msg)          { c.messages = append(c.messages, m) }
func (c *testChat) status(_, _ string, tr *turn) { c.statuses = append(c.statuses, *tr) }
func (c *testChat) react(_, _, emoji string)     { c.reactions = append(c.reactions, emoji) }
func (c *testChat) rename(_, _, _ string)        {}
func (c *testChat) close(_ string)               {}

func testCore(t *testing.T) (*bridge, *testChat) {
	t.Helper()
	c := &testChat{}
	b := &bridge{path: filepath.Join(t.TempDir(), "state.json"), chats: map[string]chat{"slack": c},
		full: map[string]bool{}, carried: map[string][]string{}, sentIn: map[string]string{}, following: map[[2]string]bool{}, renames: map[string]*title{},
		st: state{Seen: map[string]string{}, Threads: map[string]thread{"slack:C1/1.0": {Session: "abcd1234"}}}}
	return b, c
}

func TestContinuationContext(t *testing.T) {
	rec := func(k statefile.Kind, via, body string) statefile.Record {
		return statefile.Record{Header: statefile.Header{Kind: k, Via: via, Type: "text/plain"}, Body: []byte(body)}
	}
	recs := []statefile.Record{
		rec(statefile.Message, "slack", sessionContext("slack:C1/1.0", "abcd1234")+"\n\n(Note from the Slack bridge: in #drawings.)\n\n[pedro <@U1>]: draw the three pieces and send the combined image as a top-level message"),
		rec(statefile.Text, "", "Let me work on that."),
		rec(statefile.Call, "", `{"name":"run","arguments":{"worker":"admin","command":"combine > /tmp/pelican.png"}}`),
		rec(statefile.Result, "", "created /tmp/pelican.png on admin"),
		rec(statefile.Thinking, "", strings.Repeat("private-thinking", 10000)),
		rec(statefile.Message, "retry", "Continue"),
		rec(statefile.Message, "job", "job-only-message"),
	}
	got := continuationContext(recs)
	for _, want := range []string{"#drawings", "[pedro <@U1>]", "top-level message", "created /tmp/pelican.png", `"worker":"admin"`} {
		if !strings.Contains(got, want) {
			t.Errorf("handoff lacks %q: %s", want, got)
		}
	}
	for _, unwanted := range []string{"private-thinking", "job-only-message"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("handoff includes %q", unwanted)
		}
	}
	if strings.Contains(got, "your current Mainplane session is abcd1234") {
		t.Fatal("old identity leaked into the new handoff")
	}
	// Progress text must not erase the task; the next continuation must not
	// need to rediscover it by reading raw logs either.
	next := continuationContext([]statefile.Record{rec(statefile.Message, "slack", continuePrefix+"abcd1234.\n"+got), rec(statefile.Message, "slack", "[pedro <@U1>]: where is it?")})
	if !strings.Contains(next, "top-level message") || !strings.Contains(next, "where is it?") {
		t.Errorf("lost requests in second handoff: %s", next)
	}
	recs = append(recs, rec(statefile.Result, "", strings.Repeat("é", 100000)+" /tmp/final.png"))
	got = continuationContext(recs)
	if len(got) > maxHandoff || !utf8.ValidString(got) || !strings.Contains(got, "/tmp/final.png") {
		t.Fatalf("bad bounded handoff: bytes=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestHandoffExcerpt(t *testing.T) {
	for limit := 1; limit < 100; limit++ {
		got := handoffExcerpt(strings.Repeat("é🍉", 100), limit)
		if len(got) > limit || !utf8.ValidString(got) {
			t.Fatalf("limit %d: %q (%d bytes)", limit, got, len(got))
		}
	}
}

func TestEndedFullIncludesGeneratedTokens(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		prompt, output int
		want           bool
	}{
		{"real overflow after long thinking", `{"type":"exceed_context_size_error","n_prompt_tokens":81997,"n_ctx":81920}`, 29116, 52804, true},
		{"length exhausted session", `The last step ended with stop "length" (52804 output tokens) and no call`, 29116, 52804, true},
		{"short output limit", `The last step ended with stop "length" (512 output tokens) and no call`, 6000, 512, false},
		{"oversized new input", `{"type":"exceed_context_size_error","n_prompt_tokens":200025,"n_ctx":81920}`, 6000, 512, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := testCore(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/records") {
					body, _ := statefile.Encode(statefile.Record{Header: statefile.Header{Kind: statefile.Step, Usage: &statefile.Usage{Input: tc.prompt, Output: tc.output}}})
					w.Write(body)
				} else {
					json.NewEncoder(w).Encode(info{ContextUsed: tc.prompt, Config: statefile.Conf{ContextLimit: 65536}})
				}
			}))
			defer srv.Close()
			b.harness = srv.URL
			if got := b.endedFull("abcd1234", tc.body); got != tc.want {
				t.Fatalf("endedFull=%v want %v", got, tc.want)
			}
		})
	}
}

func TestAbnormalStepDoesNotFinishSuccessfully(t *testing.T) {
	b, c := testCore(t)
	tr := &turn{start: time.Now(), text: []string{"truncated"}, asked: []string{"question"}}
	b.relay("slack:C1/1.0", "abcd1234", tr, statefile.Record{Header: statefile.Header{Kind: statefile.Step, Provider: "openai-chat", Stop: "length"}})
	if len(c.statuses) != 0 || len(c.reactions) != 0 || len(tr.text) != 1 {
		t.Fatal("cut-off step marked done or discarded its pending text")
	}
	b.relay("slack:C1/1.0", "abcd1234", tr, statefile.Record{Header: statefile.Header{Kind: statefile.Step, Provider: "openai-chat", Stop: "stop"}})
	if len(c.statuses) != 1 || c.statuses[0].outcome != "" {
		t.Fatal("normal step did not finish")
	}
}

func TestStaleSessionErrorDoesNotOfferRetry(t *testing.T) {
	b, c := testCore(t)
	tr := &turn{start: time.Now(), status: "old-status"}
	b.relay("slack:C1/1.0", "eeeeeeee", tr, statefile.Record{Header: statefile.Header{Kind: statefile.Error, Time: time.Now()}, Body: []byte("old error")})
	if len(c.messages) != 0 || len(c.reactions) != 0 || len(c.statuses) != 1 || c.statuses[0].outcome != "continued" {
		t.Fatalf("stale error changed the new session: %+v", c)
	}
}

func TestFailedContinuationCanBeRetried(t *testing.T) {
	b, _ := testCore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	b.harness = srv.URL
	for range 2 {
		if b.continueFull("slack:C1/1.0", "abcd1234") || b.full["abcd1234"] {
			t.Fatal("failed continuation poisoned the recovery latch")
		}
		if b.sessionFor("slack:C1/1.0") != "abcd1234" {
			t.Fatal("failed continuation moved the thread")
		}
	}
}

func TestReplyAttachmentDestinations(t *testing.T) {
	b, c := testCore(t)
	path := filepath.Join(t.TempDir(), "image.png")
	data := []byte("image bytes")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	b.reply("slack:C1/1.0", fmt.Sprintf("Here it is\nattach: %s\nattach-channel: %s", path, path), "", false)
	if len(c.messages) != 1 || len(c.messages[0].Files) != 2 {
		t.Fatalf("no files: %+v", c.messages)
	}
	for i, f := range c.messages[0].Files {
		got, err := f.fetch()
		if err != nil || !bytes.Equal(got, data) || f.topLevel != (i == 1) {
			t.Errorf("file %d: %v top=%v", i, err, f.topLevel)
		}
	}
	b.reply("slack:C1/1.0", "Example only:\n```\nattach: "+path+"\n```", "", false)
	if len(c.messages[1].Files) != 0 {
		t.Fatal("code example uploaded a file")
	}
}

func TestContinuationTransaction(t *testing.T) {
	for _, failSend := range []bool{false, true} {
		t.Run(fmt.Sprint(failSend), func(t *testing.T) {
			b, _ := testCore(t)
			// Exercise the transaction without starting a long-polling goroutine.
			b.following[[2]string{"slack:C1/1.0", "eeeeeeee"}] = true
			var deleted, retried bool
			var posted string
			stamp := time.Now()
			step := statefile.Record{Header: statefile.Header{N: 10, Kind: statefile.Step, Provider: "openai-chat", Stop: "length", Usage: &statefile.Usage{Input: 29116, Output: 52804}}}
			errRecord := statefile.Record{Header: statefile.Header{N: 11, Kind: statefile.Error}, Body: []byte(`{"type":"exceed_context_size_error","n_prompt_tokens":81997,"n_ctx":81920}`)}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/sessions/abcd1234":
					json.NewEncoder(w).Encode(info{ID: "abcd1234", N: 11, ContextUsed: 29116, Config: statefile.Conf{Model: "local/model", ContextLimit: 65536}})
				case r.Method == "GET" && r.URL.Path == "/sessions/eeeeeeee/records":
					// The failed-send case is confirmed empty before cleanup.
				case r.Method == "GET" && r.URL.Path == "/sessions/cccccccc/records":
					for _, rec := range []statefile.Record{
						{Header: statefile.Header{N: 6, Kind: statefile.Message, Via: "slack", Type: "text/plain", Time: stamp.Add(-time.Hour)}, Body: []byte("draw a pelican and send it")},
						{Header: statefile.Header{N: 7, Kind: statefile.Message, Via: "cli", Type: "text/plain", Time: stamp.Add(time.Hour)}, Body: []byte("future-unrelated-user-message")},
					} {
						data, _ := statefile.Encode(rec)
						w.Write(data)
					}
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/records"):
					for _, rec := range []statefile.Record{{Header: statefile.Header{N: 6, Kind: statefile.Message, Via: "slack", Type: "text/plain", Time: stamp}, Body: []byte(continuePrefix + "cccccccc. Retrieve key context from it as you go.")}, {Header: statefile.Header{N: 7, Kind: statefile.Message, Via: "slack", Type: "text/plain", Time: stamp.Add(time.Minute)}, Body: []byte("where is it?")}, step, errRecord} {
						data, _ := statefile.Encode(rec)
						w.Write(data)
					}
				case r.URL.Path == "/workers":
					fmt.Fprint(w, "[]")
				case r.Method == "POST" && r.URL.Path == "/sessions":
					fmt.Fprint(w, `{"id":"eeeeeeee"}`)
				case r.Method == "POST" && r.URL.Path == "/sessions/eeeeeeee/records":
					data, _ := io.ReadAll(r.Body)
					posted = string(data)
					if failSend {
						http.Error(w, "send unavailable", 503)
					} else {
						fmt.Fprint(w, `{"n":6}`)
					}
				case r.Method == "DELETE" && r.URL.Path == "/sessions/eeeeeeee":
					deleted = true
					fmt.Fprint(w, "{}")
				case strings.HasSuffix(r.URL.Path, "/retry"):
					retried = true
					fmt.Fprint(w, "{}")
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer srv.Close()
			b.harness = srv.URL
			// A user intentionally retries even an old error. It must migrate,
			// not retry an already-full provider request on the old session.
			b.command("slack:C1/1.0", "abcd1234", "button", "!retry")
			want := "eeeeeeee"
			if failSend {
				want = "abcd1234"
			}
			if got := b.sessionFor("slack:C1/1.0"); got != want {
				t.Errorf("session=%s want %s", got, want)
			}
			if retried || deleted != failSend || !strings.Contains(posted, "draw a pelican") || !strings.Contains(posted, "attach-channel:") {
				t.Errorf("retry=%v cleanup=%v handoff=%s", retried, deleted, posted)
			}
			if !strings.Contains(posted, "your current Mainplane session is eeeeeeee") || !strings.HasPrefix(posted, continuePrefix+"abcd1234.") {
				t.Fatal("continuation identity or predecessor missing")
			}
			if strings.Contains(posted, "future-unrelated-user-message") {
				t.Fatal("read predecessor changes after the thread moved away")
			}
			if failSend && b.full["abcd1234"] {
				t.Fatal("recovery remained latched")
			}
		})
	}
}

func TestStalledContinuation(t *testing.T) {
	first := statefile.Record{Header: statefile.Header{Kind: statefile.Message, IdempotencyKey: fullKey + "abcd1234"}}
	length := statefile.Record{Header: statefile.Header{Kind: statefile.Step, Provider: "openai-chat", Stop: "length"}}
	if !stalledContinuation([]statefile.Record{first, length}) {
		t.Fatal("fresh no-progress continuation can loop")
	}
	if stalledContinuation([]statefile.Record{length}) {
		t.Fatal("ordinary session falsely blocked")
	}
	result := statefile.Record{Header: statefile.Header{Kind: statefile.Result}}
	if stalledContinuation([]statefile.Record{first, result, length}) {
		t.Fatal("session made progress")
	}
}

func TestAcceptedMessageWithLostResponse(t *testing.T) {
	b, c := testCore(t)
	b.following[[2]string{"slack:C1/1.0", "eeeeeeee"}] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			http.Error(w, "lost response", 503)
			return
		}
		data, _ := statefile.Encode(statefile.Record{Header: statefile.Header{N: 6, Kind: statefile.Message, IdempotencyKey: "original-message"}})
		w.Write(data)
	}))
	defer srv.Close()
	b.harness = srv.URL
	if err := b.post("slack:C1/1.0", "eeeeeeee", "original-message", "", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if b.sessionFor("slack:C1/1.0") != "eeeeeeee" || len(c.messages) != 0 {
		t.Fatal("accepted message was lost or reported failed")
	}
}

func TestPartialMultipartSendIsNotComplete(t *testing.T) {
	b, _ := testCore(t)
	b.following[[2]string{"slack:C1/1.0", "eeeeeeee"}] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			http.Error(w, "partial response", 503)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/records") {
			json.NewEncoder(w).Encode(info{Config: statefile.Conf{Input: []string{"image/png"}}})
			return
		}
		// The text was accepted, but the image was not.
		data, _ := statefile.Encode(statefile.Record{Header: statefile.Header{N: 6, Kind: statefile.Message, IdempotencyKey: "original-message"}})
		w.Write(data)
	}))
	defer srv.Close()
	b.harness = srv.URL
	image := file{name: "proof.png", ctype: "image/png", size: 3, fetch: func() ([]byte, error) { return []byte("png"), nil }}
	if err := b.post("slack:C1/1.0", "eeeeeeee", "original-message", "", "hello", []file{image}); err == nil {
		t.Fatal("partially accepted multipart message reported complete")
	}
	if b.sessionFor("slack:C1/1.0") != "abcd1234" {
		t.Fatal("partial message moved the thread")
	}
}
