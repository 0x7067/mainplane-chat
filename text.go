package main

import (
	"strings"
	"unicode/utf8"
)

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
