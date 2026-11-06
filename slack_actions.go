package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
	"github.com/slack-go/slack"
)

// Sending is an executed tool action. Its durable, metadata-only receipt is
// also the retry boundary: never repeat a possibly accepted write blindly.
const slackActionWait = 30 * time.Second

var slackChannelID = regexp.MustCompile(`^[CDG][A-Z0-9]+$`)

type slackAction struct {
	Action, Text, File, Channel, Thread, Session, Request, Key string
}

type slackReceipt struct {
	OK          bool      `json:"ok"`
	State       string    `json:"state"`
	Key         string    `json:"key"`
	Action      string    `json:"action"`
	Session     string    `json:"session"`
	Request     string    `json:"request"`
	Channel     string    `json:"channel"`
	Thread      string    `json:"thread_ts,omitempty"`
	Message     string    `json:"message_ts,omitempty"`
	File        string    `json:"file_id,omitempty"`
	Permalink   string    `json:"permalink,omitempty"`
	Fingerprint string    `json:"fingerprint"`
	Created     time.Time `json:"created"`
	Cached      bool      `json:"cached,omitempty"`
	Error       string    `json:"error,omitempty"`
}

func slackActionDir() string { return filepath.Join(filepath.Dir(statePath()), "slack-actions") }

func slackReceiptPath(key string) (string, error) {
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(key) != key {
		return "", errors.New("invalid receipt key")
	}
	return filepath.Join(slackActionDir(), key+".json"), nil
}

func runSlackCLI(args []string, out io.Writer) error {
	finish := func(r slackReceipt, err error) error {
		if err != nil {
			r.OK, r.Error = false, err.Error()
		}
		if encodeErr := json.NewEncoder(out).Encode(r); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, "usage: mainplane-chat slack post|reply --text TEXT [--file PATH] [--channel ID] [--thread TS] [--key NAME]\n       mainplane-chat slack receipt --key RECEIPT_ID\npost always creates a top-level channel message; reply stays in a thread. MAINPLANE_SESSION selects the current Slack conversation. A confirmed send returns ok:true and a message permalink. Retrying the same payload for the same request reuses its receipt. Use a distinct --key only for an intentionally separate action.")
		return nil
	}
	a := slackAction{Action: args[0]}
	fs := flag.NewFlagSet("slack "+a.Action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.Text, "text", "", "message or file caption")
	fs.StringVar(&a.File, "file", "", "file on the bridge host")
	fs.StringVar(&a.Channel, "channel", "", "default: this session's Slack channel")
	fs.StringVar(&a.Thread, "thread", "", "reply only: thread timestamp")
	fs.StringVar(&a.Session, "session", os.Getenv("MAINPLANE_SESSION"), "default: MAINPLANE_SESSION")
	fs.StringVar(&a.Key, "key", "", "explicit operation name, or receipt id for receipt")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return runSlackCLI([]string{"help"}, out)
		}
		return finish(slackReceipt{}, err)
	}
	if fs.NArg() != 0 {
		return finish(slackReceipt{}, errors.New("unexpected positional arguments"))
	}
	if a.Action != "post" && a.Action != "reply" && a.Action != "receipt" {
		return finish(slackReceipt{}, errors.New("action must be post, reply or receipt"))
	}
	if a.Action != "receipt" {
		if !statefile.ValidID(a.Session) {
			return finish(slackReceipt{}, errors.New("a valid MAINPLANE_SESSION or --session is required"))
		}
		if a.Text == "" && a.File == "" {
			return finish(slackReceipt{}, errors.New("--text and/or --file is required"))
		}
		if err := a.destination(); err != nil {
			return finish(slackReceipt{}, err)
		}
	}
	token := os.Getenv("SLACK_BOT_TOKEN")
	if token == "" {
		return finish(slackReceipt{}, errors.New("SLACK_BOT_TOKEN is not configured for the sender; use the installed command on the bridge worker"))
	}
	api := slack.New(token)
	ctx, cancel := context.WithTimeout(context.Background(), slackActionWait)
	defer cancel()
	if a.Action == "receipt" {
		path, err := slackReceiptPath(a.Key)
		if err != nil {
			return finish(slackReceipt{}, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return finish(slackReceipt{}, err)
		}
		var r slackReceipt
		if err = json.Unmarshal(data, &r); err != nil {
			return finish(r, err)
		}
		if !r.OK {
			err = confirmSlackAction(ctx, api, &r)
			if saveErr := saveSlackReceipt(path, r); saveErr != nil {
				err = errors.Join(err, saveErr)
			}
		}
		return finish(r, err)
	}
	r, err := sendSlackAction(ctx, api, a)
	return finish(r, err)
}

