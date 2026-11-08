package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

// state is kept across restarts, so the bot picks up what happened while it
// was down. Its keys are "<platform>:<id>", as "discord:1565949504866222130".
type state struct {
	Seen    map[string]string `json:"seen"`    // channel or thread -> newest message handled there
	Threads map[string]thread `json:"threads"` // thread -> what it shows
	// MirrorSince is when mirroring began: sessions made outside the chats
	// after it get a thread in #sessions; older ones do not.
	MirrorSince time.Time `json:"mirror_since"`
}

type thread struct {
	Session            string   `json:"session"`
	N                  int      `json:"n"` // the newest record relayed
	AttachmentPending  bool     `json:"attachment_pending,omitempty"`
	AttachmentRecovery bool     `json:"attachment_recovery,omitempty"`
	Attaching          []string `json:"attaching,omitempty"`      // run calls that printed an attach directive, their results not yet seen
	Sending            []string `json:"sending,omitempty"`        // run calls that ran the Slack sender, their results not yet seen
	ActionReceipt      string   `json:"action_receipt,omitempty"` // a confirmed sender action of the open turn
	ActionFailed       bool     `json:"action_failed,omitempty"`  // a sender action of the open turn returned no receipt
	Request            string   `json:"request,omitempty"`        // latest genuine chat request; stable through auto-continuation
	Sent               []string `json:"sent,omitempty"`           // newest chat message keys posted to this session, so a restart knows its own
}

// sentKeys is how many posted message keys a thread remembers: a restart
// must still recognize the messages not relayed yet, without the state
// growing with every message ever sent.
const sentKeys = 20

func (b *bridge) threadsShowing(id string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ts []string
	for k, t := range b.st.Threads {
		if _, on := b.chats[platform(k)]; on && t.Session == id {
			ts = append(ts, k)
		}
	}
	return ts
}

func (b *bridge) shows(th, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st.Threads[th].Session == id
}

// sessionFor is the session the state says a thread shows, or "".
func (b *bridge) sessionFor(th string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st.Threads[th].Session
}

// bind makes thread th show session id, relayed up to record n; a thread
// that shows it already keeps its newer n.
func (b *bridge) bind(th, id string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.st.Threads[th]; t.Session != id || n > t.N {
		if t.Session != id {
			t = thread{Session: id, Request: t.Request}
		}
		t.N = n
		b.st.Threads[th] = t
		b.save()
	}
}

// advance records that session id is relayed up to record n, if th still
// shows it.
func (b *bridge) advance(th, id string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.st.Threads[th]; t.Session == id && n > t.N {
		t.N = n
		b.st.Threads[th] = t
		b.save()
	}
}

// forget drops a thread whose session is gone.
func (b *bridge) forget(th string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.st.Threads, th)
	delete(b.st.Seen, th)
	b.save()
}

// lastSeen is the newest message handled in channel ch, if any was.
func (b *bridge) lastSeen(ch string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.st.Seen[ch]
	return id, ok
}

// unseen reports whether message id is newer than every one handled in
// channel ch. mark says one was handled.
func (b *bridge) unseen(ch, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return newer(id, b.st.Seen[ch])
}

// mark records message id as the newest message handled in channel ch. A
// message the bridge could not deliver is not marked, so catch-up after a
// restart reads it again; one delivered after it buries it either way.
func (b *bridge) mark(ch, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !newer(id, b.st.Seen[ch]) {
		return
	}
	b.st.Seen[ch] = id
	b.save()
}

// newer compares message ids, which grow with time.
func newer(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

func statePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "mainplane-chat", "state.json")
}

func (b *bridge) load() {
	data, err := os.ReadFile(b.path)
	if err == nil {
		err = json.Unmarshal(data, &b.st)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("state: %v", err)
	}
	if b.st.Seen == nil {
		b.st.Seen = map[string]string{}
	}
	if b.st.Threads == nil {
		b.st.Threads = map[string]thread{}
	}
	for th, t := range b.st.Threads {
		for _, k := range t.Sent {
			b.sentIn[k] = th
		}
	}
	if b.st.MirrorSince.IsZero() {
		b.st.MirrorSince = time.Now().UTC()
		b.save()
	}
}

// save writes the state; b.mu is held.
func (b *bridge) save() {
	data, err := json.Marshal(b.st)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(b.path), 0o700)
	}
	if err == nil {
		err = os.WriteFile(b.path+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(b.path+".tmp", b.path)
	}
	if err != nil {
		log.Printf("state: %v", err)
	}
}
