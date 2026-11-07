package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
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

// slackChat is the chat adapter for Slack DMs with the app, and for the group
// DMs, private channels and channels it is in, where it hears only mentions
// and replies in its threads. A top-level message starts a session in its
// thread, which is a Slack agent session: Slack shows its title, its
// "Working..." state and a Stop button. Its threads are
// "slack:<channel>/<thread ts>", its channels "slack:<channel>".
type slackChat struct {
	b       *bridge
	sl      *slack.Client
	sm      *socketmode.Client
	users   map[string]bool // the Slack users the bot takes messages from
	members bool            // "*" in MAINPLANE_SLACK_USERS: also every full member of the bot's workspace
	team    string          // the bot's workspace
	self    string          // the bot's own Slack user
	name    string          // the bot's own name, as the agent is told it
	mention *regexp.Regexp  // the bot's mention in a message's text
	work    chan func()     // the events' work, done in order by one goroutine

	mu         sync.Mutex
	processing map[string]time.Time // thread -> when Slack was last told it is processing
	people     map[string]person    // user -> profile, cached for allowed and names
}

// person is a cached Slack profile.
type person struct {
	u  *slack.User
	at time.Time
}

func newSlack(b *bridge, botToken, appToken, users string) *slackChat {
	s := &slackChat{b: b, users: map[string]bool{}, work: make(chan func(), 256), processing: map[string]time.Time{}, people: map[string]person{}}
	for _, u := range strings.Split(users, ",") {
		if u = strings.TrimSpace(u); u == "*" {
			s.members = true
		} else if u != "" {
			s.users[u] = true
		}
	}
	s.sl = slack.New(botToken, slack.OptionAppLevelToken(appToken), slack.OptionRetry(3))
	me, err := s.sl.AuthTest()
	if err != nil {
		log.Fatalf("slack: %v", err)
	}
	s.self = me.UserID
	s.team = me.TeamID
	s.name = me.User
	s.mention = mentionOf(s.self)
	s.sm = socketmode.New(s.sl)
	if have, err := grantedScopes(botToken); err != nil {
		log.Printf("slack scopes: %v", err)
	} else {
		for _, l := range lacking(have) {
			log.Print("slack: " + l)
		}
	}
	return s
}

// scopeNeeds are the bot scopes slack-manifest.yaml asks for, each with what
// goes without it, so a deployment's app with fewer says at start what it lacks.
// Event subscriptions (message.channels, ...) are not visible to a bot token:
// an app with a history scope but not its event still hears nothing there.
var scopeNeeds = []struct{ scope, without string }{
	{"assistant:write", "threads are not agent sessions: no title, working state or Stop"},
	{"chat:write", "the bot cannot post"},
	{"im:history", "DMs never arrive"},
	{"channels:history", "public channel messages never arrive"},
	{"groups:history", "private channel messages never arrive"},
	{"mpim:history", "group DM messages never arrive"},
	{"users:read", `names show as user ids, and "*" lets in only listed users`},
	{"channels:read", "the agent is not told which public channel it is in"},
	{"groups:read", "the agent is not told which private channel it is in"},
	{"im:read", "the agent is not told it is in a DM"},
	{"mpim:read", "the agent is not told which group DM it is in, or who is in it"},
	{"files:read", "files sent with a message are not fetched"},
	{"files:write", "attach: lines do not upload"},
	{"reactions:write", "messages get no 👀, ✅ or ❌"},
}

// lacking is a line per needed scope missing from have, a token's
// comma-separated scopes.
func lacking(have string) []string {
	got := map[string]bool{}
	for _, sc := range strings.Split(have, ",") {
		got[strings.TrimSpace(sc)] = true
	}
	var out []string
	for _, n := range scopeNeeds {
		if !got[n.scope] {
			out = append(out, "the app lacks "+n.scope+": "+n.without)
		}
	}
	return out
}

