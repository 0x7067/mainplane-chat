// mainplane-chat drives Mainplane agents from Discord and Slack, the way the
// mainplane terminal app does. A message in a channel or DM starts a session
// in a new thread; messages in that thread continue it, and the agent's
// replies come back there. A thread takes the terminal app's /model,
// /continue and /stop, and the CLI's /retry and /info (in Slack, as !model
// and so on). What happens while the bot is down is picked up when it comes
// back.
//
// This file is the core: the harness client, following sessions, commands,
// continuations and the state. Each chat app is an adapter (discord.go, slack.go) that
// turns its events into calls of the core and shows what the core says.
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mainplane-ai/mainplane/pkg/catalog"
	"github.com/mainplane-ai/mainplane/pkg/provider"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// The catalog groups the terminal app reads: the models it offers, and the
// ones a new session takes when the newest session's is not connected.
const (
	group    = "self-hosted-all"
	defaults = "self-hosted-default"
)

// maxAttach is the largest file an agent's "attach: /path" sends: Discord's
// upload limit, which is below Slack's.
const maxAttach = 20 << 20

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
	Session            string `json:"session"`
	N                  int    `json:"n"` // the newest record relayed
	AttachmentPending  bool   `json:"attachment_pending,omitempty"`
	AttachmentRecovery bool   `json:"attachment_recovery,omitempty"`
	Request            string `json:"request,omitempty"` // latest genuine chat request; stable through auto-continuation
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

type info struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Config      statefile.Conf `json:"config"`
	N           int            `json:"n"`
	ContextUsed int            `json:"context_used"`
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

// continueIn is /continue, and /model with a model: a new session with this
// one's config, or the model's and this one's workers, told to carry on from
// this one, as the terminal app does. The thread, and every other thread that
// showed this one, then show the new session, from before its first message,
// so the watch does not move them again.
func (b *bridge) continueIn(th, old, msgID, model, text string) (string, error) {
	cur, err := b.info(old)
	if err != nil {
		return "", err
	}
	conf := cur.Config
	if model != "" {
		ch, _, err := b.choices()
		if err != nil {
			return "", err
		}
		if conf, err = ch.switchConf(cur.Config, model); err != nil {
			return "", err
		}
	}
	recs, err := b.records(old, 0, "")
	if err != nil {
		return "", fmt.Errorf("read continuation context: %w", err)
	}
	// Upgrade old pointer-only continuations too. Read their predecessor's
	// snapshot, not messages sent to it after this conversation had moved on.
	contextRecords, ancestor := recs, recs
	visited := map[string]bool{old: true}
	for range 3 {
		var first *statefile.Record
		for i := range ancestor {
			if ancestor[i].Kind == statefile.Message {
				first = &ancestor[i]
				break
			}
		}
		if first == nil || strings.Contains(string(first.Body), "Current requests:\n") {
			break
		}
		previous := continued(string(first.Body))
		if previous == "" || visited[previous] {
			break
		}
		visited[previous] = true
		previousRecords, readErr := b.records(previous, 0, "")
		if readErr != nil {
			log.Printf("older continuation %s: %v", previous, readErr)
			break
		}
		ancestor = slices.DeleteFunc(previousRecords, func(r statefile.Record) bool { return r.Time.After(first.Time) })
		contextRecords = append(slices.Clone(ancestor), contextRecords...)
	}
	m := continuePrefix + old + ". Continue the task using the handoff below; retrieve older context only if a specific fact is missing."
	m += "\n\n" + continuationContext(contextRecords)
	if paths := b.logPaths(old, cur.Config); len(paths) > 0 {
		m += "\nOlder records (only if needed): " + strings.Join(paths, ", ") + ". " + logFormatNote
	}
	if text != "" {
		m += "\n\nContinuation instruction:\n" + text
	}
	id, err := b.create(conf, platform(th))
	if err != nil {
		return "", err
	}
	b.say(th, msg{Note: "continuing in session " + id + " on " + conf.Model})
	others := b.threadsShowing(old)
	if err := b.post(th, id, msgID, "", m, nil); err != nil {
		// A lost response is not proof that the send failed. Delete only a
		// session confirmed empty; never destroy potentially accepted work.
		if pending, readErr := b.records(id, 0, ""); readErr == nil && !slices.ContainsFunc(pending, func(r statefile.Record) bool { return r.Kind == statefile.Message }) {
			if cleanup := b.call("DELETE", "/sessions/"+id, "", nil, nil); cleanup != nil {
				log.Printf("remove empty continuation %s: %v", id, cleanup)
			}
		} else {
			b.say(th, msg{Note: "Could not confirm delivery to session " + id + "; preserved it for inspection. The thread still follows " + old + "."})
		}
		return "", err
	}
	for _, o := range others {
		if o == th {
			continue
		}
		b.follow(o, id, 0)
		b.say(o, msg{Note: "continued in session " + id})
	}
	return id, nil
}

