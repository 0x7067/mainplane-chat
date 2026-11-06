package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// discord is the chat adapter for a private Discord server. Its threads are
// "discord:<channel id>".
type discord struct {
	b        *bridge
	dc       *discordgo.Session
	tester   string      // a bot user id whose messages count as a person's, for end-to-end tests
	work     chan func() // the events' work, done in order by one goroutine
	mu       sync.Mutex  // guards sessions
	sessions string      // the #sessions channel, where sessions made outside the chats get threads
}

var errNoSessions = errors.New("no #sessions channel")

func newDiscord(b *bridge, token, tester string) *discord {
	dc, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}
	dc.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
	d := &discord{b: b, dc: dc, tester: tester, work: make(chan func(), 256)}
	// Handlers run in the gateway's goroutine, in order, and queue their work
	// for one goroutine: messages are handled in order, catching up on Ready
	// finishes before the next message is handled, and a slow message (a start
	// can wait a minute on the catalog) never delays the gateway, so a button
	// is acked within Discord's 3 seconds.
	dc.SyncEvents = true
	dc.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) { d.work <- func() { d.handle(m.Message) } })
	dc.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) { d.work <- func() { d.catchUp(r) } })
	dc.AddHandler(d.onInteraction)
	return d
}

func (d *discord) open() {
	go func() {
		for f := range d.work {
			f()
		}
	}()
	if err := d.dc.Open(); err != nil {
		log.Fatal(err)
	}
	log.Print("connected")
}

func (d *discord) key(id string) string { return "discord:" + id }

func (d *discord) id(key string) string { return strings.TrimPrefix(key, "discord:") }

// catchUp follows the sessions of the active threads from the last record
// relayed, then handles the messages sent while the bot was down. A channel
// the bot has not seen before starts from now.
func (d *discord) catchUp(r *discordgo.Ready) {
	for _, g := range r.Guilds {
		var chans []*discordgo.Channel
		if threads, err := d.dc.GuildThreadsActive(g.ID); err == nil {
			for _, t := range threads.Threads {
				th := d.key(t.ID)
				if id := cmp.Or(d.b.sessionFor(th), sessionOf(t.Name)); id != "" {
					d.b.resume(th, id)
				}
				chans = append(chans, t)
			}
		}
		if cs, err := d.dc.GuildChannels(g.ID); err == nil {
			for _, c := range cs {
				if c.Type == discordgo.ChannelTypeGuildText {
					chans = append(chans, c)
					if c.Name == "sessions" {
						d.mu.Lock()
						d.sessions = c.ID
						d.mu.Unlock()
					}
				}
			}
		}
		for _, c := range chans {
			d.replay(c)
		}
	}
	log.Print("caught up")
}

// replay handles the messages in c after the newest one handled there.
func (d *discord) replay(c *discordgo.Channel) {
	after, ok := d.b.lastSeen(d.key(c.ID))
	if !ok {
		if c.LastMessageID != "" {
			d.b.fresh(d.key(c.ID), c.LastMessageID)
		}
		return
	}
	for {
		msgs, err := d.dc.ChannelMessages(c.ID, 100, "", after, "")
		if err != nil || len(msgs) == 0 {
			return
		}
		sort.Slice(msgs, func(i, j int) bool { return newer(msgs[j].ID, msgs[i].ID) })
		for _, m := range msgs {
			d.handle(m)
		}
		after = msgs[len(msgs)-1].ID
		if len(msgs) < 100 {
			return
		}
	}
}

func (d *discord) handle(m *discordgo.Message) {
	if !d.b.fresh(d.key(m.ChannelID), m.ID) || m.Author == nil || m.Author.Bot && m.Author.ID != d.tester {
		return
	}
	text := strings.TrimSpace(m.Content)
	if text == "" && len(m.Attachments) == 0 {
		return
	}
	ch := d.channel(m.ChannelID)
	if ch == nil {
		return
	}
	var files []file
	for _, a := range m.Attachments {
		files = append(files, file{name: a.Filename, ctype: a.ContentType, size: a.Size, fetch: func() ([]byte, error) { return download(a.URL) }})
	}
	if !ch.IsThread() {
		label := text
		if label == "" {
			label = m.Attachments[0].Filename // a message with only files
		}
		open := func(id string) (string, error) {
			th, err := d.dc.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{Name: id + " " + firstWords(label, 80), AutoArchiveDuration: 10080})
			if err != nil {
				return "", err
			}
			return d.key(th.ID), nil
		}
		if err := d.b.start("discord", open, m.ID, "", text, files); err != nil {
			d.say(d.key(m.ChannelID), msg{Text: err.Error()})
		}
		return
	}
	th := d.key(ch.ID)
	if id := cmp.Or(d.b.sessionFor(th), sessionOf(ch.Name)); id != "" {
		d.b.message(th, id, m.ID, text, files)
	}
}

