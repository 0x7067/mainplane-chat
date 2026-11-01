// mainplane-discord drives Mainplane agents from a private Discord server, the
// way the mainplane terminal app does. A message in any channel starts a
// session in a new thread named after it; messages in that thread continue it,
// and the agent's replies come back there. A thread takes the terminal app's
// /model, /continue and /stop, and the CLI's /retry and /info. What happens
// while the bot is down is picked up when it comes back.
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mainplane-ai/mainplane/pkg/catalog"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

const via = "discord"

// The catalog groups the terminal app reads: the models it offers, and the
// ones a new session takes when the newest session's is not connected.
const (
	group    = "self-hosted-all"
	defaults = "self-hosted-default"
)

type bridge struct {
	dc      *discordgo.Session
	harness string // base URL of the harness API
	key     string
	path    string // the state file
	tester  string // a bot user id whose messages count as a person's, for end-to-end tests

	mu        sync.Mutex
	following map[string]bool // session ids with a follower running
	sessions  string          // the #sessions channel, where sessions made outside Discord get threads
	st        state
}

// state is kept across restarts, so the bot picks up what happened while it
// was down.
type state struct {
	Seen    map[string]string `json:"seen"`    // channel or thread id -> newest Discord message handled
	Relayed map[string]int    `json:"relayed"` // session id -> newest record relayed
	Threads map[string]string `json:"threads"` // thread id -> the session it shows
	// MirrorSince is when mirroring began: sessions made outside Discord after
	// it get a thread in #sessions; older ones do not.
	MirrorSince time.Time `json:"mirror_since"`
}

type info struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Config      statefile.Conf `json:"config"`
	N           int            `json:"n"`
	ContextUsed int            `json:"context_used"`
}

func main() {
	b := &bridge{
		harness:   envOr("MAINPLANE_URL", "http://127.0.0.1:8080"),
		key:       secret(mustEnv("MAINPLANE_KEY")),
		path:      statePath(),
		tester:    os.Getenv("MAINPLANE_DISCORD_TESTER"),
		following: map[string]bool{},
	}
	b.load()
	dc, err := discordgo.New("Bot " + mustEnv("DISCORD_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	dc.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
	// Events are handled one at a time, in order, and catching up on Ready
	// finishes before the next message is handled.
	dc.SyncEvents = true
	dc.AddHandler(b.onMessage)
	dc.AddHandler(b.onReady)
	dc.AddHandler(b.onInteraction)
	b.dc = dc
	if err := dc.Open(); err != nil {
		log.Fatal(err)
	}
	defer dc.Close()
	log.Print("connected")
	go b.watchSessions()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}

// onReady follows the sessions of the active threads from the last record
// relayed, then handles the messages sent while the bot was down. A channel
// the bot has not seen before starts from now.
func (b *bridge) onReady(s *discordgo.Session, r *discordgo.Ready) {
	for _, g := range r.Guilds {
		var chans []*discordgo.Channel
		if threads, err := s.GuildThreadsActive(g.ID); err == nil {
			for _, t := range threads.Threads {
				if id := b.sessionFor(t); id != "" {
					if n, ok := b.relayedFrom(id); ok {
						b.follow(t.ID, id, n)
					}
				}
				chans = append(chans, t)
			}
		}
		if cs, err := s.GuildChannels(g.ID); err == nil {
			for _, c := range cs {
				if c.Type == discordgo.ChannelTypeGuildText {
					chans = append(chans, c)
					if c.Name == "sessions" {
						b.mu.Lock()
						b.sessions = c.ID
						b.mu.Unlock()
					}
				}
			}
		}
		for _, c := range chans {
			b.replay(c)
		}
	}
	log.Print("caught up")
}

// replay handles the messages in c after the newest one handled there.
func (b *bridge) replay(c *discordgo.Channel) {
	b.mu.Lock()
	after, ok := b.st.Seen[c.ID]
	b.mu.Unlock()
	if !ok {
		if c.LastMessageID != "" {
			b.fresh(c.ID, c.LastMessageID)
		}
		return
	}
	for {
		msgs, err := b.dc.ChannelMessages(c.ID, 100, "", after, "")
		if err != nil || len(msgs) == 0 {
			return
		}
		sort.Slice(msgs, func(i, j int) bool { return newer(msgs[j].ID, msgs[i].ID) })
		for _, m := range msgs {
			b.handle(m)
		}
		after = msgs[len(msgs)-1].ID
		if len(msgs) < 100 {
			return
		}
	}
}

func (b *bridge) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) { b.handle(m.Message) }

func (b *bridge) handle(m *discordgo.Message) {
	if !b.fresh(m.ChannelID, m.ID) || m.Author == nil || m.Author.Bot && m.Author.ID != b.tester {
		return
	}
	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}
	ch, err := b.dc.State.Channel(m.ChannelID)
	if err != nil {
		if ch, err = b.dc.Channel(m.ChannelID); err != nil {
			return
		}
	}
	if !ch.IsThread() {
		b.start(m, text)
		return
	}
	id := b.sessionFor(ch)
	if id == "" {
		return
	}
	if b.command(ch, id, m.ID, text) {
		return
	}
	b.post(ch.ID, id, m.ID, text)
}