func (a *slackAction) destination() error {
	if a.Action == "post" && a.Thread != "" {
		return errors.New("post is top-level and does not accept --thread; use reply for a thread")
	}
	var st state
	if data, err := os.ReadFile(statePath()); err == nil {
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("read chat mapping: %w", err)
		}
	} else if a.Channel == "" {
		return fmt.Errorf("read chat mapping: %w", err)
	}
	var matches []string
	for th, t := range st.Threads {
		ch, ts := threadParts(th)
		if platform(th) == "slack" && t.Session == a.Session && (a.Channel == "" || a.Channel == ch) && (a.Thread == "" || a.Thread == ts) {
			matches = append(matches, th)
		}
	}
	if len(matches) > 1 {
		return errors.New("session is shown in multiple Slack threads; select an unambiguous destination with --channel (and --thread for reply)")
	}
	if len(matches) == 1 {
		th := matches[0]
		ch, ts := threadParts(th)
		if a.Channel == "" {
			a.Channel = ch
		}
		if a.Action == "reply" && a.Thread == "" {
			a.Thread = ts
		}
		a.Request = st.Threads[th].Request
	}
	if !slackChannelID.MatchString(a.Channel) {
		return errors.New("no valid Slack channel; use a mapped Slack session or --channel")
	}
	if a.Action == "reply" && !isTS(a.Thread) {
		return errors.New("reply requires the mapped Slack thread or --thread")
	}
	if a.Request == "" {
		a.Request = a.Session
	}
	return nil
}

func sendSlackAction(ctx context.Context, api *slack.Client, a slackAction) (slackReceipt, error) {
	r := slackReceipt{State: "rejected", Action: a.Action, Session: a.Session, Request: a.Request, Channel: a.Channel, Thread: a.Thread, Created: time.Now().UTC()}
	var data []byte
	if a.File != "" {
		info, err := os.Stat(a.File)
		if err != nil {
			return r, err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAttach {
			return r, fmt.Errorf("file must be regular, nonempty and at most %d MiB", maxAttach>>20)
		}
		f, err := os.Open(a.File)
		if err != nil {
			return r, err
		}
		data, err = io.ReadAll(io.LimitReader(f, maxAttach+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return r, errors.Join(err, closeErr)
		}
		if len(data) == 0 || len(data) > maxAttach {
			return r, fmt.Errorf("file must be nonempty and at most %d MiB", maxAttach>>20)
		}
	}
	fingerprint, _ := json.Marshal([]string{a.Action, a.Channel, a.Thread, a.Text, filepath.Base(a.File), fmt.Sprintf("%x", sha256.Sum256(data))})
	r.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(fingerprint))
	identity := a.Key
	if identity == "" {
		identity = r.Fingerprint
	}
	r.Key = fmt.Sprintf("%x", sha256.Sum256([]byte(a.Request+"\x00"+identity)))
	path, err := slackReceiptPath(r.Key)
	if err != nil {
		return r, err
	}
	if err = os.MkdirAll(slackActionDir(), 0700); err != nil {
		return r, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		previous, readErr := os.ReadFile(path)
		if readErr != nil {
			return r, readErr
		}
		var cached slackReceipt
		if err = json.Unmarshal(previous, &cached); err != nil {
			return r, errors.New("operation state is incomplete; inspect Slack before retrying")
		}
		if cached.Fingerprint != r.Fingerprint || cached.Request != r.Request || cached.Key != r.Key {
			return r, errors.New("operation key was already used for a different payload")
		}
		cached.Cached = true
		if !cached.OK {
			return cached, fmt.Errorf("delivery is unconfirmed; inspect with: mainplane-chat slack receipt --key %s; do not post again blindly", cached.Key)
		}
		return cached, nil
	}
	if err != nil {
		return r, err
	}
	r.State = "pending"
	err = json.NewEncoder(f).Encode(r)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(err, syncErr, closeErr); err != nil {
		return r, err
	}
	// Record the file id before uploading so a lost response can be inspected
	// without repeating the write or storing a signed upload URL.
	if a.File != "" {
		upload, uploadErr := api.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{FileName: filepath.Base(a.File), FileSize: len(data)})
		if uploadErr != nil {
			return r, uploadErr
		}
		r.File = upload.FileID
		if err = saveSlackReceipt(path, r); err != nil {
			return r, err
		}
		if err = api.UploadToURL(ctx, slack.UploadToURLParameters{UploadURL: upload.UploadURL, Reader: bytes.NewReader(data), Filename: filepath.Base(a.File)}); err != nil {
			return r, err
		}
		_, err = api.CompleteUploadExternalContext(ctx, slack.CompleteUploadExternalParameters{Files: []slack.FileSummary{{ID: r.File, Title: filepath.Base(a.File)}}, Channel: a.Channel, ThreadTimestamp: a.Thread, InitialComment: a.Text})
	} else {
		opts := []slack.MsgOption{slack.MsgOptionText(a.Text, false), slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl()}
		if a.Thread != "" {
			opts = append(opts, slack.MsgOptionTS(a.Thread))
		}
		_, r.Message, err = api.PostMessageContext(ctx, a.Channel, opts...)
	}
	if err != nil {
		return r, fmt.Errorf("Slack send is unconfirmed; use receipt --key %s before retrying: %w", r.Key, err)
	}
	if err = saveSlackReceipt(path, r); err != nil {
		return r, err
	}
	err = confirmSlackAction(ctx, api, &r)
	if saveErr := saveSlackReceipt(path, r); saveErr != nil {
		err = errors.Join(err, saveErr)
	}
	return r, err
}