// grantedScopes are the bot token's scopes, which Slack gives only in a
// response header.
func grantedScopes(token string) (string, error) {
	req, err := http.NewRequest("POST", "https://slack.com/api/auth.test", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	have := resp.Header.Get("X-OAuth-Scopes")
	if resp.StatusCode != http.StatusOK || have == "" {
		return "", fmt.Errorf("auth.test: HTTP %d, no X-OAuth-Scopes", resp.StatusCode)
	}
	return have, nil
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
	if !s.allowed(cb.User.ID) {
		return
	}
	th := s.thread(cb.Container.ChannelID, cb.Container.ThreadTs)
	for _, a := range cb.ActionCallback.BlockActions {
		if a.BlockID != s.b.sessionFor(th) {
			s.say(th, msg{Note: "This control belongs to a previous session. Use !retry or !model in this thread for the current session."})
			continue
		}
		text := "!" + a.ActionID
		if a.ActionID == "model" {
			text += " " + a.SelectedOption.Value
		}
		s.b.command(th, a.BlockID, cb.Container.MessageTs, text)
	}
}

func (s *slackChat) key(ch string) string { return "slack:" + ch }

func (s *slackChat) thread(ch, ts string) string { return "slack:" + ch + "/" + ts }

// threadParts is the channel and thread ts of thread key th.
func threadParts(th string) (channel, ts string) {
	channel, ts, _ = strings.Cut(strings.TrimPrefix(th, "slack:"), "/")
	return channel, ts
}

// mentionOf matches the bot's mention in a message's text, <@U…>, with the
// name Slack may add after a |.
func mentionOf(self string) *regexp.Regexp {
	return regexp.MustCompile(`<@` + regexp.QuoteMeta(self) + `(?:\|[^>]*)?>`)
}

// chatType is the type of a Slack conversation from its id, as a message
// event names it: "im" for a DM, "group" for a private channel or group DM,
// "channel" otherwise.
func chatType(ch string) string {
	switch {
	case strings.HasPrefix(ch, "D"):
		return "im"
	case strings.HasPrefix(ch, "G"):
		return "group"
	}
	return "channel"
}

// hears is whether the bot takes a message in a conversation of type chType.
// Every DM message is for it; in the others, only a message that mentions it,
// even in a thread it runs, so people can talk in its threads without the
// agent answering each message.
func hears(chType string, mentioned bool) bool {
	return chType == "im" || mentioned
}

// handle takes a message event it hears. A top-level message starts a session
// in its thread, and a reply continues the session its thread shows. In a
// channel, a mention in a thread the bot does not run starts one in that thread.
func (s *slackChat) handle(m *slackevents.MessageEvent) {
	if !slices.Contains([]string{"", "file_share", "thread_broadcast"}, m.SubType) {
		return
	}
	root := cmp.Or(m.ThreadTimeStamp, m.TimeStamp)
	th := s.thread(m.Channel, root)
	if !hears(m.ChannelType, s.mention.MatchString(m.Text)) || !s.b.fresh(s.key(m.Channel), m.TimeStamp) {
		return
	}
	// A user's message sent through another app, as slack-axi sends them, has
	// that app's bot_id and is still the user's.
	if m.User == s.self {
		return
	}
	if !s.allowed(m.User) {
		log.Printf("ignored a message from %s, who is not allowed by MAINPLANE_SLACK_USERS", m.User)
		return
	}
	text := plain(s.mention.ReplaceAllString(m.Text, ""))
	var files []file
	if m.Message != nil {
		files = s.files(m.Message.Files)
	}
	if text == "" && len(files) == 0 {
		return
	}
	if id := s.b.sessionFor(th); id != "" {
		if !s.b.command(th, id, m.TimeStamp, text) {
			s.b.post(th, id, m.TimeStamp, s.framed(m.Channel, m.User, false), text, files)
		}
		return
	}
	// A reply in a DM's thread that the bot does not run is not for it.
	if root != m.TimeStamp && m.ChannelType == "im" {
		return
	}
	// The thread is the message's, or the one it is in; the agent session's
	// title is its first words, or its first file's name, and its initiator
	// the message's author, who Slack subscribes to the session.
	label := text
	if label == "" {
		label = files[0].name // a message with only files
	}
	title := firstWords(label, 80)
	open := func(string) (string, error) {
		if _, err := s.sl.SetAgentSessionStatus(slack.AgentSessionSetStatusParameters{Status: "processing", ChannelID: m.Channel, ThreadTS: root, Title: title, InitiatorUserID: m.User}); err != nil {
			log.Printf("session status %s: %v", th, err)
		}
		return th, nil
	}
	if err := s.b.start("slack", open, m.TimeStamp, s.framed(m.Channel, m.User, true), text, files); err != nil {
		s.say(th, msg{Text: err.Error()})
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

// catchUp handles the messages sent while the bot was down, in the
// conversations it has seen before: new top-level messages, and new replies
// in threads whose first message is from the last 30 days. Slack does not
// send events again once a connection is gone. Reading a conversation that
// fails for a while, as when offline or rate limited, is tried again before
// any new event is handled: handling only some messages, or a new one, would
// mark the rest handled. A conversation that is gone is forgotten.
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
				Channel: ch, ChannelType: chatType(ch), User: m.User, Text: m.Text, SubType: m.SubType,
				TimeStamp: m.Timestamp, ThreadTimeStamp: m.ThreadTimestamp, Message: &m.Msg,
			})
		}
	}
	log.Print("slack caught up")
}

