package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"html"
	"log"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// Slack ends a "processing" agent session after an hour; a longer turn says
// so again before then.
const keepProcessing = 50 * time.Minute

// slackChat is the chat adapter for Slack DMs with the app. A top-level DM
// message starts a session in its thread, which is a Slack agent session:
// Slack shows its title, its "Working..." state and a Stop button. Its
// threads are "slack:<dm>/<thread ts>", its channels "slack:<dm>".
type slackChat struct {
	b     *bridge
	sl    *slack.Client
	sm    *socketmode.Client
	users map[string]bool // the Slack users the bot takes messages from
	self  string          // the bot's own Slack user
	work  chan func()     // the events' work, done in order by one goroutine

	mu         sync.Mutex
	processing map[string]time.Time // thread -> when Slack was last told it is processing
}

func newSlack(b *bridge, botToken, appToken, users string) *slackChat {
	s := &slackChat{b: b, users: map[string]bool{}, work: make(chan func(), 256), processing: map[string]time.Time{}}
	for _, u := range strings.Split(users, ",") {
		s.users[strings.TrimSpace(u)] = true
	}
	s.sl = slack.New(botToken, slack.OptionAppLevelToken(appToken), slack.OptionRetry(3))
	me, err := s.sl.AuthTest()
	if err != nil {
		log.Fatalf("slack: %v", err)
	}
	s.self = me.UserID
	s.sm = socketmode.New(s.sl)
	return s
}

// open follows the sessions of the Slack threads from the last record
// relayed, and handles Slack's events.
func (s *slackChat) open() {
	s.b.mu.Lock()
	threads := map[string]string{}
	for th, t := range s.b.st.Threads {
		if platform(th) == "slack" {
			threads[th] = t.Session
		}
	}
	s.b.mu.Unlock()
	for th, id := range threads {
		s.b.resume(th, id)
	}
	go func() {
		for f := range s.work {
			f()
		}
	}()
	go s.run()
	go func() { log.Fatalf("slack: %v", s.sm.Run()) }()
}

// run acks Slack's events at once, within Slack's 3 seconds, and queues
// their work, which is done one at a time, in order: catching up on connect
// finishes before the next message is handled.
func (s *slackChat) run() {
	for evt := range s.sm.Events {
		switch evt.Type {
		case socketmode.EventTypeConnected:
			log.Print("slack connected")
			s.work <- s.catchUp
		case socketmode.EventTypeConnectionError:
			log.Printf("slack: %v", evt.Data)
		case socketmode.EventTypeEventsAPI:
			s.sm.Ack(*evt.Request)
			e, ok := evt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				continue
			}
			switch ev := e.InnerEvent.Data.(type) {
			case *slackevents.MessageEvent:
				s.work <- func() { s.handle(ev) }
			case *slackevents.AgentSessionStoppedEvent:
				s.work <- func() { s.stopped(ev) }
			case *slackevents.AgentSessionTitleChangedEvent:
				s.work <- func() { s.renamed(ev) }
			}
		case socketmode.EventTypeInteractive:
			s.sm.Ack(*evt.Request)
			if cb, ok := evt.Data.(slack.InteractionCallback); ok && cb.Type == slack.InteractionTypeBlockActions {
				s.work <- func() { s.act(cb) }
			}
		}
	}
}

// act runs a Retry button or model picker as the thread command it is: the
// actions block's id is the session, so "retry" is !retry on it and "model"
// picking m is !model m. The message the control is on stands for the
// person's.
func (s *slackChat) act(cb slack.InteractionCallback) {
	if !s.users[cb.User.ID] {
		return
	}
	th := s.thread(cb.Container.ChannelID, cb.Container.ThreadTs)
	for _, a := range cb.ActionCallback.BlockActions {
		text := "!" + a.ActionID
		if a.ActionID == "model" {
			text += " " + a.SelectedOption.Value
		}
		s.b.command(th, a.BlockID, cb.Container.MessageTs, text)
	}
}

func (s *slackChat) key(ch string) string { return "slack:" + ch }

func (s *slackChat) thread(ch, ts string) string { return "slack:" + ch + "/" + ts }

// threadParts is the DM and thread ts of thread key th.
func threadParts(th string) (channel, ts string) {
	channel, ts, _ = strings.Cut(strings.TrimPrefix(th, "slack:"), "/")
	return channel, ts
}

