package main

import "testing"

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