// missed is the messages in conversation ch after ts after that catchUp handles.
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
	if id == "" || !s.allowed(ev.User) {
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
	if id == "" || !s.allowed(ev.User) {
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
func (s *slackChat) say(th string, m msg) error {
	var errs []error
	channel, ts := threadParts(th)
	for _, part := range split(strings.TrimSpace(m.Text), 11000) {
		if err := s.post(channel, ts, firstWords(part, 300), slack.NewMarkdownBlock("", part)); err != nil {
			errs = append(errs, err)
		}
	}
	note := m.Note
	for _, f := range m.Files {
		data, err := f.fetch()
		if err == nil {
			thread := ts
			if f.topLevel {
				thread = ""
			}
			var uploaded *slack.FileSummary
			uploaded, err = s.sl.UploadFile(slack.UploadFileParameters{Reader: bytes.NewReader(data), FileSize: len(data), Filename: f.name, Channel: channel, ThreadTimestamp: thread})
			if err == nil && f.topLevel {
				receipt := "Uploaded " + html.EscapeString(f.name) + " as a top-level file in this channel."
				if info, _, _, lookupErr := s.sl.GetFileInfo(uploaded.ID, 1, 1); lookupErr == nil && info.Permalink != "" {
					receipt += " <" + info.Permalink + "|Open file>"
				}
				note = strings.TrimSpace(note + "\n" + receipt)
			}
		}
		if err != nil {
			note = strings.TrimSpace(note + "\ncould not attach " + f.name + ": " + err.Error())
			errs = append(errs, err)
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
		if err := s.post(channel, ts, firstWords(cmp.Or(note, "Retry or continue on another model"), 300), blocks...); err != nil {
			errs = append(errs, err)
		}
	}
	if m.Retry {
		s.setStatus(th, "suspended")
	}
	return errors.Join(errs...)
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

func (s *slackChat) post(channel, ts, fallback string, blocks ...slack.Block) error {
	_, _, err := s.sl.PostMessage(channel, slack.MsgOptionTS(ts), slack.MsgOptionText(fallback, false), slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("slack send: %v", err)
	}
	return err
}

// status posts or edits the turn's status message: a plan block with a task
// for each call, which shows whether it runs, is done or failed. Done, its
// title says how the turn went, with the session id, which Slack's title does
// not show.
func (s *slackChat) status(th, id string, t *turn) {
	title := planTitle(id, t)
	current := s.b.shows(th, id)
	if t.done {
		// The agent session is ready for the next message.
		if current {
			s.setStatus(th, "active")
		}
		if t.status == "" {
			return
		}
	} else if current {
		s.mu.Lock()
		stale := time.Since(s.processing[th]) > keepProcessing
		s.mu.Unlock()
		if stale {
			s.setStatus(th, "processing")
		}
	}
	tasks := make([]*slack.TaskCardBlock, len(t.tasks))
	for i, k := range t.tasks {
		status := taskStatus(t, k)
		tasks[i] = slack.NewTaskCardBlock(k.id, k.title).WithStatus(status)
		if k.detail != "" {
			tasks[i].WithDetails(slack.NewRichTextBlock("", slack.NewRichTextSection(slack.NewRichTextSectionTextElement(k.detail, &slack.RichTextSectionTextStyle{Code: true}))))
		}
	}
	// Slack wants a new block id each time a message changes.
	plan := slack.NewPlanBlock(title, slack.PlanBlockOptionBlockID(strconv.FormatInt(time.Now().UnixNano(), 36))).WithTasks(tasks...)
	channel, ts := threadParts(th)
	opts := []slack.MsgOption{slack.MsgOptionText(title, false), slack.MsgOptionBlocks(plan)}
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

// planTitle is the title of a turn's status message: working with its calls,
// or how it ended, with the session id, which Slack's title does not show.
// The console prints it as is, so it shows what Slack shows.
func planTitle(id string, t *turn) string {
	if !t.done {
		return "Working · " + calls(t.calls)
	}
	title := fmt.Sprintf("Done · %s · %s", calls(t.calls), t.took)
	if t.outcome != "" {
		title = fmt.Sprintf("%s after %s · %s", strings.ToUpper(t.outcome[:1])+t.outcome[1:], calls(t.calls), t.took)
	} else if t.used >= 0 {
		title += fmt.Sprintf(" · %d%% context", t.used)
	}
	return title + " · " + id
}

// taskStatus is how a call shows in the status message's plan.
func taskStatus(t *turn, k task) slack.TaskCardStatus {
	status := slack.TaskCardStatus(k.status)
	if t.done && status == slack.TaskCardStatusInProgress {
		// A call left open by a failure or a stop failed with it; one left
		// open when the session filled up has no result: pending.
		status = slack.TaskCardStatusError
		if t.outcome == "continued" {
			status = slack.TaskCardStatusPending // no result: not done, not failed
		}
	}
	return status
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

// allowed reports whether the bot takes messages from a Slack user: one listed
// in MAINPLANE_SLACK_USERS, or with "*" there, any full member of the bot's
// workspace. A failed lookup denies and is retried on the next message.
func (s *slackChat) allowed(user string) bool {
	if s.users[user] {
		return true
	}
	if !s.members || user == "" {
		return false
	}
	u, err := s.profile(user)
	return err == nil && fullMember(u, s.team)
}

// profile is a Slack user's profile, cached for ten minutes.
func (s *slackChat) profile(user string) (*slack.User, error) {
	s.mu.Lock()
	p, ok := s.people[user]
	s.mu.Unlock()
	if ok && time.Since(p.at) < 10*time.Minute {
		return p.u, nil
	}
	u, err := s.sl.GetUserInfo(user)
	if err != nil {
		log.Printf("users.info %s: %v", user, err)
		return nil, err
	}
	s.mu.Lock()
	s.people[user] = person{u: u, at: time.Now()}
	s.mu.Unlock()
	return u, nil
}

// display is the name Slack shows for a user, or the id when it is unknown.
func (s *slackChat) display(user string) string {
	if u, err := s.profile(user); err == nil {
		return cmp.Or(u.Profile.DisplayName, u.RealName, u.Name, user)
	}
	return user
}

// framed is what goes before a message's text for the agent: who wrote it, so
// the agent can tell several people apart and mention them, and on a session's
// first message, that it is a Slack app and where the conversation is.
func (s *slackChat) framed(channel, user string, first bool) string {
	where := ""
	if first {
		where = s.where(channel)
	}
	return frame(s.name, where, s.display(user), user)
}

// where describes a conversation: a DM, a group DM and who is in it, or a
// channel by name.
func (s *slackChat) where(channel string) string {
	info, err := s.sl.GetConversationInfo(&slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		log.Printf("conversations.info %s: %v", channel, err)
		return "a Slack conversation"
	}
	var names []string
	if info.IsMpIM {
		ids, _, err := s.sl.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: channel, Limit: 50})
		if err != nil {
			log.Printf("conversations.members %s: %v", channel, err)
		}
		for _, id := range ids {
			if id != s.self {
				names = append(names, s.display(id))
			}
		}
	}
	return describe(info, names)
}

// describe names a conversation for the agent.
func describe(info *slack.Channel, names []string) string {
	switch {
	case info.IsIM:
		return "a direct message with you"
	case info.IsMpIM && len(names) > 0:
		return "a group DM with " + strings.Join(names, ", ")
	case info.IsMpIM:
		return "a group DM"
	case info.IsPrivate:
		return "the private channel #" + info.Name
	default:
		return "the channel #" + info.Name
	}
}

// frame is the prefix of a message: its author and Slack mention. When where is
// set (a session's first message), a note first tells the agent it is a Slack
// app, where the thread is, and how its replies show.
func frame(bot, where, author, user string) string {
	line := "[" + author + " <@" + user + ">]: "
	if where == "" {
		return line
	}
	return "(Note from the Slack bridge: you are " + bot + ", an app in Slack. This conversation is a Slack thread in " + where +
		". What you write is posted to the thread as Slack messages, so use Slack formatting. Several people may write here; " +
		"each message starts with its author's name and Slack mention, and writing a mention such as <@" + user +
		"> notifies that person.)\n\n" + line
}

const slackDeliveryNote = "For Slack sending, execute mainplane-chat slack post --text 'message' and/or --file /absolute/path via run on the bridge worker. post always creates a NEW TOP-LEVEL CHANNEL MESSAGE. To send inside this thread, use mainplane-chat slack reply instead. MAINPLANE_SESSION automatically selects this conversation; do not guess a channel or session id. The installed sender handles credentials and returns JSON with ok:true and the actual Slack message permalink. Only report delivery after that confirmed receipt. Do not merely echo the command, print attach: lines, read image bytes, or use mainplane message. Files must exist on the bridge worker; copy other workers' files there first. Normal final text still replies in this thread."

// fullMember is a person with a regular account in the given workspace: not a
// guest (single- or multi-channel), not from another organization through
// Slack Connect, not a bot or app, and not deactivated.
func fullMember(u *slack.User, team string) bool {
	return u.TeamID == team && !u.IsRestricted && !u.IsUltraRestricted && !u.IsStranger &&
		!u.IsBot && !u.IsAppUser && !u.Deleted && u.ID != "USLACKBOT"
}