// command runs a thread command and reports whether text was one. /model and
// /continue take a message for the session they make.
func (b *bridge) command(th *discordgo.Channel, id, msgID, text string) bool {
	word := strings.Fields(text)[0]
	if len(word) < 2 || word[0] != '/' && word[0] != '!' {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, word))
	var err error
	switch word[1:] {
	case "stop":
		err = b.call("POST", "/sessions/"+id+"/stop?via="+via, "", nil, nil)
	case "retry":
		if err = b.call("POST", "/sessions/"+id+"/retry?via="+via, "", nil, nil); err == nil {
			if n, ok := b.relayedFrom(id); ok {
				b.follow(th.ID, id, n)
			}
		}
	case "info":
		err = b.sayInfo(th.ID, id)
	case "continue":
		_, err = b.continueIn(th, id, msgID, "", rest)
	case "model":
		if rest == "" {
			err = b.listModels(th.ID, id)
			break
		}
		model := strings.Fields(rest)[0]
		_, err = b.continueIn(th, id, msgID, model, strings.TrimSpace(strings.TrimPrefix(rest, model)))
	default:
		return false
	}
	if err != nil {
		b.say(th.ID, word+" failed: "+err.Error())
	}
	return true
}

// start makes a session with the config newConf picks and a thread for it
// named "<session id> <first words>".
func (b *bridge) start(m *discordgo.Message, text string) {
	conf, err := b.newConf()
	if err != nil {
		b.say(m.ChannelID, "could not start a session: "+err.Error())
		return
	}
	id, err := b.create(conf)
	if err != nil {
		b.say(m.ChannelID, "could not start a session: "+err.Error())
		return
	}
	name := id + " " + firstWords(text, 80)
	th, err := b.dc.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{Name: name, AutoArchiveDuration: 10080})
	if err != nil {
		b.say(m.ChannelID, "session "+id+" started, but the thread failed: "+err.Error())
		return
	}
	b.post(th.ID, id, m.ID, text)
}

// newConf is the config of a new session, picked as the terminal app picks
// it: the model of the newest session, else the catalog's default, each only
// when its provider is connected. Its workers are every connected worker.
func (b *bridge) newConf() (statefile.Conf, error) {
	cat, ids, err := b.models()
	if err != nil {
		return statefile.Conf{}, err
	}
	if len(ids) == 0 {
		return statefile.Conf{}, errors.New("no provider of the catalog is connected")
	}
	want := cat.Groups[defaults]
	var newest []info
	if err := b.call("GET", "/sessions?limit=1", "", nil, &newest); err != nil {
		return statefile.Conf{}, err
	}
	if len(newest) > 0 {
		want = append([]string{newest[0].Config.Preset, newest[0].Config.Model}, want...)
	}
	pick := ids[0]
	for _, id := range want {
		if slices.Contains(ids, id) {
			pick = id
			break
		}
	}
	conf := presetConf(pick, cat.Presets[pick])
	conf.Workers, err = b.workers()
	return conf, err
}