// onInteraction runs a button or select as the thread command it names:
// "stop:<id>" is !stop on session id, and "model:<id>" picking m is
// !model m. The message the component is on stands for the person's. It is
// acked here, in the gateway's goroutine, and run in turn with the messages.
func (d *discord) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
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
	d.work <- func() { d.b.command(d.key(i.ChannelID), id, i.Message.ID, "!"+action) }
}

// mirror makes a thread in #sessions for a session made outside the chats
// and returns its key.
func (d *discord) mirror(id, from, model, first string) (string, error) {
	d.mu.Lock()
	channel := d.sessions
	d.mu.Unlock()
	if channel == "" {
		return "", errNoSessions
	}
	m, err := d.dc.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Content:         fmt.Sprintf("`%s` · via %s · %s", id, from, model),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		return "", err
	}
	th, err := d.dc.MessageThreadStartComplex(channel, m.ID, &discordgo.ThreadStart{Name: id + " " + firstWords(first, 80), AutoArchiveDuration: 10080})
	if err != nil {
		return "", err
	}
	return d.key(th.ID), nil
}

// say sends m in Discord-sized pieces, link previews off. Its note lines are
// subtext, its files and controls come with the last piece, and a reply
// notifies only when m says so.
func (d *discord) say(th string, m msg) error {
	var errs []error
	ch := d.id(th)
	content := m.Text
	if m.Note != "" {
		content = strings.TrimSpace(content + "\n-# " + strings.ReplaceAll(m.Note, "\n", "\n-# "))
	}
	mentions := &discordgo.MessageAllowedMentions{RepliedUser: m.Notify}
	var ref *discordgo.MessageReference
	switch {
	case !snowflake(m.ReplyTo):
	case m.ReplyTo != ch:
		ref = &discordgo.MessageReference{MessageID: m.ReplyTo, ChannelID: ch}
	case m.Notify:
		// A thread's id is its starter message's, which is in the parent
		// channel, and Discord refuses a reply across channels: mention.
		if t := d.channel(ch); t != nil {
			if s, err := d.dc.ChannelMessage(t.ParentID, ch); err == nil {
				content = s.Author.Mention() + " " + content
				mentions.Users = []string{s.Author.ID}
			}
		}
	}
	var files []*discordgo.File
	for _, f := range m.Files {
		data, err := f.fetch()
		if err != nil {
			log.Printf("attach %s: %v", f.name, err)
			errs = append(errs, err)
			continue
		}
		files = append(files, &discordgo.File{Name: f.name, Reader: bytes.NewReader(data)})
	}
	comps := controls(m)
	parts := split(content, 1900)
	if len(parts) == 0 && (len(files) > 0 || len(comps) > 0) {
		parts = []string{""}
	}
	for i, part := range parts {
		s := &discordgo.MessageSend{Content: part, AllowedMentions: mentions, Flags: discordgo.MessageFlagsSuppressEmbeds}
		if i == 0 {
			s.Reference = ref
		}
		if i == len(parts)-1 {
			s.Files, s.Components = files, comps
		}
		if _, err := d.dc.ChannelMessageSendComplex(ch, s); err != nil {
			log.Printf("discord send: %v", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// controls are m's Retry button and model picker, which name the thread
// command they run in their custom id. A select shows at most 25 options.
func controls(m msg) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	if m.Retry {
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Retry", Style: discordgo.PrimaryButton, CustomID: "retry:" + m.Session},
		}})
	}
	var opts []discordgo.SelectMenuOption
	for _, o := range m.Models[:min(len(m.Models), 25)] {
		opts = append(opts, discordgo.SelectMenuOption{Label: o.Name, Value: o.ID, Description: o.ID, Default: o.Current})
	}
	if len(opts) > 0 {
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: "model:" + m.Session, Placeholder: "Continue on another model", Options: opts},
		}})
	}
	return rows
}

