package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestMention(t *testing.T) {
	re := mentionOf("U0BOT")
	for in, want := range map[string]string{
		"<@U0BOT> what is up":        " what is up",
		"hi <@U0BOT|symphony> there": "hi  there",
		"no mention <@U0OTHER>":      "no mention <@U0OTHER>",
		"<@U0BOT2> is not the bot":   "<@U0BOT2> is not the bot",
	} {
		if got := re.ReplaceAllString(in, ""); got != want {
			t.Errorf("strip %q = %q, want %q", in, got, want)
		}
	}
	if !re.MatchString("ping <@U0BOT>") || re.MatchString("ping <@U0BOT2>") || re.MatchString("ping U0BOT") {
		t.Error("mentionOf matched the wrong text")
	}
}

func TestChatType(t *testing.T) {
	for ch, want := range map[string]string{"D01": "im", "G01": "group", "C01": "channel"} {
		if got := chatType(ch); got != want {
			t.Errorf("chatType(%q) = %q, want %q", ch, got, want)
		}
	}
}

func TestHears(t *testing.T) {
	for _, c := range []struct {
		chType          string
		mentioned, want bool
	}{
		{"im", false, true},
		{"im", true, true},
		{"mpim", true, true},
		{"group", true, true},
		{"channel", true, true},
		{"mpim", false, false}, // a reply in the bot's own thread without a mention, too
		{"group", false, false},
		{"channel", false, false},
	} {
		if got := hears(c.chType, c.mentioned); got != c.want {
			t.Errorf("hears(%q, mentioned=%v) = %v, want %v", c.chType, c.mentioned, got, c.want)
		}
	}
}

func TestFullMember(t *testing.T) {
	base := slack.User{ID: "U01", TeamID: "T01"}
	cases := map[string]func(u *slack.User){
		"member":          func(u *slack.User) {},
		"other workspace": func(u *slack.User) { u.TeamID = "T02" },
		"multi guest":     func(u *slack.User) { u.IsRestricted = true },
		"single guest":    func(u *slack.User) { u.IsUltraRestricted = true },
		"slack connect":   func(u *slack.User) { u.IsStranger = true },
		"bot":             func(u *slack.User) { u.IsBot = true },
		"app user":        func(u *slack.User) { u.IsAppUser = true },
		"deactivated":     func(u *slack.User) { u.Deleted = true },
		"slackbot":        func(u *slack.User) { u.ID = "USLACKBOT" },
	}
	for name, change := range cases {
		u := base
		change(&u)
		if got, want := fullMember(&u, "T01"), name == "member"; got != want {
			t.Errorf("%s: fullMember = %v, want %v", name, got, want)
		}
	}
}

func TestAllowedListedUsersWithoutLookup(t *testing.T) {
	// Listed users pass, and without "*" nobody else does, with no Slack call (s.sl is nil).
	s := &slackChat{users: map[string]bool{"U01": true}, people: map[string]person{}}
	if !s.allowed("U01") || s.allowed("U02") || s.allowed("") {
		t.Error("allowed: listed user must pass, others must not")
	}
}

func TestFrame(t *testing.T) {
	if got, want := frame("symphony", "", "gabrielg", "U07")+"hi", "[gabrielg <@U07>]: hi"; got != want {
		t.Errorf("later message = %q, want %q", got, want)
	}
	first := frame("symphony", "a group DM with pedro, gabrielg", "pedro", "U09") + "what can you do"
	for _, want := range []string{
		"you are symphony, an app in Slack",
		"a Slack thread in a group DM with pedro, gabrielg",
		"Slack formatting",
		"attach: /absolute/path in your assistant reply",
		"attach-channel: /absolute/path",
		"not in shell output",
		"You do not need Slack tokens",
		"\n\n[pedro <@U09>]: what can you do",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first message lacks %q:\n%s", want, first)
		}
	}
}