// contextFull is whether an error record is the harness ending a session at
// its context limit: "context 250009 exceeds limit 250000".
func contextFull(body string) bool {
	return strings.HasPrefix(body, "context ") && strings.Contains(body, " exceeds limit ")
}

// providerFull is whether an error record is the provider refusing a request
// longer than its context window (llama-server: exceed_context_size_error;
// OpenAI: context_length_exceeded). When that window is no larger than the
// session's limit, the provider refuses before the harness ends the session.
func providerFull(body string) bool {
	return strings.Contains(body, "exceed_context_size_error") || strings.Contains(body, "context_length_exceeded")
}

// overflowSizes are the request's tokens and the provider's window from its
// refusal: llama-server's "n_prompt_tokens":N and "n_ctx":N, or OpenAI's
// "maximum context length is N tokens ... resulted in (or you requested) N tokens". Zero when
// the body does not say.
var (
	llamaPrompt = regexp.MustCompile(`"n_prompt_tokens"\s*:\s*(\d+)`)
	llamaCtx    = regexp.MustCompile(`"n_ctx"\s*:\s*(\d+)`)
	openaiCtx   = regexp.MustCompile(`maximum context length is (\d+) tokens`)
	openaiReq   = regexp.MustCompile(`(?:resulted in|you requested) (\d+) tokens`)
)

func overflowSizes(body string) (request, window int) {
	num := func(re *regexp.Regexp) int {
		if m := re.FindStringSubmatch(body); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	return cmp.Or(num(llamaPrompt), num(openaiReq)), cmp.Or(num(llamaCtx), num(openaiCtx))
}

// historyOverflow is whether a provider's refusal came from the session's
// history rather than from what was just added: what the request adds over
// the context the session last used is under half the window, so a new
// session, which starts with only that, fits. One oversized message, at any
// point in a session, keeps Retry instead of failing again in a continuation.
// Without the sizes, it is not a history overflow.
func historyOverflow(body string, used int) bool {
	request, window := overflowSizes(body)
	return request > 0 && window > 0 && (request-used)*2 < window
}

// fresh is whether an error is recent enough to continue its session: one
// replayed when the bot comes back, say after a restart, may be long settled,
// and continuing it then restarts work nobody is waiting for.
func fresh(at, now time.Time) bool {
	return now.Sub(at) < 10*time.Minute
}

// endedFull is whether a session's error record ends it full, so the bot continues it.
func (b *bridge) endedFull(id, body string) bool {
	if contextFull(body) {
		return true
	}
	lengthStop := strings.HasPrefix(body, `The last step ended with stop "length"`) || strings.HasPrefix(body, `The last step ended with stop "max_tokens"`)
	if !providerFull(body) && !lengthStop {
		return false
	}
	i, err := b.info(id)
	if err != nil {
		return false
	}
	// ContextUsed counts only the prompt. A long thinking/reply block from
	// that step is now history too, even when no later step succeeded.
	recs, err := b.records(id, 0, "")
	if err != nil {
		return false
	}
	used := i.ContextUsed
	if step := statefile.LastStep(recs); step != nil && step.Usage != nil {
		used = step.Usage.Prompt() + step.Usage.Output
	}
	if lengthStop {
		return i.Config.ContextLimit > 0 && used >= i.Config.ContextLimit
	}
	return historyOverflow(body, used)
}

// fullKey starts the idempotency key of the first message of a session the
// bot continues because the one before was full; the full session's id ends it.
const fullKey = "context-full-"

// continueFull is /continue, run by the bot when the thread's session ends
// at its context limit, as Mainplane intends a full session to go on. Its
// bounded handoff retains the task even after intermediate progress text.
// Of the threads that show the
// session, only the first to get here continues it; the others follow. It
// reports whether the session goes on: continued here, or by another thread.
func (b *bridge) continueFull(th, id string) bool {
	b.mu.Lock()
	taken := b.full[id]
	b.full[id] = true
	b.mu.Unlock()
	if taken {
		return true
	}
	text := autoContinueText
	if recs, err := b.records(id, 0, ""); err == nil {
		if stalledContinuation(recs) {
			b.mu.Lock()
			delete(b.full, id)
			b.mu.Unlock()
			b.say(th, msg{Text: "Automatic continuation stopped: the new session filled before making progress. Use !model to choose a larger context, or !continue with a shorter task."})
			return false
		}
	}
	if _, err := b.continueIn(th, id, fullKey+id, "", text); err != nil {
		b.mu.Lock()
		delete(b.full, id) // a transient failure must not poison later recovery
		b.mu.Unlock()
		b.say(th, msg{Text: "automatic /continue failed: " + err.Error()})
		return false
	}
	return true
}

// continuePrefix starts the first message of a session that continues
// another, from the terminal app's /model and /continue and from this bot's.
const continuePrefix = "You are continuing session "

// continued is the session a first message says it continues, or "".
func continued(msg string) string {
	rest, ok := strings.CutPrefix(msg, continuePrefix)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, ".")
	if statefile.ValidID(id) {
		return id
	}
	return ""
}

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
		var infos []struct {
			ID      string
			Status  statefile.Status
			N       int
			Created time.Time
			Config  statefile.Conf
		}
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
			busy[in.ID] = in.Status == statefile.StatusStepping || in.Status == statefile.StatusOpen || cached.count > 0
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
			first, from, ok := b.firstMessage(in.ID)
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