// continueIn is /continue, and /model with a model: a new session with this
// one's config, or the model's and this one's workers, told to carry on from
// this one, as the terminal app does. The thread then shows the new session.
func (b *bridge) continueIn(th *discordgo.Channel, old, msgID, model, text string) (string, error) {
	cur, err := b.info(old)
	if err != nil {
		return "", err
	}
	conf := cur.Config
	if model != "" {
		cat, ids, err := b.models()
		if err != nil {
			return "", err
		}
		id := pickModel(cat, ids, model)
		if id == "" {
			return "", fmt.Errorf("no connected model %q: /model lists them", model)
		}
		conf = presetConf(id, cat.Presets[id])
		conf.Workers = cur.Config.Workers
	}
	id, err := b.create(conf)
	if err != nil {
		return "", err
	}
	b.say(th.ID, "-# continuing in session "+id+" on "+conf.Model)
	msg := "You are continuing session " + old + ". Retrieve key context from it as you go."
	if text != "" {
		msg += "\n" + text
	}
	b.post(th.ID, id, msgID, msg)
	return id, nil
}

// contextFull is whether an error record is the harness ending a session at
// its context limit: "context 250009 exceeds limit 250000".
func contextFull(body string) bool {
	return strings.HasPrefix(body, "context ") && strings.Contains(body, " exceeds limit ")
}

// continueFull is /continue, run by the bot when the thread's session ends
// at its context limit (error record n), as Mainplane intends a full session
// to go on. The new session gets the last message the full one did not
// answer, so it answers what was just asked.
func (b *bridge) continueFull(thread, id string, n int) {
	th, err := b.dc.State.Channel(thread)
	if err != nil {
		if th, err = b.dc.Channel(thread); err != nil {
			log.Printf("continue %s: %v", id, err)
			return
		}
	}
	text := "The session before ran out of context. Carry on where it stopped."
	if recs, err := b.records(id, max(0, n-20), ""); err == nil {
		if m := lastUnanswered(recs); m != "" {
			text += "\nThe last message, which it did not answer:\n" + m
		}
	}
	if _, err := b.continueIn(th, id, "context-full-"+id, "", text); err != nil {
		b.say(thread, "automatic /continue failed: "+err.Error())
	}
}

