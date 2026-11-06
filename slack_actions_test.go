package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
	"github.com/slack-go/slack"
)

func TestSlackActionDestinations(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(statePath()), 0700); err != nil {
		t.Fatal(err)
	}
	st := state{Threads: map[string]thread{"slack:C1/1.000001": {Session: "abcd1234", Request: "request-1"}}}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(statePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"post", "reply"} {
		a := slackAction{Action: kind, Session: "abcd1234"}
		if err := a.destination(); err != nil {
			t.Fatal(err)
		}
		want := ""
		if kind == "reply" {
			want = "1.000001"
		}
		if a.Channel != "C1" || a.Thread != want || a.Request != "request-1" {
			t.Fatalf("wrong destination %+v", a)
		}
	}
	a := slackAction{Action: "post", Channel: "C1", Thread: "1.000001", Session: "abcd1234"}
	if err := a.destination(); err == nil {
		t.Fatal("post accepted a thread destination")
	}
	st.Threads["slack:C2/2.000001"] = thread{Session: "abcd1234"}
	data, _ = json.Marshal(st)
	os.WriteFile(statePath(), data, 0600)
	a = slackAction{Action: "post", Session: "abcd1234"}
	if err := a.destination(); err == nil {
		t.Fatal("ambiguous session destination guessed")
	}
}

func TestSlackTextActionsHaveConfirmedReceiptsAndDeduplicate(t *testing.T) {
	for _, kind := range []string{"post", "reply"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			posts := 0
			a := slackAction{Action: kind, Text: "hello", Channel: "C1", Session: "abcd1234", Request: "request-1"}
			if kind == "reply" {
				a.Thread = "1.000001"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/chat.postMessage":
					posts++
					r.ParseForm()
					if r.Form.Get("thread_ts") != a.Thread || r.Form.Get("text") != "hello" || r.Form.Get("channel") != "C1" {
						t.Errorf("wrong post parameters: %v", r.Form)
					}
					fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"2.000001"}`)
				case "/chat.getPermalink":
					fmt.Fprint(w, `{"ok":true,"permalink":"https://example.slack.com/archives/C1/p2000001"}`)
				default:
					t.Errorf("unexpected %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			api := slack.New("test", slack.OptionAPIURL(srv.URL+"/"))
			first, err := sendSlackAction(context.Background(), api, a)
			if err != nil || !first.OK || first.Permalink == "" || first.Message != "2.000001" {
				t.Fatalf("receipt=%+v err=%v", first, err)
			}
			second, err := sendSlackAction(context.Background(), api, a)
			if err != nil || !second.Cached || first.Key != second.Key || posts != 1 {
				t.Fatalf("duplicate action: %+v err=%v posts=%d", second, err, posts)
			}
			a.Request = "request-2"
			if _, err := sendSlackAction(context.Background(), api, a); err != nil || posts != 2 {
				t.Fatalf("new request blocked: %v posts=%d", err, posts)
			}
		})
	}
}

func TestSlackFileActionConfirmsActualShareAndCaption(t *testing.T) {
	for _, kind := range []string{"post", "reply"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "image.png")
			os.WriteFile(path, []byte("image bytes"), 0600)
			a := slackAction{Action: kind, Text: "caption", File: path, Channel: "C1", Session: "abcd1234", Request: "request-file"}
			if kind == "reply" {
				a.Thread = "1.000001"
			}
			var base string
			completed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/files.getUploadURLExternal":
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": "F1", "upload_url": base + "/upload"})
				case "/upload":
					io.Copy(io.Discard, r.Body)
					fmt.Fprint(w, "ok")
				case "/files.completeUploadExternal":
					r.ParseForm()
					if r.Form.Get("thread_ts") != a.Thread || r.Form.Get("initial_comment") != "caption" || r.Form.Get("channel_id") != "C1" {
						t.Errorf("wrong file share: %v", r.Form)
					}
					completed = true
					fmt.Fprint(w, `{"ok":true,"files":[{"id":"F1"}]}`)
				case "/files.info":
					if !completed {
						t.Error("file confirmed before share")
					}
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "file": map[string]any{"id": "F1", "shares": slack.Share{Private: map[string][]slack.ShareFileInfo{"C1": {{Ts: "3.000001", ThreadTs: a.Thread}}}}}})
				case "/chat.getPermalink":
					fmt.Fprint(w, `{"ok":true,"permalink":"https://example.slack.com/archives/C1/p3000001"}`)
				default:
					t.Errorf("unexpected %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			base = srv.URL
			r, err := sendSlackAction(context.Background(), slack.New("test", slack.OptionAPIURL(base+"/")), a)
			if err != nil || !r.OK || r.File != "F1" || r.Message != "3.000001" || r.Thread != a.Thread {
				t.Fatalf("receipt=%+v err=%v", r, err)
			}
		})
	}
}

func TestUnconfirmedSendNeverRetriesBlindly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			posts++
			fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"2.000001"}`)
			return
		}
		fmt.Fprint(w, `{"ok":false,"error":"temporarily_unavailable"}`)
	}))
	defer srv.Close()
	api := slack.New("test", slack.OptionAPIURL(srv.URL+"/"))
	a := slackAction{Action: "post", Text: "hello", Channel: "C1", Session: "abcd1234", Request: "request-1", Key: "one-operation"}
	first, err := sendSlackAction(context.Background(), api, a)
	if err == nil || first.OK {
		t.Fatal("unconfirmed send reported success")
	}
	second, err := sendSlackAction(context.Background(), api, a)
	if err == nil || second.OK || posts != 1 {
		t.Fatalf("blind retry: posts=%d err=%v", posts, err)
	}
	a.Text = "different payload"
	if _, err := sendSlackAction(context.Background(), api, a); err == nil || posts != 1 {
		t.Fatal("same key accepted different payload")
	}
}