// threadTitle is a thread's name for a session with a label: "<id> <label>",
// on one line and within Discord's 100 characters.
func threadTitle(id, label string) string {
	t := id + " " + strings.Join(strings.Fields(label), " ")
	if r := []rune(t); len(r) > 100 {
		t = strings.TrimSpace(string(r[:99])) + "…"
	}
	return t
}

// quote shows a message sent from elsewhere in a thread, with where it came
// from: "> **cli:** fix the build".
func quote(via, body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	lines[0] = "**" + via + ":** " + lines[0]
	return "> " + strings.Join(lines, "\n> ")
}

// firstMessage is the session's first message and who sent it, if it has one.
func (b *bridge) firstMessage(id string) (body, via string, ok bool) {
	recs, err := b.records(id, 0, "")
	if err != nil {
		return "", "", false
	}
	for _, r := range recs {
		if r.Kind == statefile.Message {
			return string(r.Body), r.Via, true
		}
	}
	return "", "", false
}

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

// busySession holds info about a running session for !busy.
type busySession struct {
	ID             string
	Name           string
	Status         string
	Model          string
	Started        time.Time
	FirstAuthor    string
	JobCount       int
	BackgroundJobs int
}

// sayBusy lists all sessions that are stepping or have background jobs.
func (b *bridge) sayBusy(th, id string) error {
	var infos []struct {
		ID      string         `json:"id"`
		Name    string         `json:"name"`
		Status  string         `json:"status"`
		Config  statefile.Conf `json:"config"`
		Created time.Time      `json:"created"`
		Updated time.Time      `json:"updated"`
	}
	if err := b.call("GET", "/sessions?limit=100", "", nil, &infos); err != nil {
		return fmt.Errorf("could not list sessions: %w", err)
	}

	var busy []busySession
	for _, in := range infos {
		if in.ID == id {
			continue
		}
		background, err := b.countBackgroundJobs(in.ID)
		if err != nil {
			return fmt.Errorf("read jobs for %s: %w", in.ID, err)
		}
		if in.Status != string(statefile.StatusStepping) && in.Status != string(statefile.StatusOpen) && background == 0 {
			continue
		}

		bs := busySession{
			ID:      in.ID,
			Name:    in.Name,
			Status:  in.Status,
			Model:   in.Config.Model,
			Started: in.Created,
		}

		// Count background jobs by checking records
		bs.JobCount = b.countJobs(in.ID)
		bs.BackgroundJobs = background

		if first, via, ok := b.firstMessage(in.ID); ok {
			bs.FirstAuthor = b.extractAuthor(via, first)
		}

		busy = append(busy, bs)
	}

	if len(busy) == 0 {
		b.say(th, msg{Text: "No other sessions are currently running or have background jobs."})
		return nil
	}

	var lines []string
	lines = append(lines, "**Other busy sessions** ("+strconv.Itoa(len(busy))+"):")
	lines = append(lines, "")

	for _, bs := range busy {
		var parts []string
		parts = append(parts, "session `"+bs.ID+"`")
		if bs.Status == "stepping" {
			parts = append(parts, "**stepping**")
		} else {
			parts = append(parts, bs.Status)
		}
		if bs.Name != "" {
			parts = append(parts, "| **"+bs.Name+"**")
		}
		parts = append(parts, "on `"+bs.Model+"`")

		elapsed := time.Since(bs.Started).Round(time.Second)
		parts = append(parts, "· started "+elapsed.String())

		if bs.JobCount > 0 {
			parts = append(parts, strconv.Itoa(bs.JobCount)+" calls")
		}
		if bs.BackgroundJobs > 0 {
			parts = append(parts, strconv.Itoa(bs.BackgroundJobs)+" background jobs")
		}

		line := strings.Join(parts, " ")
		if bs.FirstAuthor != "" {
			line += " · by " + bs.FirstAuthor
		}
		lines = append(lines, line)
	}

	b.say(th, msg{Text: strings.Join(lines, "\n")})
	return nil
}