func confirmSlackAction(ctx context.Context, api *slack.Client, r *slackReceipt) error {
	if r.File != "" && r.Message == "" {
		for end := time.Now().Add(10 * time.Second); ; {
			info, _, _, err := api.GetFileInfoContext(ctx, r.File, 1, 1)
			if err != nil {
				return err
			}
			for _, shares := range []map[string][]slack.ShareFileInfo{info.Shares.Public, info.Shares.Private} {
				for _, share := range shares[r.Channel] {
					root := share.ThreadTs == "" || share.ThreadTs == share.Ts
					if r.Thread == "" && root || r.Thread != "" && share.ThreadTs == r.Thread {
						r.Message = share.Ts
						break
					}
				}
			}
			if r.Message != "" {
				break
			}
			if time.Now().After(end) {
				return fmt.Errorf("file %s was accepted but its requested Slack share is not confirmed; inspect this receipt, do not repost", r.File)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	if r.Message == "" {
		return errors.New("send may be in progress or its response was lost; inspect Slack before retrying with a new operation key")
	}
	link, err := api.GetPermalinkContext(ctx, &slack.PermalinkParameters{Channel: r.Channel, Ts: r.Message})
	if err != nil {
		return err
	}
	if link == "" {
		return errors.New("Slack returned no message permalink")
	}
	r.OK, r.State, r.Permalink, r.Error = true, "confirmed", link, ""
	return nil
}

func saveSlackReceipt(path string, r slackReceipt) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(r)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(err, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Trust only our persisted receipt for this request, never a tool's arbitrary
// JSON or a model's statement that it posted something.
func confirmedSlackReceipt(body []byte, session, request string) *slackReceipt {
	if request == "" {
		request = session
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		var hint slackReceipt
		if json.Unmarshal(line, &hint) != nil || !hint.OK {
			continue
		}
		path, err := slackReceiptPath(hint.Key)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var actual slackReceipt
		if json.Unmarshal(data, &actual) == nil && actual.OK && actual.Key == hint.Key && actual.State == "confirmed" && actual.Fingerprint != "" && actual.Request == request && actual.Permalink != "" {
			return &actual
		}
	}
	return nil
}