func (s *slackChat) handle(m *slackevents.MessageEvent) {
	if m.ChannelType != "im" || !slices.Contains([]string{"", "file_share", "thread_broadcast"}, m.SubType) || !s.b.fresh(s.key(m.Channel), m.TimeStamp) {
		return
	}
	// A user's message sent through another app, as slack-axi sends them, has
	// that app's bot_id and is still the user's.
	if m.User == s.self {
		return
	}
	if !s.users[m.User] {
		log.Printf("ignored a message from %s, who is not in MAINPLANE_SLACK_USERS", m.User)
		return
	}
	text := plain(m.Text)
	var files []file
	if m.Message != nil {
		files = s.files(m.Message.Files)
	}
	if text == "" && len(files) == 0 {
		return
	}
	if m.ThreadTimeStamp == "" || m.ThreadTimeStamp == m.TimeStamp {
		th := s.thread(m.Channel, m.TimeStamp)
		// The thread is the message's; the agent session's title is its first
		// words, or its first file's name.
		label := text
		if label == "" {
			label = files[0].name // a message with only files
		}
		title := firstWords(label, 80)
		open := func(string) (string, error) {
			if _, err := s.sl.SetAgentSessionStatus(slack.AgentSessionSetStatusParameters{Status: "processing", ChannelID: m.Channel, ThreadTS: m.TimeStamp, Title: title}); err != nil {
				log.Printf("session status %s: %v", th, err)
			}
			return th, nil
		}
		if err := s.b.start("slack", open, m.TimeStamp, text, files); err != nil {
			s.say(th, msg{Text: err.Error()})
		}
		return
	}
	th := s.thread(m.Channel, m.ThreadTimeStamp)
	if id := s.b.sessionFor(th); id != "" {
		s.b.message(th, id, m.TimeStamp, text, files)
	}
}

// files are a message's files, downloaded with the bot token when needed. A
// remote file (mode external) lives outside Slack: the token never goes there.
func (s *slackChat) files(fs []slack.File) []file {
	var out []file
	for _, f := range fs {
		out = append(out, file{name: f.Name, ctype: f.Mimetype, size: f.Size, fetch: func() ([]byte, error) {
			if f.Mode == "external" || f.URLPrivateDownload == "" {
				return nil, errors.New("not a file uploaded to Slack")
			}
			var buf bytes.Buffer
			err := s.sl.GetFile(f.URLPrivateDownload, &buf)
			return buf.Bytes(), err
		}})
	}
	return out
}

// catchUp handles the messages sent while the bot was down, in the DMs it
// has seen before: new top-level messages, and new replies in threads whose
// first message is from the last 30 days. Slack does not send events again
// once a connection is gone. Reading a DM that fails for a while, as when
// offline or rate limited, is tried again before any new event is handled:
// handling only some messages, or a new one, would mark the rest handled.
// A DM that is gone is forgotten.
func (s *slackChat) catchUp() {
	s.b.mu.Lock()
	seen := map[string]string{}
	for k, ts := range s.b.st.Seen {
		if platform(k) == "slack" {
			seen[strings.TrimPrefix(k, "slack:")] = ts
		}
	}
	s.b.mu.Unlock()
	for ch, after := range seen {
		missed, err := s.missed(ch, after)
		for ; err != nil; missed, err = s.missed(ch, after) {
			var se slack.SlackErrorResponse
			if errors.As(err, &se) && slices.Contains([]string{"channel_not_found", "not_in_channel", "is_archived"}, se.Err) {
				break
			}
			log.Printf("catch up %s: %v; trying again", ch, err)
			time.Sleep(10 * time.Second)
		}
		if err != nil {
			log.Printf("catch up %s: %v; forgetting it", ch, err)
			s.b.forget(s.key(ch))
			continue
		}
		slices.SortFunc(missed, func(x, y slack.Message) int { return strings.Compare(x.Timestamp, y.Timestamp) })
		for _, m := range missed {
			s.handle(&slackevents.MessageEvent{
				Channel: ch, ChannelType: "im", User: m.User, Text: m.Text, SubType: m.SubType,
				TimeStamp: m.Timestamp, ThreadTimeStamp: m.ThreadTimestamp, Message: &m.Msg,
			})
		}
	}
	log.Print("slack caught up")
}

// missed is the messages in DM ch after ts after that catchUp handles.
func (s *slackChat) missed(ch, after string) ([]slack.Message, error) {
	var missed []slack.Message
	cutoff := fmt.Sprintf("%d.000000", time.Now().Add(-30*24*time.Hour).Unix())
	params := slack.GetConversationHistoryParameters{ChannelID: ch, Oldest: cutoff, Limit: 200}
	for {
		h, err := s.sl.GetConversationHistory(&params)
		if err != nil {
			return nil, err
		}
		for _, m := range h.Messages {
			if newer(m.Timestamp, after) {
				missed = append(missed, m)
			}
			if m.ReplyCount > 0 && newer(m.LatestReply, after) {
				rs, err := s.replies(ch, m.Timestamp, after)
				if err != nil {
					return nil, err
				}
				missed = append(missed, rs...)
			}
		}
		if params.Cursor = h.ResponseMetaData.NextCursor; params.Cursor == "" {
			return missed, nil
		}
	}
}