// extractAuthor gets the author name from via and first message.
func (b *bridge) extractAuthor(via, first string) string {
	if via == "" {
		return ""
	}
	// via is the connector, not the person. Slack's author is in its body,
	// after any bridge/session notes.
	if via == "slack" {
		for _, line := range strings.Split(first, "\n") {
			if strings.HasPrefix(line, "[") {
				if author, _, ok := strings.Cut(line[1:], "]:"); ok && strings.Contains(author, "<@") {
					return author
				}
			}
		}
	}
	if idx := strings.Index(via, ":"); idx >= 0 {
		return via[:idx]
	}
	return via
}

// countJobs counts the total number of tool calls in a session.
func (b *bridge) countJobs(id string) int {
	recs, err := b.records(id, 0, "")
	if err != nil {
		return 0
	}
	count := 0
	for _, r := range recs {
		if r.Kind == statefile.Call {
			count++
		}
	}
	return count
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

// choices is what sessions can run on now: the catalog, the providers the
// harness serves, and the models of the newest 20 sessions; without the
// catalog, those models still are. It also returns the newest 100 sessions,
// for startBase to find the newest one a thread shows.
func (b *bridge) choices() (choices, []info, error) {
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
	var recent []info
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

// logPaths are where the harness copied a session's records when it
// stopped, which its system prompt says a continuation names: the log on each
// connected worker of its config. None when the workers cannot be listed.
func (b *bridge) logPaths(id string, conf statefile.Conf) []string {
	var ws []struct{ Name, Scratch, Refused string }
	if err := b.call("GET", "/workers", "", nil, &ws); err != nil {
		log.Printf("workers: %v", err)
		return nil
	}
	scratch := map[string]string{}
	for _, w := range ws {
		if w.Refused == "" && w.Scratch != "" {
			scratch[w.Name] = w.Scratch
		}
	}
	return logLines(id, conf.Workers, scratch)
}

// logLines names <scratch>/logs/<id>.log on each of workers that has a scratch.
func logLines(id string, workers []statefile.Worker, scratch map[string]string) []string {
	var out []string
	for _, w := range workers {
		if s, ok := scratch[w.Name]; ok {
			out = append(out, s+"/logs/"+id+".log on "+w.Name)
		}
	}
	return out
}

func (b *bridge) create(conf statefile.Conf, via string) (string, error) {
	body, err := json.Marshal(conf)
	if err != nil {
		return "", err
	}
	var out struct{ ID string }
	err = b.call("POST", "/sessions?via="+via, "application/json", bytes.NewReader(body), &out)
	return out.ID, err
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

// resume follows the session a thread shows from the last record relayed.
func (b *bridge) resume(th, id string) {
	b.mu.Lock()
	t, ok := b.st.Threads[th]
	b.mu.Unlock()
	if ok && t.Session == id {
		b.follow(th, id, t.N)
		return
	}
	if i, err := b.info(id); err == nil {
		b.follow(th, id, i.N)
	}
}

// follow makes the thread show the session and long-polls the session after
// record n, relaying what the agent says. Once the thread shows another
// session it goes on until this one has been quiet for a poll, so a reply to
// a message sent before a quick /model still arrives. It stops when the
// harness refuses the session, as for one deleted.
func (b *bridge) follow(th, id string, n int) {
	b.bind(th, id, n)
	b.mu.Lock()
	k := [2]string{th, id}
	running := b.following[k]
	b.following[k] = true
	saved := b.st.Threads[th]
	b.mu.Unlock()
	if running {
		return
	}
	go func() {
		defer func() { b.mu.Lock(); delete(b.following, k); b.mu.Unlock() }()
		t := turn{attachmentExpected: saved.AttachmentPending, deliveryRecovery: saved.AttachmentRecovery}
		for {
			// Calls not shown yet, held back by the status throttle, show after a
			// short poll at most.
			wait := "60s"
			if t.dirty {
				wait = "3s"
			}
			recs, err := b.records(id, n, wait)
			var refused *refusedError
			if errors.As(err, &refused) {
				log.Printf("session %s: %v; no longer following it", id, err)
				if refused.gone() && b.shows(th, id) {
					b.chat(th).close(th)
					b.forget(th)
				}
				return
			}
			if err != nil {
				log.Printf("session %s: %v", id, err)
				time.Sleep(5 * time.Second)
				continue
			}
			if len(recs) == 0 && !b.shows(th, id) {
				return
			}
			for _, r := range recs {
				n = r.N
				b.relay(th, id, &t, r)
				// Text waiting for its step is not relayed yet, so a restart
				// shows it again.
				if len(t.text) == 0 {
					b.advance(th, id, n)
				}
			}
			if t.dirty && time.Since(t.shown) >= 3*time.Second {
				t.dirty, t.shown = false, time.Now()
				b.chat(th).status(th, id, &t)
			}
		}
	}()
}

// relay shows one record of session id in thread th.
func (b *bridge) relay(th, id string, t *turn, r statefile.Record) {
	if t.start.IsZero() && (r.Kind == statefile.Message || r.Kind == statefile.Text || r.Kind == statefile.Call) {
		t.start = r.Time
	}
	switch r.Kind {
	case statefile.Text:
		t.text = append(t.text, string(r.Body))
	case statefile.Step:
		if t.stepCalls > 0 {
			text := strings.Join(t.text, "\n\n")
			err := b.reply(th, text, "", false)
			t.deliveryErr = errors.Join(t.deliveryErr, err)
			if err == nil && hasAttachment(text) {
				t.attachmentExpected = false
				b.deliveryState(th, id, t)
			}
			t.text, t.stepCalls = nil, 0
		} else if provider.Normal(r.Header) {
			b.finishStep(th, id, t, r)
		}
	case statefile.Message:
		// A message the thread sent: the turn's reply answers it. One sent
		// from another thread of the same chat is quoted, as from elsewhere.
		b.mu.Lock()
		mine := r.Via == platform(th) && b.sentIn[r.IdempotencyKey] == th
		b.mu.Unlock()
		internal := bridgeMessage(r.IdempotencyKey)
		if strings.HasPrefix(r.IdempotencyKey, deliveryKey) {
			t.deliveryRecovery, t.attachmentExpected = true, true
			b.deliveryState(th, id, t)
		}
		if mine && r.IdempotencyKey != "" && !internal && r.IdempotencyKey != t.key {
			t.key = r.IdempotencyKey
			t.asked = append(t.asked, r.IdempotencyKey)
		}
		// The harness's own "Continue" from a retry is not a person's, and
		// a job's report is one line: its log tail floods the thread.
		switch {
		case r.Via == "job" && strings.HasPrefix(r.Type, "text/plain"):
			b.say(th, msg{Note: jobLine(string(r.Body))})
		case !mine && !internal && r.Via != "retry" && strings.HasPrefix(r.Type, "text/plain"):
			b.say(th, msg{Text: quote(r.Via, string(r.Body))})
		}
		// Every turn asked or retried, from anywhere, shows on the thread itself.
		if r.Via != "job" && strings.HasPrefix(r.Type, "text/plain") && !slices.Contains(t.asked, "") {
			t.asked = append(t.asked, "")
			b.chat(th).react(th, "", seen)
		}
	case statefile.Name:
		// The session's label, often set by its agent at the start: the thread
		// takes it.
		if b.shows(th, id) && len(r.Body) > 0 {
			b.rename(th, id, string(r.Body))
		}
	case statefile.Error:
		if !b.shows(th, id) {
			// An old request can fail after a model switch. Do not offer a
			// Retry for it, or change the new session's state or reactions.
			if t.status != "" {
				t.done, t.outcome, t.took = true, "continued", time.Since(t.start).Round(time.Second)
				b.chat(th).status(th, id, t)
			}
			*t = turn{}
			return
		}
		full := r.Via == "" && fresh(r.Time, time.Now()) && b.shows(th, id) && b.endedFull(id, string(r.Body))
		text := "⚠️ " + string(r.Body)
		outcome := "failed"
		switch {
		case r.Via != "": // who stopped it
			outcome = "stopped"
		case full:
			// What the full session wrote goes out first, above the notice.
			if len(t.text) > 0 {
				b.reply(th, strings.Join(t.text, "\n\n"), t.key, time.Since(t.start) > 2*time.Minute)
				t.text = nil
			}
			// Carry reactions before starting the next follower: a fast new
			// turn can otherwise finish before these messages are carried.
			b.finish(th, id, t, "continued")
			b.say(th, msg{Text: text, Note: "the context is full: continuing in a new session"})
			b.advance(th, id, r.N)
			if b.continueFull(th, id) { // not a failure: the turn goes on in a new session
				return
			}
		}
		b.finish(th, id, t, outcome)
		switch {
		case full: // said above
		case r.Via != "": // a stop: on purpose, so no Retry to offer
			b.say(th, msg{Text: text})
		default:
			ms, err := b.modelList(id)
			if err != nil {
				log.Printf("models: %v", err)
			}
			b.say(th, msg{Text: text, Session: id, Retry: true, Models: ms})
		}
	case statefile.Call:
		t.calls++
		t.stepCalls++
		title, detail := callParts(r.Body)
		t.tasks = append(t.tasks, task{id: r.ID, title: title, detail: detail, status: "in_progress", attachment: attachmentCall(r.Body), slackAction: slackActionCall(r.Body)})[max(0, len(t.tasks)+1-maxTasks):]
		t.dirty = true
	case statefile.Result:
		b.mu.Lock()
		request := b.st.Threads[th].Request
		b.mu.Unlock()
		receipt := confirmedSlackReceipt(r.Body, id, request)
		if receipt != nil {
			t.attachmentExpected = false
			t.actionReceipt = receipt.Permalink
			t.actionFailed = false
			b.deliveryState(th, id, t)
		}
		for i := range t.tasks {
			if t.tasks[i].id == r.For {
				if t.tasks[i].slackAction && receipt == nil {
					t.actionFailed = true
				}
				t.tasks[i].status = "complete"
				if (r.Exit != nil && *r.Exit != 0) || (r.Exit == nil && bytes.HasPrefix(r.Body, []byte("error: "))) {
					t.tasks[i].status = "error"
				}
				if t.tasks[i].attachment && t.tasks[i].status == "complete" && hasAttachment(string(r.Body)) {
					t.attachmentExpected = true
					b.deliveryState(th, id, t)
				}
				t.dirty = true
			}
		}
	}
}

// turn is what a follower shows of the agent's work since a person last
// spoke: one status message, edited as calls come, and then the reply.
type turn struct {
	start              time.Time
	key                string   // the chat message the reply answers, if any
	asked              []string // the chat messages of the turn, to mark done
	calls              int      // tool calls so far
	stepCalls          int      // calls of the step not yet closed
	tasks              []task   // the newest calls, at most maxTasks
	text               []string // text of the step not yet closed
	status             string   // the status message, once posted
	dirty              bool     // calls the status message does not show yet
	shown              time.Time
	attachmentExpected bool   // a run call only printed the delivery directive
	deliveryRecovery   bool   // one corrective model step has already been requested
	deliveryErr        error  // any progress reply/attachment that failed to send
	actionReceipt      string // confirmed executed Slack action, not an assistant assertion
	actionFailed       bool   // sender action returned no authoritative receipt

	// Set when the turn is done, for its last status.
	done    bool
	outcome string // "", "failed", "stopped" or "continued"
	took    time.Duration
	used    int // percent of the context used, or -1
}

// task is one call of a turn: its tool and worker, its command or path, and
// its status: in_progress, complete or error.
type task struct {
	id, title, detail, status string
	attachment                bool
	slackAction               bool
}

// maxTasks is the most calls a turn keeps, as many as a Slack plan block holds.
const maxTasks = 50

// finish ends the turn: the status message says how it went, and the
// waiting text goes out as a reply to the message that began the turn. A
// reply after a long turn notifies its author. outcome is "", "failed",
// "stopped" or "continued" (the session was full and goes on in a new one).
func (b *bridge) finish(th, id string, t *turn, outcome string) {
	text, key, asked := strings.Join(t.text, "\n\n"), t.key, t.asked
	if text != "" {
		if err := b.reply(th, text, key, time.Since(t.start) > 2*time.Minute); err != nil && outcome == "" {
			outcome = "failed"
		}
	}
	if t.deliveryErr != nil && outcome == "" {
		outcome = "failed"
	}
	took := time.Since(t.start).Round(time.Second)
	t.done, t.outcome, t.took, t.used = true, outcome, took, -1
	if t.status != "" && outcome == "" {
		if i, err := b.info(id); err == nil && i.Config.ContextLimit > 0 {
			t.used = i.ContextUsed * 100 / i.Config.ContextLimit
		}
	}
	b.chat(th).status(th, id, t)
	if outcome != "continued" {
		t.attachmentExpected, t.deliveryRecovery = false, false
	}
	b.deliveryState(th, id, t)
	*t = turn{}
	b.mu.Lock()
	asked, b.carried[th] = marks(b.carried[th], asked, outcome)
	b.mu.Unlock()
	for _, m := range asked {
		b.chat(th).react(th, m, reaction(outcome))
	}
}

// marks splits a thread's messages at the end of a turn: those to mark now,
// and those carried to its next turn. A continued turn marks nothing and
// carries its messages on; any other end marks them with those carried before.
func marks(carried, asked []string, outcome string) (mark, carry []string) {
	all := append(slices.Clone(carried), asked...)
	if outcome == "continued" {
		return nil, all
	}
	return all, nil
}

// reaction is the mark a turn's messages get when it ends: done, failed for a
// failure or a stop, and none when it is continued in a new session, since
// that is not where it ends.
func reaction(outcome string) string {
	switch outcome {
	case "":
		return done
	case "continued":
		return ""
	default:
		return failed
	}
}

// reply posts the agent's text. A line "attach: /path" sends that file from
// this machine with it, in place of the line.
func (b *bridge) reply(th, text, replyTo string, notify bool) error {
	var lines, notes []string
	var files []file
	var errs []error
	fence := ""
	for _, l := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[:3]
			if fence == "" {
				fence = marker
			} else if fence == marker {
				fence = ""
			}
			lines = append(lines, l)
			continue
		}
		if fence != "" {
			lines = append(lines, l)
			continue
		}
		path, ok := strings.CutPrefix(strings.TrimSpace(l), "attach: ")
		topLevel := false
		if !ok && platform(th) == "slack" {
			path, ok = strings.CutPrefix(strings.TrimSpace(l), "attach-channel: ")
			topLevel = ok
		}
		if !ok {
			lines = append(lines, l)
			continue
		}
		path = strings.TrimSpace(path)
		data, err := os.ReadFile(path)
		if err == nil && len(data) > maxAttach {
			err = fmt.Errorf("%s is over %d MiB", path, maxAttach>>20)
		}
		if err != nil {
			notes = append(notes, "could not attach: "+err.Error())
			errs = append(errs, err)
			continue
		}
		files = append(files, file{name: filepath.Base(path), size: len(data), fetch: func() ([]byte, error) { return data, nil }, topLevel: topLevel})
	}
	text = strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" && len(notes) == 0 && len(files) == 0 {
		return nil
	}
	return errors.Join(errors.Join(errs...), b.say(th, msg{Text: text, Note: strings.Join(notes, "\n"), ReplyTo: replyTo, Notify: notify, Files: files}))
}