func TestSlackFileDestinations(t *testing.T) {
	for _, topLevel := range []bool{false, true} {
		t.Run(fmt.Sprint(topLevel), func(t *testing.T) {
			var completed bool
			var base string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/files.getUploadURLExternal":
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": "F1", "upload_url": base + "/upload"})
				case "/upload":
					io.Copy(io.Discard, r.Body)
					fmt.Fprint(w, "ok")
				case "/files.completeUploadExternal":
					r.ParseForm()
					if r.Form.Get("channel_id") != "C1" {
						t.Errorf("channel=%q", r.Form.Get("channel_id"))
					}
					want := "1.0"
					if topLevel {
						want = ""
					}
					if r.Form.Get("thread_ts") != want {
						t.Errorf("thread_ts=%q want %q", r.Form.Get("thread_ts"), want)
					}
					completed = true
					fmt.Fprint(w, `{"ok":true,"files":[{"id":"F1"}]}`)
				default:
					t.Errorf("unexpected Slack call %s", r.URL.Path)
					fmt.Fprint(w, `{"ok":false,"error":"unexpected"}`)
				}
			}))
			defer srv.Close()
			base = srv.URL
			s := &slackChat{sl: slack.New("test", slack.OptionAPIURL(base+"/"))}
			s.say("slack:C1/1.0", msg{Files: []file{{name: "test.txt", topLevel: topLevel, fetch: func() ([]byte, error) { return []byte("proof"), nil }}}})
			if !completed {
				t.Fatal("file never shared")
			}
		})
	}
}

func TestSlackStaleControl(t *testing.T) {
	b, _ := testCore(t)
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("stale action called %s", r.URL.Path)
		}
		r.ParseForm()
		posted = r.Form.Get("text")
		fmt.Fprint(w, `{"ok":true,"ts":"3.0"}`)
	}))
	defer srv.Close()
	s := &slackChat{b: b, sl: slack.New("test", slack.OptionAPIURL(srv.URL+"/")), users: map[string]bool{"U1": true}}
	var cb slack.InteractionCallback
	if err := json.Unmarshal([]byte(`{"user":{"id":"U1"},"container":{"channel_id":"C1","thread_ts":"1.0","message_ts":"2.0"},"actions":[{"action_id":"retry","block_id":"eeeeeeee"}]}`), &cb); err != nil {
		t.Fatal(err)
	}
	s.act(cb)
	if !strings.Contains(posted, "previous session") {
		t.Errorf("missing stale control explanation: %q", posted)
	}
}

func TestDescribe(t *testing.T) {
	ch := func(set func(c *slack.Channel)) *slack.Channel {
		c := &slack.Channel{}
		c.Name = "general"
		set(c)
		return c
	}
	for want, c := range map[string]*slack.Channel{
		"a direct message with you":    ch(func(c *slack.Channel) { c.IsIM = true }),
		"a group DM":                   ch(func(c *slack.Channel) { c.IsMpIM = true }),
		"the private channel #general": ch(func(c *slack.Channel) { c.IsPrivate = true }),
		"the channel #general":         ch(func(c *slack.Channel) {}),
	} {
		if got := describe(c, nil); got != want {
			t.Errorf("describe = %q, want %q", got, want)
		}
	}
	if got := describe(ch(func(c *slack.Channel) { c.IsMpIM = true }), []string{"a", "b"}); got != "a group DM with a, b" {
		t.Errorf("describe group DM with names = %q", got)
	}
}

func TestScopeNeedsAreTheManifests(t *testing.T) {
	m, err := os.ReadFile("slack-manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bot := regexp.MustCompile(`(?s)\n    bot:\n(.*?)\nsettings:`).FindSubmatch(m)[1]
	var want, got []string
	for _, l := range regexp.MustCompile(`(?m)^\s+- (\S+)$`).FindAllSubmatch(bot, -1) {
		want = append(want, string(l[1]))
	}
	for _, n := range scopeNeeds {
		got = append(got, n.scope)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("scopeNeeds %v, manifest %v", got, want)
	}
}

func TestLacking(t *testing.T) {
	all := make([]string, len(scopeNeeds))
	for i, n := range scopeNeeds {
		all[i] = n.scope
	}
	if l := lacking(strings.Join(all, ",")); l != nil {
		t.Fatalf("all scopes: %v", l)
	}
	l := lacking("assistant:write,chat:write,reactions:write,im:history,files:read,files:write")
	if len(l) != 8 || !strings.HasPrefix(l[0], "the app lacks channels:history: ") {
		t.Fatalf("DM-only: %v", l)
	}
}