// replies are the replies in a thread after ts after.
func (s *slackChat) replies(ch, thread, after string) ([]slack.Message, error) {
	var out []slack.Message
	params := slack.GetConversationRepliesParameters{ChannelID: ch, Timestamp: thread, Oldest: after, Limit: 200}
	for {
		msgs, more, cursor, err := s.sl.GetConversationReplies(&params)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			// A broadcast reply shows in the DM's history too: handle it once.
			if m.Timestamp != thread && newer(m.Timestamp, after) && m.SubType != "thread_broadcast" {
				out = append(out, m)
			}
		}
		if !more || cursor == "" {
			return out, nil
		}
		params.Cursor = cursor
	}
}

// stopped is Slack's Stop button: it stops the thread's session, whose error
// record then ends the turn.
func (s *slackChat) stopped(ev *slackevents.AgentSessionStoppedEvent) {
	th := s.thread(ev.Channel, ev.ThreadTimestamp)
	id := s.b.sessionFor(th)
	if id == "" || !s.users[ev.User] {
		return
	}
	if err := s.b.call("POST", "/sessions/"+id+"/stop?via=slack", "", nil, nil); err != nil {
		s.say(th, msg{Text: "stop failed: " + err.Error()})
		s.setStatus(th, "active")
	}
}

// renamed gives a title set in Slack to the thread's session as its name.
func (s *slackChat) renamed(ev *slackevents.AgentSessionTitleChangedEvent) {
	id := s.b.sessionFor(s.thread(ev.Channel, ev.ThreadTimestamp))
	if id == "" || !s.users[ev.User] {
		return
	}
	if err := s.b.call("PUT", "/sessions/"+id+"/name?via=slack", "text/plain; charset=utf-8", strings.NewReader(ev.Title), nil); err != nil {
		log.Printf("name %s: %v", id, err)
	}
}

// say sends m to the thread: its text as Markdown, in pieces within a
// markdown block's limit, its files, then its note small, with its Retry
// button and model picker. A failure that offers Retry suspends the agent
// session, as it waits for the user.
func (s *slackChat) say(th string, m msg) {
	channel, ts := threadParts(th)
	for _, part := range split(strings.TrimSpace(m.Text), 11000) {
		s.post(channel, ts, firstWords(part, 300), slack.NewMarkdownBlock("", part))
	}
	note := m.Note
	for _, f := range m.Files {
		data, err := f.fetch()
		if err == nil {
			_, err = s.sl.UploadFile(slack.UploadFileParameters{Reader: bytes.NewReader(data), FileSize: len(data), Filename: f.name, Channel: channel, ThreadTimestamp: ts})
		}
		if err != nil {
			note = strings.TrimSpace(note + "\ncould not attach " + f.name + ": " + err.Error())
		}
	}
	var blocks []slack.Block
	if note != "" {
		blocks = append(blocks, slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, note, false, false)))
	}
	if c := slackControls(m); c != nil {
		blocks = append(blocks, c)
	}
	if len(blocks) > 0 {
		s.post(channel, ts, firstWords(cmp.Or(note, "Retry or continue on another model"), 300), blocks...)
	}
	if m.Retry {
		s.setStatus(th, "suspended")
	}
}

// slackControls is m's Retry button and model picker, in an actions block
// whose id is the session they act on, or nil. A select shows at most 100
// options of at most 75 characters.
func slackControls(m msg) slack.Block {
	var els []slack.BlockElement
	if m.Retry {
		els = append(els, slack.NewButtonBlockElement("retry", "retry", slack.NewTextBlockObject(slack.PlainTextType, "Retry", false, false)).WithStyle(slack.StylePrimary))
	}
	var opts []*slack.OptionBlockObject
	var current *slack.OptionBlockObject
	for _, o := range m.Models[:min(len(m.Models), 100)] {
		opt := slack.NewOptionBlockObject(o.ID, slack.NewTextBlockObject(slack.PlainTextType, firstWords(o.Name, 70), false, false), slack.NewTextBlockObject(slack.PlainTextType, firstWords(o.ID, 70), false, false))
		opts = append(opts, opt)
		if o.Current {
			current = opt
		}
	}
	if len(opts) > 0 {
		sel := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, slack.NewTextBlockObject(slack.PlainTextType, "Continue on another model", false, false), "model", opts...)
		if current != nil {
			sel.WithInitialOption(current)
		}
		els = append(els, sel)
	}
	if len(els) == 0 {
		return nil
	}
	return slack.NewActionBlock(m.Session, els...)
}

