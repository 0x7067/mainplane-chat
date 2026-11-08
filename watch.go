package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// active is whether a session is working: stepping, waiting on its next
// step, or with a background job still outstanding.
func active(s sessionInfo, jobs int) bool {
	return s.Status == statefile.StatusStepping || s.Status == statefile.StatusOpen || jobs > 0
}

// continued is the session a first message says it continues, or "".
// watchSessions keeps the threads in step with sessions made anywhere. A
// session that continues one a thread shows moves that thread to it; one made
// outside the chats after mirroring began gets its own thread in #sessions,
// unless a thread shows it or it was continued. Mainplane has no event for a
// new session, so this reads the list every few seconds, as the terminal
// app's sidebar does. A session is checked once it has its first message;
// only sessions made in the last day are.
func (b *bridge) watchSessions() {
	checked := map[string]bool{}
	jobCounts := map[string]struct{ n, count int }{}
	// Sessions another continues: their thread, if any, has moved on. The list
	// is newest first, so a continuation is read before the session it continues.
	moved := map[string]bool{}
	for ; ; time.Sleep(3 * time.Second) {
		var infos []sessionInfo
		if err := b.call("GET", "/sessions?limit=100", "", nil, &infos); err != nil {
			log.Printf("sessions: %v", err)
			continue
		}
		busy := map[string]bool{}
		present := map[string]bool{}
		for _, in := range infos {
			present[in.ID] = true
			cached, ok := jobCounts[in.ID]
			if !ok || cached.n != in.N {
				if count, err := b.countBackgroundJobs(in.ID); err == nil {
					cached.n, cached.count = in.N, count
					jobCounts[in.ID] = cached
				} else {
					log.Printf("jobs %s: %v", in.ID, err)
				}
			}
			busy[in.ID] = active(in, cached.count)
		}
		for id := range jobCounts {
			if !present[id] {
				delete(jobCounts, id)
			}
		}
		b.mu.Lock()
		b.busy = busy
		b.mu.Unlock()
		for _, in := range infos {
			if checked[in.ID] || time.Since(in.Created) > 24*time.Hour {
				continue
			}
			recs, err := b.records(in.ID, 0, "")
			if err != nil {
				continue
			}
			first, from, ok := firstMessage(recs)
			if !ok {
				continue
			}
			old := continued(first)
			if old != "" {
				moved[old] = true
			}
			if len(b.threadsShowing(in.ID)) > 0 {
				checked[in.ID] = true
				continue
			}
			if threads := b.threadsShowing(old); old != "" && len(threads) > 0 {
				checked[in.ID] = true
				for _, th := range threads {
					b.follow(th, in.ID, 0)
					b.say(th, msg{Note: "continued in session " + in.ID})
				}
				continue
			}
			b.mu.Lock()
			since := b.st.MirrorSince
			b.mu.Unlock()
			if b.dc == nil || from == "discord" || from == "slack" || from == "console" || moved[in.ID] || !in.Created.After(since) {
				checked[in.ID] = true
				continue
			}
			th, err := b.dc.mirror(in.ID, from, in.Config.Model, first)
			if errors.Is(err, errNoSessions) {
				continue // check again once #sessions is found
			}
			checked[in.ID] = true
			if err != nil {
				log.Printf("mirror %s: %v", in.ID, err)
				continue
			}
			b.follow(th, in.ID, 0)
		}
	}
}

// firstMessage is the session's first message and who sent it, if it has one.
func firstMessage(recs []statefile.Record) (body, via string, ok bool) {
	for _, r := range recs {
		if r.Kind == statefile.Message {
			return string(r.Body), r.Via, true
		}
	}
	return "", "", false
}

// busyNow is whether the watcher last saw session id working.
func (b *bridge) busyNow(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busy[id]
}

// sayBusy is !busy: the other sessions that are working, who started them
// and what they are running on.
func (b *bridge) sayBusy(th, id string) error {
	var infos []sessionInfo
	if err := b.call("GET", "/sessions?limit=100", "", nil, &infos); err != nil {
		return fmt.Errorf("could not list sessions: %w", err)
	}
	var lines []string
	for _, in := range infos {
		if in.ID == id || !b.busyNow(in.ID) {
			continue
		}
		recs, err := b.records(in.ID, 0, "")
		if err != nil {
			return fmt.Errorf("read %s: %w", in.ID, err)
		}
		background := runningBackgroundJobs(recs)
		var calls int
		for _, r := range recs {
			if r.Kind == statefile.Call {
				calls++
			}
		}
		var author string
		if body, via, ok := firstMessage(recs); ok {
			author = via
			if via == "slack" {
				author = slackAuthor(body)
			}
		}
		var parts []string
		parts = append(parts, "session `"+in.ID+"`")
		if in.Status == statefile.StatusStepping {
			parts = append(parts, "**stepping**")
		} else {
			parts = append(parts, string(in.Status))
		}
		if in.Name != "" {
			parts = append(parts, "| **"+in.Name+"**")
		}
		parts = append(parts, "on `"+in.Config.Model+"`")
		parts = append(parts, "· started "+time.Since(in.Created).Round(time.Second).String())
		if calls > 0 {
			parts = append(parts, strconv.Itoa(calls)+" calls")
		}
		if background > 0 {
			parts = append(parts, strconv.Itoa(background)+" background jobs")
		}
		line := strings.Join(parts, " ")
		if author != "" {
			line += " · by " + author
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		b.say(th, msg{Text: "No other sessions are currently running or have background jobs."})
		return nil
	}
	b.say(th, msg{Text: "**Other busy sessions** (" + strconv.Itoa(len(lines)) + "):\n\n" + strings.Join(lines, "\n")})
	return nil
}

// countBackgroundJobs counts how many background jobs are running in a session.
func (b *bridge) countBackgroundJobs(id string) (int, error) {
	recs, err := b.records(id, 0, "")
	if err != nil {
		return 0, err
	}
	return runningBackgroundJobs(recs), nil
}

// Match the harness's job lifecycle, not the lifetime count of background
// run calls: only an acknowledged start with no report/stop is still running.
func runningBackgroundJobs(recs []statefile.Record) int {
	background, running, reported := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range recs {
		switch {
		case r.Kind == statefile.Call:
			var c struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if json.Unmarshal(r.Body, &c) != nil {
				continue
			}
			if c.Name == "run" {
				if bg, ok := c.Arguments["background"].(bool); ok && bg {
					background[r.ID] = true
				}
			}
		case r.Kind == statefile.Result && background[r.For] && !reported[r.For] && strings.HasPrefix(string(r.Body), "started job "):
			running[r.For] = true
		case r.Kind == statefile.Message && r.Via == "job" || r.Kind == statefile.System && r.Source == "job":
			reported[r.IdempotencyKey] = true
			delete(running, r.IdempotencyKey)
		case r.Kind == statefile.Error && r.Via != "" || r.Kind == statefile.System && r.Source == "stop":
			clear(background)
			clear(running)
		}
	}
	return len(running)
}

// busyCount is a cheap snapshot read: status runs every three seconds per
// thread and must never make harness calls or reread session histories.
func (b *bridge) busyCount(current string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for id, active := range b.busy {
		if id != current && active {
			count++
		}
	}
	return count
}
