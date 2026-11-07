// mainplane-chat drives Mainplane agents from Discord and Slack, the way the
// mainplane terminal app does. A message in a channel or DM starts a session
// in a new thread; messages in that thread continue it, and the agent's
// replies come back there. A thread takes the terminal app's /model,
// /continue and /stop, and the CLI's /retry and /info (in Slack, as !model
// and so on). What happens while the bot is down is picked up when it comes
// back.
//
// This file wires the chats up and takes messages in: thread commands,
// starting sessions, posting to them. Each concern the messages flow through
// has its own file: harness.go (the harness API), state.go (what a thread
// shows, kept across restarts), turn.go (following a session and showing its
// turns), watch.go (sessions made anywhere), continuation.go (carrying a
// full session on), delivery.go (checked file delivery), models.go (a new
// session's model), and one adapter per chat (discord.go, slack.go,
// console.go).
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/catalog"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

type bridge struct {
	harness string // base URL of the harness API
	key     string
	path    string          // the state file
	dc      *discord        // for mirroring, which only Discord does
	chats   map[string]chat // platform -> its adapter; the platform is also the via of its messages

	mu        sync.Mutex
	following map[[2]string]bool  // thread and session pairs with a follower running
	full      map[string]bool     // full sessions a thread has continued
	carried   map[string][]string // thread -> messages of continued turns, marked when its next turn ends
	sentIn    map[string]string   // chat message -> the thread that sent it to a session
	renames   map[string]*title   // thread -> the title it waits for, while its renamer runs
	busy      map[string]bool     // session activity, refreshed by watchSessions; status never does I/O
	st        state
}

// chat is what the core needs of a chat app. th is a thread's key.
type chat interface {
	say(th string, m msg) error
	// status posts or edits the turn's status message; t.done shrinks it, and
	// comes at the end of every turn, with or without a status message.
	status(th, id string, t *turn)
	// react marks message msgID, or the thread itself when msgID is "".
	react(th, msgID, emoji string)
	rename(th, id, name string)
	// close shows that the thread's session is gone.
	close(th string)
}

// msg is what the core says in a thread; the adapter formats it.
type msg struct {
	Text    string
	Note    string // lines shown small, under Text
	ReplyTo string // the message it answers
	Notify  bool   // notify ReplyTo's author
	Files   []file
	Session string  // the session Retry and Models act on
	Retry   bool    // a Retry button: /retry
	Models  []model // a model picker: /model with the one picked
}

type model struct {
	ID, Name string
	Current  bool
}

