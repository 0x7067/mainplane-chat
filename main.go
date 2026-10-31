// mainplane-discord drives Mainplane agents from a private Discord server. A
// message in any channel starts a session in a new thread named after it;
// messages in that thread continue it, and the agent's replies come back there.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

const via = "discord"

type bridge struct {
	dc      *discordgo.Session
	harness string // base URL of the harness API
	key     string

	mu        sync.Mutex
	following map[string]bool // session ids with a follower running
}

func main() {
	b := &bridge{
		harness:   envOr("MAINPLANE_URL", "http://127.0.0.1:8080"),
		key:       secret(mustEnv("MAINPLANE_KEY")),
		following: map[string]bool{},
	}
	dc, err := discordgo.New("Bot " + mustEnv("DISCORD_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	dc.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
	dc.AddHandler(b.onMessage)
	dc.AddHandler(b.onReady)
	b.dc = dc
	if err := dc.Open(); err != nil {
		log.Fatal(err)
	}
	defer dc.Close()
	log.Print("connected")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}

// onReady resumes following the sessions of the active threads.
func (b *bridge) onReady(s *discordgo.Session, r *discordgo.Ready) {
	for _, g := range r.Guilds {
		threads, err := s.GuildThreadsActive(g.ID)
		if err != nil {
			continue
		}
		for _, t := range threads.Threads {
			if id := sessionOf(t.Name); id != "" {
				if info, err := b.info(id); err == nil {
					b.follow(t.ID, id, info.N)
				}
			}
		}
	}
}

func (b *bridge) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}
	ch, err := s.State.Channel(m.ChannelID)
	if err != nil {
		if ch, err = s.Channel(m.ChannelID); err != nil {
			return
		}
	}
	if !ch.IsThread() {
		b.start(m.Message, text)
		return
	}
	id := sessionOf(ch.Name)
	if id == "" {
		return
	}
	if text == "!stop" {
		if err := b.call("POST", "/sessions/"+id+"/stop?via="+via, "", nil, nil); err != nil {
			b.say(ch.ID, "stop failed: "+err.Error())
		}
		return
	}
	b.post(ch.ID, id, m.ID, text)
}

// start makes a session like the newest one, on every connected worker, and a
// thread for it named "<session id> <first words>".
func (b *bridge) start(m *discordgo.Message, text string) {
	conf, err := b.newConf()
	if err != nil {
		b.reply(m, "could not start a session: "+err.Error())
		return
	}
	var created struct{ ID string }
	body, _ := json.Marshal(conf)
	if err := b.call("POST", "/sessions?via="+via, "application/json", bytes.NewReader(body), &created); err != nil {
		b.reply(m, "could not start a session: "+err.Error())
		return
	}
	name := created.ID + " " + firstWords(text, 80)
	th, err := b.dc.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{Name: name, AutoArchiveDuration: 10080})
	if err != nil {
		b.reply(m, "session "+created.ID+" started, but the thread failed: "+err.Error())
		return
	}
	b.post(th.ID, created.ID, m.ID, text)
}

// newConf copies the newest session's model and settings and gives the new
// session every connected worker.
func (b *bridge) newConf() (map[string]any, error) {
	var infos []struct {
		Config statefile.Conf `json:"config"`
	}
	if err := b.call("GET", "/sessions?limit=1", "", nil, &infos); err != nil {
		return nil, err
	}
	conf := map[string]any{"model": "claude-code/claude-opus-5-5", "tools": "default", "context_limit": 250000,
		"params": map[string]any{"effort": "medium"}, "input": []string{"image/jpeg", "image/png", "image/gif", "image/webp"}}
	if len(infos) > 0 {
		c := infos[0].Config
		conf = map[string]any{"model": c.Model, "tools": c.Tools, "context_limit": c.ContextLimit, "params": c.Params, "input": c.Input, "preset": c.Preset}
	}
	var workers []struct {
		Name    string `json:"name"`
		Refused string `json:"refused"`
	}
	if err := b.call("GET", "/workers", "", nil, &workers); err != nil {
		return nil, err
	}
	var all []map[string]string
	for _, w := range workers {
		if w.Refused == "" {
			all = append(all, map[string]string{"name": w.Name})
		}
	}
	conf["workers"] = all
	return conf, nil
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
	b.follow(thread, id, out.N)
}

// follow long-polls the session after record n and relays what the agent says.
func (b *bridge) follow(thread, id string, n int) {
	b.mu.Lock()
	if b.following[id] {
		b.mu.Unlock()
		return
	}
	b.following[id] = true
	b.mu.Unlock()
	go func() {
		defer func() { b.mu.Lock(); delete(b.following, id); b.mu.Unlock() }()
		for {
			recs, err := b.records(id, n)
			if err != nil {
				log.Printf("session %s: %v", id, err)
				time.Sleep(5 * time.Second)
				continue
			}
			for _, r := range recs {
				n = r.N
				switch r.Kind {
				case statefile.Text:
					b.say(thread, string(r.Body))
				case statefile.Error:
					b.say(thread, "⚠️ "+string(r.Body))
				case statefile.Call:
					_ = b.dc.ChannelTyping(thread)
					b.say(thread, "-# "+callLine(r.Body))
				}
			}
		}
	}()
}

func (b *bridge) records(id string, after int) ([]statefile.Record, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/sessions/%s/records?after=%d&wait=60s", b.harness, id, after), nil)
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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

func (b *bridge) info(id string) (struct{ N int }, error) {
	var i struct{ N int }
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
func (b *bridge) say(channel, text string) {
	for _, part := range split(text, 1900) {
		_, err := b.dc.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
			Content:         part,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
			Flags:           discordgo.MessageFlagsSuppressEmbeds,
		})
		if err != nil {
			log.Printf("discord send: %v", err)
		}
	}
}

func (b *bridge) reply(m *discordgo.Message, text string) { b.say(m.ChannelID, text) }

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
