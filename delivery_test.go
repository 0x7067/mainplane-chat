package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
	"github.com/slack-go/slack"
)

func TestAttachmentIntentNotArbitraryToolOutput(t *testing.T) {
	for text, want := range map[string]bool{
		"attach: /tmp/image.png":           true,
		"attach-channel: /tmp/image.png":   true,
		"```\nattach: /tmp/image.png\n```": false,
		"Example: attach: /tmp/image.png":  false,
		"attach: ":                         false,
	} {
		if got := hasAttachment(text); got != want {
			t.Errorf("directive %q = %v", text, got)
		}
	}
	for body, want := range map[string]bool{
		`{"name":"run","arguments":{"code":"cat <<'EOF'\nattach: /tmp/image.png\nEOF"}}`: true,
		`{"name":"run","arguments":{"command":"echo 'attach-channel: /tmp/image.png'"}}`: true,
		`{"name":"read","arguments":{"path":"/tmp/attach: example"}}`:                    false,
		`{"name":"run","arguments":{"command":"cat /tmp/untrusted-page"}}`:               false,
	} {
		if got := attachmentCall([]byte(body)); got != want {
			t.Errorf("intent %s = %v", body, got)
		}
	}
}

func TestShellEchoCannotFinishAsDelivery(t *testing.T) {
	b, c := testCore(t)
	const th, id = "slack:C1/1.0", "abcd1234"
	b.following[[2]string{th, id}] = true
	var correction, correctionKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/records") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		correction = string(data)
		correctionKey = r.Header.Get("Idempotency-Key")
		fmt.Fprint(w, `{"n":14}`)
	}))
	defer srv.Close()
	b.harness = srv.URL
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	tr := &turn{start: time.Now(), key: "user-question", asked: []string{"user-question"}}
	relay := func(kind statefile.Kind, n int, body string, header statefile.Header) {
		header.Kind, header.N, header.Time = kind, n, time.Now()
		b.relay(th, id, tr, statefile.Record{Header: header, Body: []byte(body)})
	}
	relay(statefile.Call, 10, `{"name":"run","arguments":{"code":"cat <<'EOF'\nattach: /tmp/image.png\nEOF"}}`, statefile.Header{ID: "call1"})
	relay(statefile.Step, 11, "", statefile.Header{ID: "step1", Provider: "openai-chat", Stop: "tool_calls"})
	relay(statefile.Result, 12, "attach: "+path, statefile.Header{For: "call1"})
	relay(statefile.Text, 13, "I've sent the image.", statefile.Header{})
	relay(statefile.Step, 14, "", statefile.Header{ID: "step2", Provider: "openai-chat", Stop: "stop"})
	if !strings.Contains(correction, deliveryCorrection) || !strings.HasPrefix(correctionKey, deliveryKey) {
		t.Fatal("no bounded corrective turn")
	}
	for _, m := range c.messages {
		if strings.Contains(m.Text, "I've sent") || len(m.Files) > 0 {
			t.Fatal("untrusted tool output uploaded or unsupported delivery claim posted")
		}
	}
	if slices.Contains(c.reactions, done) {
		t.Fatal("shell echo marked successful")
	}
	if got := c.statuses[len(c.statuses)-1].outcome; got != "continued" {
		t.Errorf("outcome=%q", got)
	}
	saved := b.st.Threads[th]
	if !saved.AttachmentPending || !saved.AttachmentRecovery {
		t.Fatal("pending correction not persisted")
	}
	// The internal correction must not notify a made-up user or quote itself.
	count := len(c.messages)
	relay(statefile.Message, 15, correction, statefile.Header{Via: "slack", Type: "text/plain", IdempotencyKey: correctionKey})
	if len(c.messages) != count {
		t.Fatal("internal correction leaked to the Slack thread")
	}
	relay(statefile.Text, 16, "attach-channel: "+path, statefile.Header{})
	relay(statefile.Step, 17, "", statefile.Header{ID: "step3", Provider: "openai-chat", Stop: "stop"})
	last := c.messages[len(c.messages)-1]
	if len(last.Files) != 1 || !last.Files[0].topLevel || c.statuses[len(c.statuses)-1].outcome != "" {
		t.Fatal("corrected assistant delivery did not finish successfully")
	}
	if !slices.Contains(c.reactions, done) {
		t.Fatal("original question was not marked after delivery")
	}
}