// file is a file sent with a message, fetched only when needed.
type file struct {
	name, ctype string
	size        int
	fetch       func() ([]byte, error)
	topLevel    bool // Slack: share in the channel instead of this thread
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "slack" {
		if err := runSlackCLI(os.Args[2:], os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	try := flag.Bool("console", false, "try this build in the terminal against the harness: stdin lines are messages in one thread, and what the bot would post is printed; no chat tokens needed, and the bot's state is not touched")
	session := flag.String("session", "", "with -console: show this session in the thread instead of starting one")
	wait := flag.Duration("wait", 10*time.Minute, "with -console: how long to wait at the end of input for the session to stop working")
	slackHelp := flag.Bool("slack-help", false, "show executable Slack sender commands without starting the service")
	flag.Parse()
	if *slackHelp {
		runSlackCLI([]string{"help"}, os.Stdout)
		return
	}
	b := &bridge{
		harness:   envOr("MAINPLANE_URL", "http://127.0.0.1:8080"),
		key:       secret(mustEnv("MAINPLANE_KEY")),
		path:      statePath(),
		following: map[[2]string]bool{},
		full:      map[string]bool{},
		carried:   map[string][]string{},
		sentIn:    map[string]string{},
		renames:   map[string]*title{},
	}
	if *try {
		dir, err := os.MkdirTemp("", "mainplane-chat-console")
		if err != nil {
			log.Fatal(err)
		}
		defer os.RemoveAll(dir)
		b.path = filepath.Join(dir, "state.json")
		b.load()
		runConsole(b, *session, *wait)
		return
	}
	b.load()
	b.chats = map[string]chat{}
	var sl *slackChat
	if token := os.Getenv("DISCORD_TOKEN"); token != "" {
		b.dc = newDiscord(b, token, os.Getenv("MAINPLANE_DISCORD_TESTER"))
		b.chats["discord"] = b.dc
	}
	if token := os.Getenv("SLACK_BOT_TOKEN"); token != "" {
		sl = newSlack(b, token, mustEnv("SLACK_APP_TOKEN"), mustEnv("MAINPLANE_SLACK_USERS"))
		b.chats["slack"] = sl
	}
	if len(b.chats) == 0 {
		log.Fatal("set DISCORD_TOKEN, SLACK_BOT_TOKEN or both")
	}
	// After b.chats is set, as catching up starts followers.
	if b.dc != nil {
		b.dc.open()
		defer b.dc.dc.Close()
	}
	if sl != nil {
		sl.open()
	}
	go b.watchSessions()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}

// platform is the chat app of a thread or channel key, which is also the via
// of the messages sent from it.
func platform(key string) string {
	p, _, _ := strings.Cut(key, ":")
	return p
}

func (b *bridge) chat(th string) chat { return b.chats[platform(th)] }

func (b *bridge) say(th string, m msg) error { return b.chat(th).say(th, m) }

// message handles a message sent in thread th, which shows session id: a
// thread command, else a message to the session.
func (b *bridge) message(th, id, msgID, text string, files []file) {
	if !b.command(th, id, msgID, text) {
		b.post(th, id, msgID, "", text, files)
	}
}

// command runs a thread command and reports whether text was one. /model and
// /continue take a message for the session they make.
func (b *bridge) command(th, id, msgID, text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	word := fields[0]
	if len(word) < 2 || word[0] != '/' && word[0] != '!' {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, word))
	var err error
	switch word[1:] {
	case "stop":
		err = b.call("POST", "/sessions/"+id+"/stop?via="+platform(th), "", nil, nil)
	case "retry":
		// An explicit Retry after downtime must recover a known full
		// session too, rather than growing the same rejected history again.
		if recs, readErr := b.records(id, 0, ""); readErr == nil && len(recs) > 0 {
			r := recs[len(recs)-1]
			if r.Kind == statefile.Error && r.Via == "" && b.endedFull(id, string(r.Body)) {
				b.continueFull(th, id)
				break
			}
		}
		if err = b.call("POST", "/sessions/"+id+"/retry?via="+platform(th), "", nil, nil); err == nil {
			b.resume(th, id)
		}
	case "info":
		err = b.sayInfo(th, id)
	case "delete":
		// As the CLI's delete: the session is gone, and the thread shows it.
		if err = b.call("DELETE", "/sessions/"+id+"?via="+platform(th), "", nil, nil); err == nil {
			b.forget(th)
			b.chat(th).close(th)
		}
	case "continue":
		_, err = b.continueIn(th, id, msgID, "", rest)
	case "model":
		if rest == "" {
			err = b.listModels(th, id)
			break
		}
		model := strings.Fields(rest)[0]
		_, err = b.continueIn(th, id, msgID, model, strings.TrimSpace(strings.TrimPrefix(rest, model)))
	case "busy":
		err = b.sayBusy(th, id)
	default:
		return false
	}
	if err != nil {
		b.say(th, msg{Text: word + " failed: " + err.Error()})
	}
	return true
}

// start makes a session for a message in a channel, with the config newConf
// picks; open makes the message's thread for the session and returns its key.
// prefix is as for post.
func (b *bridge) start(via string, open func(id string) (string, error), msgID, prefix, text string, files []file) error {
	conf, err := b.newConf()
	if err != nil {
		return fmt.Errorf("could not start a session: %w", err)
	}
	id, err := b.create(conf, via)
	if err != nil {
		return fmt.Errorf("could not start a session: %w", err)
	}
	th, err := open(id)
	if err != nil {
		return fmt.Errorf("session %s started, but the thread failed: %w", id, err)
	}
	b.bind(th, id, 0)
	b.post(th, id, msgID, prefix, text, files)
	return nil
}

// newConf is the config of a new session (choices.startConf, from startBase),
// with every connected worker.
func (b *bridge) newConf() (statefile.Conf, error) {
	ch, recent, err := b.choices()
	if err != nil {
		return statefile.Conf{}, err
	}
	b.mu.Lock()
	var shown []string
	for _, t := range b.st.Threads {
		shown = append(shown, t.Session)
	}
	b.mu.Unlock()
	conf, err := ch.startConf(startBase(recent, shown))
	if err != nil {
		return conf, err
	}
	conf.Workers, err = b.workers()
	return conf, err
}

// listModels is /model alone: a picker of the connected models.
func (b *bridge) listModels(th, id string) error {
	ms, err := b.modelList(id)
	if err != nil {
		return err
	}
	b.say(th, msg{Note: "continue in a new session on another model, or `!model <id> [message]`", Session: id, Models: ms})
	return nil
}

// modelList is the connected models, session id's first, so that a picker
// that shows only some shows it.
func (b *bridge) modelList(id string) ([]model, error) {
	ch, _, err := b.choices()
	if err != nil {
		return nil, err
	}
	cur, _ := b.info(id)
	ms := ch.list(cur.Config)
	if len(ms) == 0 {
		return nil, errNoModel
	}
	return ms, nil
}

func (b *bridge) sayInfo(th, id string) error {
	i, err := b.info(id)
	if err != nil {
		return err
	}
	var names []string
	for _, w := range i.Config.Workers {
		names = append(names, w.Name)
	}
	b.say(th, msg{Text: fmt.Sprintf("session `%s` %s\nmodel `%s`\nworkers %s\ncontext %d/%d",
		i.ID, i.Status, i.Config.Model, strings.Join(names, ", "), i.ContextUsed, i.Config.ContextLimit)})
	return nil
}

// choices is what sessions can run on now: the catalog, the providers the
// harness serves, and the models of the newest 20 sessions; without the
// catalog, those models still are. It also returns the newest 100 sessions,
// for startBase to find the newest one a thread shows.
func (b *bridge) choices() (choices, []sessionInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cat, err := catalog.Fetch(ctx)
	if err != nil {
		log.Printf("catalog: %v", err)
	}
	var ps []struct{ Name string }
	if err := b.call("GET", "/providers", "", nil, &ps); err != nil {
		return choices{}, nil, err
	}
	var recent []sessionInfo
	if err := b.call("GET", "/sessions?limit=100", "", nil, &recent); err != nil {
		return choices{}, nil, err
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	var confs []statefile.Conf
	for _, s := range recent[:min(len(recent), 20)] {
		confs = append(confs, s.Config)
	}
	return newChoices(cat, names, confs), recent, nil
}

// workers is every connected worker.
func (b *bridge) workers() ([]statefile.Worker, error) {
	var ws []struct{ Name, Refused string }
	if err := b.call("GET", "/workers", "", nil, &ws); err != nil {
		return nil, err
	}
	var all []statefile.Worker
	for _, w := range ws {
		if w.Refused == "" {
			all = append(all, statefile.Worker{Name: w.Name})
		}
	}
	return all, nil
}

// post sends a chat message's text and files to the session, keyed by the
// message so a repeat appends nothing, and makes sure the thread follows the
// session. Each part is a record: the text, then each image. prefix (who wrote
// it, from a chat that says) goes before the text only when the message has
// something to send, so a message whose files were all skipped sends nothing.
func (b *bridge) post(th, id, msgID, prefix, text string, files []file) error {
	note := sessionContext(th, id)
	if strings.HasPrefix(text, continuePrefix) {
		// The first line must still identify the predecessor for the watcher.
		first, rest, _ := strings.Cut(text, "\n")
		text = first + "\n\n" + note + "\n" + rest
	} else {
		prefix = note + "\n\n" + prefix
	}
	parts, skipped := b.parts(id, prefix, text, files)
	if len(skipped) > 0 {
		b.say(th, msg{Note: "not sent: " + strings.Join(skipped, ", ")})
	}
	if len(parts) == 0 {
		return nil
	}
	if platform(th) == "slack" && isTS(msgID) && !bridgeMessage(msgID) && !strings.HasPrefix(text, continuePrefix) {
		b.mu.Lock()
		if current, ok := b.st.Threads[th]; ok {
			current.Request = msgID
			b.st.Threads[th] = current
			b.save()
		}
		b.mu.Unlock()
	}
	var body bytes.Buffer
	ctype := "text/plain"
	if len(parts) == 1 && parts[0].ctype == ctype {
		body.Write(parts[0].body)
	} else {
		mw := multipart.NewWriter(&body)
		for _, p := range parts {
			w, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {p.ctype}})
			w.Write(p.body)
		}
		mw.Close()
		ctype = mw.FormDataContentType()
	}
	req, _ := http.NewRequest("POST", b.harness+"/sessions/"+id+"/records?via="+platform(th), &body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Idempotency-Key", msgID)
	b.mu.Lock()
	b.sentIn[msgID] = th // before the send, as the thread's follower may relay it first
	b.mu.Unlock()
	var out struct{ N int }
	if err := b.do(req, &out); err != nil {
		// The harness may have accepted the idempotent message before the
		// connection/response failed. Reattach instead of creating duplicates.
		if recs, lookupErr := b.records(id, 0, ""); lookupErr == nil {
			first, accepted := 0, 0
			for _, r := range recs {
				if r.Kind == statefile.Message && msgID != "" && r.IdempotencyKey == msgID {
					if first == 0 {
						first = r.N
					}
					accepted++
				}
			}
			if accepted >= len(parts) {
				if !bridgeMessage(msgID) {
					b.chat(th).react(th, msgID, seen)
				}
				b.follow(th, id, first-1)
				return nil
			}
		}
		b.say(th, msg{Text: "send failed: " + err.Error()})
		return err
	}
	if !bridgeMessage(msgID) {
		b.chat(th).react(th, msgID, seen)
	}
	b.follow(th, id, out.N-len(parts)) // from the message itself, the reply's target
	return nil
}

type part struct {
	ctype string
	body  []byte
}

// parts is what a message sends: its text, with text files inlined, then the
// images the session's model takes. The rest are skipped, by file name.
func (b *bridge) parts(id, prefix, text string, files []file) ([]part, []string) {
	var input []string
	if len(files) > 0 {
		if i, err := b.info(id); err == nil {
			input = i.Config.Input
		}
	}
	var images []part
	var skipped []string
	for _, f := range files {
		ctype, _, _ := mime.ParseMediaType(f.ctype)
		image := slices.Contains(input, ctype) && f.size <= 5<<20
		textual := (strings.HasPrefix(ctype, "text/") || ctype == "application/json") && f.size <= 200<<10
		var data []byte
		var err error
		if image || textual {
			data, err = f.fetch()
		}
		switch {
		case err != nil:
			skipped = append(skipped, f.name+" ("+err.Error()+")")
		case image:
			images = append(images, part{ctype, data})
		case textual:
			text += "\n\n" + f.name + ":\n```\n" + strings.TrimRight(string(data), "\n") + "\n```"
		default:
			skipped = append(skipped, f.name)
		}
	}
	text = strings.TrimSpace(text)
	if text == "" && len(images) == 0 {
		return nil, skipped
	}
	var ps []part
	if text = strings.TrimSpace(prefix + text); text != "" {
		ps = append(ps, part{"text/plain", []byte(text)})
	}
	return append(ps, images...), skipped
}

// secret is the bearer part of a harness token, mp_key_<harness>.<secret>; a
// bare secret passes through.
func secret(token string) string {
	if _, s, ok := strings.Cut(token, "."); ok && strings.HasPrefix(token, "mp_key_") {
		return s
	}
	return token
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is not set", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