// The bot's reaction on a message the session was sent: seen while the
// agent works on it, then done or failed.
const (
	seen   = "👀"
	done   = "✅"
	failed = "❌"
)

type title struct{ id, name string }

// rename gives thread th session id's name, in a goroutine of its own: a
// platform's rename limit (Discord's is about two per ten minutes) then holds
// back the rename only, and a rename that waits takes the newest name.
func (b *bridge) rename(th, id, name string) {
	b.mu.Lock()
	_, running := b.renames[th]
	b.renames[th] = &title{id, name}
	b.mu.Unlock()
	if running {
		return
	}
	go func() {
		for {
			b.mu.Lock()
			t := b.renames[th]
			if t == nil {
				delete(b.renames, th)
				b.mu.Unlock()
				return
			}
			b.renames[th] = nil
			b.mu.Unlock()
			b.chat(th).rename(th, t.id, t.name)
		}
	}()
}

// jobLine is the first line of a job's report without its ids and log path:
// "[job toolu_x on admin exited 0 after 40s. Log: …]" is "job on admin exited
// 0 after 40s".
func jobLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	line, _, _ = strings.Cut(strings.Trim(line, "[]"), ". Log:")
	if f := strings.Fields(line); len(f) > 2 && f[0] == "job" {
		line = "job " + strings.Join(f[2:], " ")
	}
	return line
}

