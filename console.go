package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// consoleThread is the console's one thread.
const consoleThread = "console:try"

// console is a chat in the terminal, to try a build against the real harness
// before deploying it: each stdin line is a message in one thread, and what
// the bot would post there is printed, its status titled as in Slack.
type console struct {
	mu   sync.Mutex
	out  io.Writer
	last string // the status last printed: each change prints once
}

func (c *console) print(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(c.out, s)
}

func (c *console) say(th string, m msg) {
	lines := []string{"say: " + m.Text}
	if m.Note != "" {
		lines = append(lines, "  note: "+m.Note)
	}
	for _, f := range m.Files {
		lines = append(lines, "  file: "+f.name)
	}
	if m.Retry {
		lines = append(lines, "  [Retry]")
	}
	if len(m.Models) > 0 {
		var names []string
		for _, md := range m.Models {
			names = append(names, md.ID+map[bool]string{true: " (current)"}[md.Current])
		}
		lines = append(lines, "  [models: "+strings.Join(names, ", ")+"]")
	}
	c.print(strings.Join(lines, "\n"))
}

func (c *console) status(th, id string, t *turn) {
	if t.done && t.status == "" {
		return // as in Slack: a turn with no calls has no status message
	}
	t.status = "console" // the status message exists from here on, as Slack's does once posted
	s := consoleStatus(id, t)
	c.mu.Lock()
	same := s == c.last
	c.last = s
	c.mu.Unlock()
	if !same {
		c.print(s)
	}
}

// consoleStatus is a turn's status as Slack shows it: the title, then each
// call with its plan status.
func consoleStatus(id string, t *turn) string {
	s := "status: " + planTitle(id, t)
	for _, k := range t.tasks {
		s += "\n  " + string(taskStatus(t, k)) + " " + k.title
	}
	return s
}

func (c *console) react(th, msgID, emoji string) {
	if msgID == "" {
		msgID = "thread" // the thread itself, as Slack's agent status shows it
	}
	c.print("react " + msgID + ": " + emoji)
}

func (c *console) rename(th, id, name string) { c.print("rename: " + name) }

func (c *console) close(th string) { c.print("closed") }

// runConsole sends each stdin line to the console thread, as a person would in
// a chat: the first starts a session, unless session names one to show, and
// the rest are messages and thread commands. At the end of the input it waits
// up to wait for the session to stop working, so its replies print.
func runConsole(b *bridge, session string, wait time.Duration) {
	c := &console{out: os.Stdout}
	b.chats = map[string]chat{"console": c}
	if session != "" {
		i, err := b.info(session)
		if err != nil {
			log.Fatalf("session %s: %v", session, err)
		}
		b.bind(consoleThread, session, i.N)
		b.follow(consoleThread, session, i.N) // what the session does next prints too
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; in.Scan(); n++ {
		text := strings.TrimSpace(in.Text())
		if text == "" {
			continue
		}
		key := fmt.Sprintf("console-%d-%d", time.Now().UnixNano(), n)
		if id := b.sessionFor(consoleThread); id != "" {
			b.message(consoleThread, id, key, text, nil)
			continue
		}
		open := func(string) (string, error) { return consoleThread, nil }
		if err := b.start("console", open, key, "", text, nil); err != nil {
			c.say(consoleThread, msg{Text: err.Error()})
		}
	}
	if err := in.Err(); err != nil {
		log.Fatalf("console: reading input: %v", err) // a cut-off input must not pass for a complete try
	}
	for end := time.Now().Add(wait); ; time.Sleep(time.Second) {
		id := b.sessionFor(consoleThread)
		if id == "" {
			return
		}
		if i, err := b.info(id); err != nil || i.Status != "stepping" && i.Status != "open" {
			time.Sleep(3 * time.Second) // the follower relays the end of the turn
			return
		}
		if time.Now().After(end) {
			c.print(fmt.Sprintf("console: session %s still working after %s", id, wait))
			return
		}
	}
}