func TestDeliveryCorrectionDoesNotLoop(t *testing.T) {
	b, c := testCore(t)
	tr := &turn{start: time.Now(), attachmentExpected: true, deliveryRecovery: true, text: []string{"I've sent it."}, asked: []string{"question"}}
	b.finishStep("slack:C1/1.0", "abcd1234", tr, statefile.Record{Header: statefile.Header{Time: time.Now()}})
	if c.statuses[len(c.statuses)-1].outcome != "failed" || slices.Contains(c.reactions, done) {
		t.Fatal("failed correction marked successful")
	}
	for _, m := range c.messages {
		if strings.Contains(m.Text, "I've sent") {
			t.Fatal("unsupported claim was posted")
		}
	}
}

func TestFinishWaitsForDeliveryAndPropagatesFailure(t *testing.T) {
	for _, failure := range []string{"", "upload", "missing file"} {
		t.Run(failure, func(t *testing.T) {
			b, c := testCore(t)
			path := filepath.Join(t.TempDir(), "proof.txt")
			if failure != "missing file" {
				if err := os.WriteFile(path, []byte("proof"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "upload" {
				c.sendErr = errors.New("upload rejected")
			}
			tr := &turn{start: time.Now(), text: []string{"attach: " + path}, asked: []string{"question"}}
			b.finish("slack:C1/1.0", "abcd1234", tr, "")
			want := ""
			if failure != "" {
				want = "failed"
			}
			if c.statuses[len(c.statuses)-1].outcome != want {
				t.Errorf("outcome=%q want %q", c.statuses[len(c.statuses)-1].outcome, want)
			}
			if failure != "" && slices.Contains(c.reactions, done) {
				t.Fatal("failed file marked done")
			}
		})
	}
}

func TestDeliveryFactsSurviveAdvanceAndReload(t *testing.T) {
	b, _ := testCore(t)
	tr := &turn{attachmentExpected: true, deliveryRecovery: true}
	b.deliveryState("slack:C1/1.0", "abcd1234", tr)
	b.advance("slack:C1/1.0", "abcd1234", 100)
	b.bind("slack:C1/1.0", "abcd1234", 101)
	reloaded := &bridge{path: b.path}
	reloaded.load()
	if got := reloaded.st.Threads["slack:C1/1.0"]; !got.AttachmentPending || !got.AttachmentRecovery || got.N != 101 {
		t.Fatalf("lost pending delivery: %+v", got)
	}
	b.bind("slack:C1/1.0", "eeeeeeee", 0)
	if b.st.Threads["slack:C1/1.0"].AttachmentPending {
		t.Fatal("new session inherited stale delivery flags")
	}
}

func TestExistingSlackMessagesAlwaysGetDeliveryProtocol(t *testing.T) {
	b, _ := testCore(t)
	b.following[[2]string{"slack:C1/1.0", "abcd1234"}] = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		for _, want := range []string{"mainplane-chat slack post", "NEW TOP-LEVEL CHANNEL MESSAGE", "current Mainplane session is abcd1234"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("message lacks %q", want)
			}
		}
		fmt.Fprint(w, `{"n":10}`)
	}))
	defer srv.Close()
	b.harness = srv.URL
	if err := b.post("slack:C1/1.0", "abcd1234", "existing-message", "[alice <@U1>]: ", "send the image", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSlackUploadFailureIsReturnedNotAcknowledged(t *testing.T) {
	var note string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files.getUploadURLExternal" {
			fmt.Fprint(w, `{"ok":false,"error":"missing_scope"}`)
			return
		}
		r.ParseForm()
		note = r.Form.Get("text")
		fmt.Fprint(w, `{"ok":true,"ts":"2.0"}`)
	}))
	defer srv.Close()
	s := &slackChat{sl: slack.New("test", slack.OptionAPIURL(srv.URL+"/"))}
	err := s.say("slack:C1/1.0", msg{Files: []file{{name: "proof.txt", topLevel: true, fetch: func() ([]byte, error) { return []byte("proof"), nil }}}})
	if err == nil || !strings.Contains(note, "could not attach") || strings.Contains(note, "Uploaded") {
		t.Fatalf("err=%v note=%q", err, note)
	}
}