func TestOnlyPersistedCurrentRequestReceiptCounts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	key := strings.Repeat("a", 64)
	hint, _ := json.Marshal(slackReceipt{OK: true, Key: key})
	if confirmedSlackReceipt(hint, "abcd1234", "request-1") != nil {
		t.Fatal("arbitrary tool JSON trusted")
	}
	os.MkdirAll(slackActionDir(), 0700)
	path, _ := slackReceiptPath(key)
	r := slackReceipt{OK: true, Key: key, State: "confirmed", Session: "abcd1234", Request: "request-1", Fingerprint: "payload", Permalink: "https://example.slack.com/message"}
	if err := saveSlackReceipt(path, r); err != nil {
		t.Fatal(err)
	}
	if confirmedSlackReceipt(hint, "abcd1234", "request-1") == nil {
		t.Fatal("real receipt ignored")
	}
	if confirmedSlackReceipt(hint, "abcd1234", "request-2") != nil {
		t.Fatal("old request replay accepted")
	}
	if _, err := slackReceiptPath("../credentials"); err == nil {
		t.Fatal("receipt traversal accepted")
	}
}

func TestNativeSenderReceiptCompletesCorrectionWithoutDuplicateUpload(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b, c := testCore(t)
	key := strings.Repeat("b", 64)
	os.MkdirAll(slackActionDir(), 0700)
	path, _ := slackReceiptPath(key)
	r := slackReceipt{OK: true, Key: key, State: "confirmed", Session: "abcd1234", Request: "abcd1234", Fingerprint: "payload", Permalink: "https://example.slack.com/confirmed"}
	saveSlackReceipt(path, r)
	data, _ := json.Marshal(r)
	tr := &turn{attachmentExpected: true, deliveryRecovery: true}
	b.relay("slack:C1/1.0", "abcd1234", tr, statefile.Record{Header: statefile.Header{Kind: statefile.Result}, Body: data})
	tr.text = []string{"attach: /tmp/already-sent.png"}
	b.finishStep("slack:C1/1.0", "abcd1234", tr, statefile.Record{})
	if len(c.messages) != 1 || len(c.messages[0].Files) != 0 || !strings.Contains(c.messages[0].Text, r.Permalink) || c.statuses[0].outcome != "" {
		t.Fatal("verified native action re-uploaded or failed")
	}
}

func TestSlackCLIRejectsInvalidActionWithoutCredentials(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "")
	var out bytes.Buffer
	if err := runSlackCLI([]string{"post", "--session", "abcd1234", "--channel", "C1", "--thread", "1.000001", "--text", "hello"}, &out); err == nil {
		t.Fatal("post accepted thread")
	}
	var r slackReceipt
	if json.Unmarshal(out.Bytes(), &r) != nil || r.OK || !strings.Contains(r.Error, "top-level") {
		t.Fatalf("bad rejection: %s", out.String())
	}
}

func TestSlackSenderHelpNeedsNoCredentialsAndIsNotAnAction(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "")
	var out bytes.Buffer
	if err := runSlackCLI([]string{"post", "--help"}, &out); err != nil || !strings.Contains(out.String(), "usage:") {
		t.Fatalf("help failed: %v %s", err, out.String())
	}
	if slackActionCall([]byte(`{"name":"run","arguments":{"code":"mainplane-chat slack post --help"}}`)) {
		t.Fatal("help was treated as an attempted send")
	}
}

func TestSlackRequestIdentitySurvivesContinuation(t *testing.T) {
	b, _ := testCore(t)
	th := b.st.Threads["slack:C1/1.0"]
	th.Request = "1793980000.000001"
	b.st.Threads["slack:C1/1.0"] = th
	b.bind("slack:C1/1.0", "eeeeeeee", 0)
	if b.st.Threads["slack:C1/1.0"].Request != th.Request {
		t.Fatal("continuation lost request idempotency scope")
	}
}

func TestSenderErrorsDoNotLeakSignedURLsOrCredentials(t *testing.T) {
	input := `Post "https://files.slack.com/upload/v1/private-signature?secret=abc": timeout xoxb-test-token`
	got := slackErrorSecrets.ReplaceAllString(input, "[redacted]")
	if strings.Contains(got, "private-signature") || strings.Contains(got, "xoxb-") || !strings.Contains(got, "timeout") {
		t.Fatalf("unsafe error: %s", got)
	}
}

func TestUnconfirmedNativeActionCannotClaimSent(t *testing.T) {
	b, c := testCore(t)
	tr := &turn{}
	relay := func(kind statefile.Kind, body string, header statefile.Header) {
		header.Kind = kind
		b.relay("slack:C1/1.0", "abcd1234", tr, statefile.Record{Header: header, Body: []byte(body)})
	}
	relay(statefile.Call, `{"name":"run","arguments":{"code":"mainplane-chat slack post --text hello"}}`, statefile.Header{ID: "send-call"})
	relay(statefile.Step, "", statefile.Header{Provider: "openai-chat", Stop: "tool_calls"})
	relay(statefile.Result, `{"ok":false,"error":"missing_scope"}`, statefile.Header{For: "send-call"})
	relay(statefile.Text, "SENT", statefile.Header{})
	relay(statefile.Step, "", statefile.Header{Provider: "openai-chat", Stop: "stop"})
	if c.statuses[len(c.statuses)-1].outcome != "failed" {
		t.Fatal("unconfirmed action marked done")
	}
	for _, m := range c.messages {
		if m.Text == "SENT" {
			t.Fatal("unsupported action claim posted")
		}
	}
}