// lastUnanswered is the newest text message, from a person or a connector,
// with no agent reply after it, or "". A retry, a job's report and a
// continuation's own first message do not count.
func lastUnanswered(recs []statefile.Record) string {
	last := ""
	for _, r := range recs {
		switch {
		case r.Kind == statefile.Text:
			last = ""
		case r.Kind == statefile.Message && strings.HasPrefix(r.Type, "text/plain") &&
			r.Via != "retry" && r.Via != "job" && !strings.HasPrefix(string(r.Body), continuePrefix):
			last = string(r.Body)
		}
	}
	return last
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

// watchSessions keeps Discord in step with sessions made anywhere. A session
// that continues one a thread shows moves that thread to it; one made outside
// Discord after mirroring began gets its own thread in #sessions. Mainplane
// has no event for a new session, so this reads the list every few seconds,
// as the terminal app's sidebar does. A session is checked once it has its
// first message; only sessions made in the last day are.
func (b *bridge) watchSessions() {
	checked := map[string]bool{}
	for ; ; time.Sleep(3 * time.Second) {
		var infos []struct {
			ID      string
			Created time.Time
			Config  statefile.Conf
		}
		if err := b.call("GET", "/sessions?limit=20", "", nil, &infos); err != nil {
			log.Printf("sessions: %v", err)
			continue
		}
		for _, in := range infos {
			if checked[in.ID] || time.Since(in.Created) > 24*time.Hour {
				continue
			}
			first, via, ok := b.firstMessage(in.ID)
			if !ok {
				continue
			}
			if old := continued(first); old != "" {
				if threads := b.threadsShowing(old); len(threads) > 0 {
					checked[in.ID] = true
					for _, thread := range threads {
						b.follow(thread, in.ID, 0)
						b.say(thread, "-# continued in session "+in.ID)
					}
					continue
				}
			}
			b.mu.Lock()
			channel, since := b.sessions, b.st.MirrorSince
			b.mu.Unlock()
			if !mirrored(via, in.Created, since) || len(b.threadsShowing(in.ID)) > 0 {
				checked[in.ID] = true
				continue
			}
			if channel == "" {
				continue // #sessions not found yet; check again
			}
			checked[in.ID] = true
			b.mirror(channel, in.ID, via, in.Config.Model, first)
		}
	}
}

// mirrored is whether a session gets a thread in #sessions: its first
// message came from outside Discord, after mirroring began.
func mirrored(from string, created, since time.Time) bool {
	return from != via && created.After(since)
}

// mirror makes a thread in #sessions for a session made outside Discord and
// follows it from its start, its first message included.
func (b *bridge) mirror(channel, id, via, model, first string) {
	m, err := b.dc.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Content:         fmt.Sprintf("`%s` · via %s · %s", id, via, model),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		log.Printf("mirror %s: %v", id, err)
		return
	}
	th, err := b.dc.MessageThreadStartComplex(channel, m.ID, &discordgo.ThreadStart{Name: id + " " + firstWords(first, 80), AutoArchiveDuration: 10080})
	if err != nil {
		log.Printf("mirror %s: %v", id, err)
		return
	}
	b.follow(th.ID, id, 0)
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

// quote shows a message sent from outside Discord in a thread, with where it
// came from: "> **cli:** fix the build".
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
	for t, s := range b.st.Threads {
		if s == id {
			ts = append(ts, t)
		}
	}
	return ts
}

// listModels is /model alone: a picker of the connected models.
func (b *bridge) listModels(thread, id string) error {
	picker, err := b.modelPicker(id)
	if err != nil {
		return err
	}
	b.say(thread, "-# continue in a new session on another model, or `/model <id> [message]`", picker...)
	return nil
}

// modelPicker is a select of the connected models, this session's chosen;
// picking one is /model with it. Discord shows at most 25, the current first.
func (b *bridge) modelPicker(id string) ([]discordgo.MessageComponent, error) {
	cat, ids, err := b.models()
	if err != nil {
		return nil, err
	}
	cur, _ := b.info(id)
	var opts []discordgo.SelectMenuOption
	if i := slices.Index(ids, cur.Config.Preset); i > 0 {
		ids = slices.Concat(ids[i:i+1], ids[:i], ids[i+1:]) // first, so it is within the 25
	}
	for _, m := range ids[:min(len(ids), 25)] {
		opts = append(opts, discordgo.SelectMenuOption{Label: cmp.Or(cat.Presets[m].Name, m), Value: m, Description: m, Default: m == cur.Config.Preset})
	}
	if len(opts) == 0 {
		return nil, errors.New("no provider of the catalog is connected")
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: "model:" + id, Placeholder: "Continue on another model", Options: opts},
	}}}, nil
}

func (b *bridge) sayInfo(thread, id string) error {
	i, err := b.info(id)
	if err != nil {
		return err
	}
	var names []string
	for _, w := range i.Config.Workers {
		names = append(names, w.Name)
	}
	b.say(thread, fmt.Sprintf("session `%s` %s\nmodel `%s`\nworkers %s\ncontext %d/%d",
		i.ID, i.Status, i.Config.Model, strings.Join(names, ", "), i.ContextUsed, i.Config.ContextLimit))
	return nil
}