// status posts or edits the turn's status message: when it started, in a
// timestamp Discord keeps live, the calls, and a Stop button. Done, it
// shrinks to one line. Each update sends typing, which shows for 10 seconds.
func (d *discord) status(th, id string, t *turn) {
	ch := d.id(th)
	if t.done && t.status == "" {
		return
	}
	if t.done {
		line := fmt.Sprintf("-# %s · %s", calls(t.calls), t.took)
		if t.outcome != "" {
			line = fmt.Sprintf("-# %s after %s · %s", t.outcome, calls(t.calls), t.took)
		} else if t.used >= 0 {
			line += fmt.Sprintf(" · %d%% context", t.used)
		}
		none := []discordgo.MessageComponent{}
		if _, err := d.dc.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: t.status, Channel: ch, Content: &line, Components: &none}); err != nil {
			log.Printf("status %s: %v", id, err)
		}
		return
	}
	_ = d.dc.ChannelTyping(ch)
	content := fmt.Sprintf("**working** · started <t:%d:R> · %s", t.start.Unix(), calls(t.calls))
	for _, k := range t.tasks[max(0, len(t.tasks)-2):] {
		content += "\n-# " + k.title
		if k.detail != "" {
			content += ": " + firstWords(k.detail, 120)
		}
	}
	comps := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Stop", Style: discordgo.SecondaryButton, CustomID: "stop:" + id},
	}}}
	if t.status == "" {
		m, err := d.dc.ChannelMessageSendComplex(ch, &discordgo.MessageSend{
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
	if _, err := d.dc.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: t.status, Channel: ch, Content: &content, Components: &comps}); err != nil {
		log.Printf("status %s: %v", id, err)
	}
}

// react sets the bot's reaction on message msgID, sent in the thread, or in
// its parent channel when it is the thread's starter, whose id the thread
// has; "" is the starter. The new reaction replaces the bot's others, so the
// start messages in a channel show at a glance how each session's last turn
// went, the mirrored ones in #sessions too.
func (d *discord) react(th, msgID, emoji string) {
	ch := d.id(th)
	msgID = cmp.Or(msgID, ch)
	if !snowflake(msgID) {
		return
	}
	if msgID == ch {
		t := d.channel(ch)
		if t == nil {
			return
		}
		ch = t.ParentID
	}
	for _, e := range []string{seen, done, failed} {
		if e != emoji {
			_ = d.dc.MessageReactionRemove(ch, msgID, e, "@me")
		}
	}
	if err := d.dc.MessageReactionAdd(ch, msgID, emoji); err != nil {
		log.Printf("react %s: %v", msgID, err)
	}
}

// rename names the thread "<id> <name>". Past Discord's limit of about two
// renames per ten minutes, discordgo waits for the limit to pass.
func (d *discord) rename(th, id, name string) {
	if _, err := d.dc.ChannelEdit(d.id(th), &discordgo.ChannelEdit{Name: threadTitle(id, name)}); err != nil {
		log.Printf("rename thread %s: %v", th, err)
	}
}

// close archives and locks the thread. A thread in #sessions loses its post
// too, so the channel lists the sessions the terminal app lists.
func (d *discord) close(th string) {
	ch := d.id(th)
	yes := true
	t, err := d.dc.ChannelEdit(ch, &discordgo.ChannelEdit{Archived: &yes, Locked: &yes})
	if err != nil {
		log.Printf("archive thread %s: %v", th, err)
		return
	}
	d.mu.Lock()
	mirrored := t.ParentID == d.sessions
	d.mu.Unlock()
	if mirrored {
		if err := d.dc.ChannelMessageDelete(t.ParentID, ch); err != nil {
			log.Printf("delete post %s: %v", th, err)
		}
	}
}

// channel is the channel or thread with Discord id ch, from the gateway's
// cache when it has it, or nil.
func (d *discord) channel(ch string) *discordgo.Channel {
	c, err := d.dc.State.Channel(ch)
	if err != nil {
		if c, err = d.dc.Channel(ch); err != nil {
			log.Printf("channel %s: %v", ch, err)
			return nil
		}
	}
	return c
}

func download(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// snowflake is whether s is a Discord id, as a message's idempotency key is
// when the bot posted it for a Discord message.
func snowflake(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// sessionOf is the session id a thread name starts with, or "".
func sessionOf(name string) string {
	id, _, _ := strings.Cut(name, " ")
	if statefile.ValidID(id) {
		return id
	}
	return ""
}