// calls is "1 call" or "n calls".
func calls(n int) string {
	if n == 1 {
		return "1 call"
	}
	return fmt.Sprintf("%d calls", n)
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

// fresh reports whether message id is newer than every one handled in
// channel ch, and marks it handled.
func (b *bridge) fresh(ch, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !newer(id, b.st.Seen[ch]) {
		return false
	}
	b.st.Seen[ch] = id
	b.save()
	return true
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

// records reads the session's records after record after, long polling for
// wait when there are none yet ("" returns at once).
func (b *bridge) records(id string, after int, wait string) ([]statefile.Record, error) {
	u := fmt.Sprintf("%s/sessions/%s/records?after=%d", b.harness, id, after)
	if wait != "" {
		u += "&wait=" + wait
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, &refusedError{resp.Status + ": " + strings.TrimSpace(string(msg))}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var recs []statefile.Record
	br := bufio.NewReader(resp.Body)
	for {
		r, err := statefile.Decode(br)
		if err == io.EOF {
			return recs, nil
		}
		if err != nil {
			return recs, err
		}
		recs = append(recs, r)
	}
}

// refusedError is a 4xx from the harness, which a retry does not mend.
type refusedError struct{ msg string }

func (e *refusedError) Error() string { return e.msg }

// gone is whether the session no longer exists, as after a delete, rather
// than the call being refused for another reason, such as a revoked key. The
// harness answers every error with 400, so only its text tells.
func (e *refusedError) gone() bool { return strings.Contains(e.msg, "no such file") }

func (b *bridge) info(id string) (info, error) {
	var i info
	return i, b.call("GET", "/sessions/"+id, "", nil, &i)
}

func (b *bridge) call(method, path, ctype string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(context.Background(), method, b.harness+path, body)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return b.do(req, out)
}

func (b *bridge) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// callParts says what tool runs where, e.g. "run on admin", and on what: the
// first line of its command, code or path.
func callParts(body []byte) (title, detail string) {
	var c struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if json.Unmarshal(body, &c) != nil {
		return "working…", ""
	}
	title = c.Name
	if w, ok := c.Arguments["worker"].(string); ok {
		title += " on " + w
	}
	for _, k := range []string{"command", "code", "path"} {
		if v, ok := c.Arguments[k].(string); ok {
			first, _, _ := strings.Cut(v, "\n")
			return title, firstWords(first, 200)
		}
	}
	return title, ""
}

// firstWords is s on one line, cut to at most max bytes and a whole rune.
func firstWords(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:runeStart(s, max)]) + "…"
}

// runeStart is i, or the start of the rune byte i of s is in, so that s[:i]
// cuts no rune in two.
func runeStart(s string, i int) int {
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// split cuts text into pieces of at most max bytes at line breaks where it
// can, else at a rune's start, closing and reopening a code fence that a cut
// falls inside.
func split(text string, max int) []string {
	var parts []string
	fence := false
	for len(text) > 0 {
		if len(text) <= max {
			parts = append(parts, text)
			break
		}
		cut := strings.LastIndex(text[:max], "\n")
		if cut <= 0 {
			cut = runeStart(text, max)
		}
		part := text[:cut]
		text = strings.TrimPrefix(text[cut:], "\n")
		if strings.Count(part, "```")%2 == 1 {
			fence = !fence
		}
		if fence {
			part += "\n```"
			text = "```\n" + text
			fence = false
		}
		parts = append(parts, part)
	}
	return parts
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
