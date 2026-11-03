package main

import (
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
		chType                 string
		mentioned, owned, want bool
	}{
		{"im", false, false, true},
		{"im", false, true, true},
		{"group", true, false, true},
		{"channel", true, false, true},
		{"group", false, true, true},
		{"channel", false, true, true},
		{"channel", false, false, false},
		{"group", false, false, false},
	} {
		if got := hears(c.chType, c.mentioned, c.owned); got != c.want {
			t.Errorf("hears(%q, mentioned=%v, owned=%v) = %v, want %v", c.chType, c.mentioned, c.owned, got, c.want)
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
		"\n\n[pedro <@U09>]: what can you do",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first message lacks %q:\n%s", want, first)
		}
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