// models is the catalog and the ids of its presets whose provider the harness
// serves, in the catalog's order: the models the terminal app offers.
func (b *bridge) models() (catalog.Catalog, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cat, err := catalog.Fetch(ctx)
	if err != nil {
		return cat, nil, err
	}
	var ps []struct{ Name string }
	if err := b.call("GET", "/providers", "", nil, &ps); err != nil {
		return cat, nil, err
	}
	var ids []string
	for _, id := range cat.Groups[group] {
		if slices.ContainsFunc(ps, func(p struct{ Name string }) bool { return p.Name == cat.Presets[id].Provider }) {
			ids = append(ids, id)
		}
	}
	return cat, ids, nil
}

// pickModel is the id among ids that s names: the id, the model or its name.
func pickModel(cat catalog.Catalog, ids []string, s string) string {
	for _, id := range ids {
		p := cat.Presets[id]
		if strings.EqualFold(s, id) || strings.EqualFold(s, p.Model) || strings.EqualFold(s, p.Name) {
			return id
		}
	}
	return ""
}

func presetConf(id string, p catalog.Preset) statefile.Conf {
	return statefile.Conf{Model: p.Provider + "/" + p.Model, Tools: p.Tools, ContextLimit: p.ContextLimit, Input: p.Input, Params: p.Params, Preset: id}
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

func (b *bridge) create(conf statefile.Conf) (string, error) {
	body, err := json.Marshal(conf)
	if err != nil {
		return "", err
	}
	var out struct{ ID string }
	err = b.call("POST", "/sessions?via="+via, "application/json", bytes.NewReader(body), &out)
	return out.ID, err
}

// post sends text to the session, keyed by the Discord message so a repeat
// appends nothing, and makes sure the thread follows the session.
func (b *bridge) post(thread, id, msgID, text string) {
	req, _ := http.NewRequest("POST", b.harness+"/sessions/"+id+"/records?via="+via, strings.NewReader(text))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Idempotency-Key", msgID)
	var out struct{ N int }
	if err := b.do(req, &out); err != nil {
		b.say(thread, "send failed: "+err.Error())
		return
	}
	b.mark(thread, msgID, seen)
	b.follow(thread, id, out.N-1) // from the message itself, the reply's target
}

// follow long-polls the session after record n and relays what the agent
// says. Once the thread shows another session it goes on until this one has
// been quiet for a poll, so a reply to a message sent before a quick /model
// still arrives. It stops when the harness refuses the session, as for one
// deleted.
func (b *bridge) follow(thread, id string, n int) {
	b.mu.Lock()
	if b.st.Threads[thread] != id {
		b.st.Threads[thread] = id
		b.save()
	}
	running := b.following[id]
	b.following[id] = true
	b.mu.Unlock()
	if running {
		return
	}
	b.relayed(id, n)
	go func() {
		defer func() { b.mu.Lock(); delete(b.following, id); b.mu.Unlock() }()
		var t turn
		for {
			recs, err := b.records(id, n, "60s")
			var refused *refusedError
			if errors.As(err, &refused) {
				log.Printf("session %s: %v; no longer following it", id, err)
				if refused.gone() && b.shows(thread, id) {
					yes := true
					if _, err := b.dc.ChannelEdit(thread, &discordgo.ChannelEdit{Archived: &yes, Locked: &yes}); err != nil {
						log.Printf("archive thread %s: %v", thread, err)
					}
				}
				return
			}
			if err != nil {
				log.Printf("session %s: %v", id, err)
				time.Sleep(5 * time.Second)
				continue
			}
			if len(recs) == 0 && !b.shows(thread, id) {
				return
			}
			for _, r := range recs {
				n = r.N
				if t.start.IsZero() && (r.Kind == statefile.Message || r.Kind == statefile.Text || r.Kind == statefile.Call) {
					t.start = r.Time
				}
				switch r.Kind {
				case statefile.Text:
					t.text = append(t.text, string(r.Body))
				case statefile.Step:
					if t.stepCalls > 0 {
						b.say(thread, strings.Join(t.text, "\n\n"))
						t.text, t.stepCalls = nil, 0
					} else {
						b.finish(thread, id, &t, "")
					}
				case statefile.Message:
					if r.Via == via && snowflake(r.IdempotencyKey) {
						t.key = r.IdempotencyKey
						t.asked = append(t.asked, r.IdempotencyKey)
					}
					// The harness's own "Continue" from a retry is not a person's.
					if r.Via != via && r.Via != "retry" && strings.HasPrefix(r.Type, "text/plain") {
						b.say(thread, quote(r.Via, string(r.Body)))
					}
				case statefile.Name:
					// The session's label, often set by its agent at the start: the
					// thread takes it. A rename past Discord's limit of about two per
					// ten minutes waits here, in this session's follower only.
					if b.shows(thread, id) && len(r.Body) > 0 {
						name := threadTitle(id, string(r.Body))
						if _, err := b.dc.ChannelEdit(thread, &discordgo.ChannelEdit{Name: name}); err != nil {
							log.Printf("rename thread %s: %v", thread, err)
						}
					}
				case statefile.Error:
					outcome := "failed"
					if r.Via != "" { // who stopped it
						outcome = "stopped"
					}
					b.finish(thread, id, &t, outcome)
					if contextFull(string(r.Body)) && b.shows(thread, id) {
						b.say(thread, "⚠️ "+string(r.Body)+"\n-# the context is full: continuing in a new session")
						b.relayed(id, r.N)
						b.continueFull(thread, id, r.N)
						continue
					}
					if r.Via != "" { // a stop: on purpose, so no Retry to offer
						b.say(thread, "⚠️ "+string(r.Body))
						break
					}
					retry := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
						discordgo.Button{Label: "Retry", Style: discordgo.PrimaryButton, CustomID: "retry:" + id},
					}}
					picker, err := b.modelPicker(id)
					if err != nil {
						log.Printf("models: %v", err)
					}
					b.say(thread, "⚠️ "+string(r.Body), append([]discordgo.MessageComponent{retry}, picker...)...)
				case statefile.Call:
					_ = b.dc.ChannelTyping(thread)
					t.calls++
					t.stepCalls++
					t.last = append(t.last, callLine(r.Body))[max(0, len(t.last)-2):]
					t.dirty = true
				}
				// Text waiting for its step is not relayed yet, so a restart
				// shows it again.
				if len(t.text) == 0 {
					b.relayed(id, n)
				}
			}
			if t.dirty {
				b.showStatus(thread, id, &t)
			}
		}
	}()
}