func (s *slackChat) post(channel, ts, fallback string, blocks ...slack.Block) {
	if _, _, err := s.sl.PostMessage(channel, slack.MsgOptionTS(ts), slack.MsgOptionText(fallback, false), slack.MsgOptionBlocks(blocks...)); err != nil {
		log.Printf("slack send: %v", err)
	}
}

// status posts or edits the turn's status message: when it started, in a
// date Slack shows in the reader's time zone, and the newest calls. Done, it
// shrinks to one line with the session id, which Slack's title does not show.
func (s *slackChat) status(th, id string, t *turn) {
	var text string
	if t.done {
		// The agent session is ready for the next message.
		s.setStatus(th, "active")
		if t.status == "" {
			return
		}
		text = fmt.Sprintf("`%s` · %s · %s", id, calls(t.calls), t.took)
		if t.outcome != "" {
			text = fmt.Sprintf("`%s` · %s after %s · %s", id, t.outcome, calls(t.calls), t.took)
		} else if t.used >= 0 {
			text += fmt.Sprintf(" · %d%% context", t.used)
		}
	} else {
		lines := []string{fmt.Sprintf("*working* · started <!date^%d^{time}|%s> · %s", t.start.Unix(), t.start.Format(time.Kitchen), calls(t.calls))}
		for _, l := range t.last {
			lines = append(lines, "`"+escape(strings.ReplaceAll(l, "`", "'"))+"`")
		}
		text = strings.Join(lines, "\n")
		s.mu.Lock()
		stale := time.Since(s.processing[th]) > keepProcessing
		s.mu.Unlock()
		if stale {
			s.setStatus(th, "processing")
		}
	}
	channel, ts := threadParts(th)
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, text, false, false))),
	}
	var err error
	if t.status == "" {
		_, t.status, err = s.sl.PostMessage(channel, append(opts, slack.MsgOptionTS(ts))...)
	} else {
		_, _, _, err = s.sl.UpdateMessage(channel, t.status, opts...)
	}
	if err != nil {
		log.Printf("status %s: %v", id, err)
	}
}

// react marks message msgID: seen sets the agent session processing, which
// Slack shows in place of a reaction; done and failed are reactions.
func (s *slackChat) react(th, msgID, emoji string) {
	if emoji == seen {
		s.setStatus(th, "processing")
		return
	}
	if !isTS(msgID) {
		return
	}
	name := map[string]string{done: "white_check_mark", failed: "x"}[emoji]
	channel, _ := threadParts(th)
	if err := s.sl.AddReaction(name, slack.NewRefToMessage(channel, msgID)); err != nil {
		log.Printf("react %s: %v", msgID, err)
	}
}

func (s *slackChat) rename(th, _, name string) {
	channel, ts := threadParts(th)
	if _, err := s.sl.RenameAgentSession(slack.AgentSessionRenameParameters{Title: firstWords(name, 100), ChannelID: channel, ThreadTS: ts}); err != nil {
		log.Printf("rename %s: %v", th, err)
	}
}

func (s *slackChat) close(th string) { s.setStatus(th, "closed") }

// setStatus sets the agent session of the thread.
func (s *slackChat) setStatus(th, status string) {
	s.mu.Lock()
	if status == "processing" {
		s.processing[th] = time.Now()
	} else {
		delete(s.processing, th)
	}
	s.mu.Unlock()
	channel, ts := threadParts(th)
	if _, err := s.sl.SetAgentSessionStatus(slack.AgentSessionSetStatusParameters{Status: status, ChannelID: channel, ThreadTS: ts}); err != nil {
		log.Printf("session status %s: %v", th, err)
	}
}

var tsPattern = regexp.MustCompile(`^\d+\.\d+$`)

// isTS is whether s is a Slack message ts, as a message's idempotency key is
// when the bot posted it for a Slack message.
func isTS(s string) bool { return tsPattern.MatchString(s) }

var link = regexp.MustCompile(`<([^<>|]+)(?:\|([^<>]+))?>`)

// plain is a Slack message's text as its author typed it: links and mentions
// unwrapped, entities decoded.
func plain(text string) string {
	text = link.ReplaceAllStringFunc(text, func(m string) string {
		s := link.FindStringSubmatch(m)
		if strings.HasPrefix(s[1], "http") || strings.HasPrefix(s[1], "mailto:") {
			return strings.TrimPrefix(s[1], "mailto:")
		}
		if s[2] != "" {
			return s[2]
		}
		return s[1]
	})
	return strings.TrimSpace(html.UnescapeString(text))
}

// escape makes text safe in Slack mrkdwn.
func escape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}