// turn is what a follower shows of the agent's work since a person last
// spoke: one status message, edited as calls come, and then the reply.
type turn struct {
	start     time.Time
	key       string   // the Discord message the reply answers, if any
	asked     []string // the Discord messages of the turn, to mark done
	calls     int      // tool calls so far
	stepCalls int      // calls of the step not yet closed
	last      []string // the newest call lines
	text      []string // text of the step not yet closed
	status    string   // the status message, once posted
	dirty     bool     // calls the status message does not show yet
}

// showStatus posts or edits the turn's status message: when it started, in a
// timestamp Discord keeps live, the calls, and a Stop button.
func (b *bridge) showStatus(thread, id string, t *turn) {
	content := fmt.Sprintf("**working** · started <t:%d:R> · %s", t.start.Unix(), calls(t.calls))
	for _, l := range t.last {
		content += "\n-# " + l
	}
	comps := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Stop", Style: discordgo.SecondaryButton, CustomID: "stop:" + id},
	}}}
	t.dirty = false
	if t.status == "" {
		m, err := b.dc.ChannelMessageSendComplex(thread, &discordgo.MessageSend{
			Content: content, Components: comps,
			AllowedMentions: &discordgo.MessageAllowedMentions{}, Flags: discordgo.MessageFlagsSuppressEmbeds,
		})
		if err != nil {
			log.Printf("status %s: %v", id, err)
			return
		}
		t.status = m.ID
		return
	}
	if _, err := b.dc.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: t.status, Channel: thread, Content: &content, Components: &comps}); err != nil {
		log.Printf("status %s: %v", id, err)
	}
}

// finish ends the turn: the status message shrinks to one line, and the
// waiting text goes out as a reply to the message that began the turn. A
// reply after a long turn notifies its author. outcome is "", "failed" or
// "stopped".
func (b *bridge) finish(thread, id string, t *turn, outcome string) {
	took := time.Since(t.start).Round(time.Second)
	if t.status != "" {
		line := fmt.Sprintf("-# %s · %s", calls(t.calls), took)
		if outcome != "" {
			line = fmt.Sprintf("-# %s after %s · %s", outcome, calls(t.calls), took)
		} else if i, err := b.info(id); err == nil && i.Config.ContextLimit > 0 {
			line += fmt.Sprintf(" · %d%% context", i.ContextUsed*100/i.Config.ContextLimit)
		}
		none := []discordgo.MessageComponent{}
		if _, err := b.dc.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: t.status, Channel: thread, Content: &line, Components: &none}); err != nil {
			log.Printf("status %s: %v", id, err)
		}
	}
	text, key, asked := strings.Join(t.text, "\n\n"), t.key, t.asked
	*t = turn{}
	result := done
	if outcome != "" {
		result = failed
	}
	for _, m := range asked {
		b.mark(thread, m, result)
	}
	if text == "" {
		return
	}
	long := took > 2*time.Minute
	mentions := &discordgo.MessageAllowedMentions{RepliedUser: long}
	var ref *discordgo.MessageReference
	switch {
	case key != "" && key != thread:
		ref = &discordgo.MessageReference{MessageID: key, ChannelID: thread}
	case key == thread && long:
		// A thread's id is its starter message's, which is in the parent
		// channel, and Discord refuses a reply across channels: mention.
		if th, err := b.dc.Channel(thread); err == nil {
			if m, err := b.dc.ChannelMessage(th.ParentID, key); err == nil {
				text = m.Author.Mention() + " " + text
				mentions.Users = []string{m.Author.ID}
			}
		}
	}
	for i, part := range split(text, 1900) {
		msg := &discordgo.MessageSend{Content: part, AllowedMentions: mentions, Flags: discordgo.MessageFlagsSuppressEmbeds}
		if i == 0 {
			msg.Reference = ref
		}
		if _, err := b.dc.ChannelMessageSendComplex(thread, msg); err != nil {
			log.Printf("discord send: %v", err)
		}
	}
}

// The bot's reaction on a message the session was sent: seen while the
// agent works on it, then done or failed. The start messages in a channel
// show at a glance how each session's last turn went.
const (
	seen   = "👀"
	done   = "✅"
	failed = "❌"
)

// mark sets the bot's reaction on Discord message msgID, sent in the thread,
// or in its parent channel when it is the thread's starter, whose id the
// thread has. Any other reaction leaves seen's place.
func (b *bridge) mark(thread, msgID, emoji string) {
	if !snowflake(msgID) {
		return
	}
	ch := thread
	if msgID == thread {
		th, err := b.dc.State.Channel(thread)
		if err != nil {
			if th, err = b.dc.Channel(thread); err != nil {
				log.Printf("mark %s: %v", msgID, err)
				return
			}
		}
		ch = th.ParentID
	}
	if emoji != seen {
		_ = b.dc.MessageReactionRemove(ch, msgID, seen, "@me")
	}
	if err := b.dc.MessageReactionAdd(ch, msgID, emoji); err != nil {
		log.Printf("mark %s: %v", msgID, err)
	}
}

// calls is "1 call" or "n calls".
func calls(n int) string {
	if n == 1 {
		return "1 call"
	}
	return fmt.Sprintf("%d calls", n)
}

// snowflake is whether s is a Discord id, as a message's idempotency key is
// when the bot posted it for a Discord message.
func snowflake(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// onInteraction runs a button or select as the thread command it names:
// "stop:<id>" is !stop on session id, and "model:<id>" picking m is
// !model m. The message the component is on stands for the person's.
func (b *bridge) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}
	data := i.MessageComponentData()
	action, id, _ := strings.Cut(data.CustomID, ":")
	for _, v := range data.Values {
		action += " " + v
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}); err != nil {
		log.Printf("interaction: %v", err)
	}
	th, err := s.State.Channel(i.ChannelID)
	if err != nil {
		if th, err = s.Channel(i.ChannelID); err != nil {
			return
		}
	}
	b.command(th, id, i.Message.ID, "!"+action)
}

func (b *bridge) shows(thread, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st.Threads[thread] == id
}

// sessionFor is the session a thread shows: the one it was last moved to,
// else the one its name starts with.
func (b *bridge) sessionFor(th *discordgo.Channel) string {
	b.mu.Lock()
	id, ok := b.st.Threads[th.ID]
	b.mu.Unlock()
	if ok {
		return id
	}
	return sessionOf(th.Name)
}

// relayedFrom is the record to follow a session after: the last relayed,
// else its newest.
func (b *bridge) relayedFrom(id string) (int, bool) {
	b.mu.Lock()
	n, ok := b.st.Relayed[id]
	b.mu.Unlock()
	if ok {
		return n, true
	}
	i, err := b.info(id)
	return i.N, err == nil
}

func (b *bridge) relayed(id string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.st.Relayed[id] {
		b.st.Relayed[id] = n
		b.save()
	}
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

// newer compares Discord ids, which grow with time.
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
	return filepath.Join(dir, "mainplane-discord", "state.json")
}

func (b *bridge) load() {
	data, err := os.ReadFile(b.path)
	if err == nil {
		err = json.Unmarshal(data, &b.st)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("state: %v", err)
	}
	if b.st.Seen == nil {
		b.st.Seen = map[string]string{}
	}
	if b.st.Relayed == nil {
		b.st.Relayed = map[string]int{}
	}
	if b.st.Threads == nil {
		b.st.Threads = map[string]string{}
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
		return nil, &refusedError{resp.StatusCode, resp.Status + ": " + strings.TrimSpace(string(msg))}
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
type refusedError struct {
	code int
	msg  string
}

func (e *refusedError) Error() string { return e.msg }

// gone is whether the session no longer exists, as after a delete, rather
// than the call being refused for another reason, such as a revoked key.
func (e *refusedError) gone() bool {
	return e.code == http.StatusNotFound || strings.Contains(e.msg, "no such file")
}

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

// say sends text in Discord-sized pieces, with mentions and link previews off.
func (b *bridge) say(channel, text string, comps ...discordgo.MessageComponent) {
	parts := split(text, 1900)
	for i, part := range parts {
		msg := &discordgo.MessageSend{
			Content:         part,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
			Flags:           discordgo.MessageFlagsSuppressEmbeds,
		}
		if i == len(parts)-1 {
			msg.Components = comps
		}
		_, err := b.dc.ChannelMessageSendComplex(channel, msg)
		if err != nil {
			log.Printf("discord send: %v", err)
		}
	}
}

// sessionOf is the session id a thread name starts with, or "".
func sessionOf(name string) string {
	id, _, _ := strings.Cut(name, " ")
	if statefile.ValidID(id) {
		return id
	}
	return ""
}

// callLine is one line saying what tool runs where, e.g. "run on admin: ls".
func callLine(body []byte) string {
	var c struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if json.Unmarshal(body, &c) != nil {
		return "working…"
	}
	line := c.Name
	if w, ok := c.Arguments["worker"].(string); ok {
		line += " on " + w
	}
	for _, k := range []string{"command", "code", "path"} {
		if v, ok := c.Arguments[k].(string); ok {
			first, _, _ := strings.Cut(v, "\n")
			line += ": " + firstWords(first, 120)
			break
		}
	}
	return line
}

func firstWords(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}

// split cuts text into pieces of at most max bytes at line breaks where it can,
// closing and reopening a code fence that a cut falls inside.
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
			cut = max
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
