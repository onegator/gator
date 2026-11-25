package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxSkew is how old a request may be: Slack's own advice, and what keeps a captured request from
// being played again later. Within it, the core handles one delivery once.
const maxSkew = 5 * time.Minute

// verify checks Slack's signature of a request: v0=HMAC-SHA256(secret, "v0:<ts>:<body>"), with a
// timestamp no further than maxSkew from now.
func verify(secret, ts, signature string, body []byte, now time.Time) bool {
	if secret == "" || ts == "" || !strings.HasPrefix(signature, "v0=") {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > maxSkew || d < -maxSkew {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + ts + ":"))
	m.Write(body)
	want := "v0=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// escape keeps Gator's words words in Slack: &, < and > are its markup, so "<!channel>" written by
// an agent (or by somebody it quotes) shows as text and pings nobody.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

var (
	userMarkup = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)
	linkRef    = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)(?:\|([^>]*))?>`)
	specialRef = regexp.MustCompile(`<![^>]*>|<#[A-Z0-9]+(?:\|([^>]*))?>`)
	handleWord = regexp.MustCompile(`(?:^|\s)@([a-z0-9][a-z0-9-]{1,31})\b`)
)

// plain is what a Slack message says, as Gator keeps it: mentions of the bot taken out, other
// people's as @<id>, links as their text and address, markup unescaped.
func plain(text, botUser string) string {
	text = userMarkup.ReplaceAllStringFunc(text, func(m string) string {
		id := userMarkup.FindStringSubmatch(m)[1]
		if id == botUser {
			return ""
		}
		return "@" + id
	})
	text = linkRef.ReplaceAllStringFunc(text, func(m string) string {
		g := linkRef.FindStringSubmatch(m)
		if g[2] != "" && g[2] != g[1] {
			return g[2] + " (" + g[1] + ")"
		}
		return g[1]
	})
	text = specialRef.ReplaceAllStringFunc(text, func(m string) string {
		if g := specialRef.FindStringSubmatch(m); g[1] != "" {
			return "#" + g[1]
		}
		return ""
	})
	text = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(text)
	return strings.TrimSpace(text)
}

// handles are the agents a message asks: "@Gator reviewer look at #12" asks the word after the
// bot's mention, and any "@reviewer" written out asks that one. The core keeps only real agents.
func handles(text, botUser string) []string {
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.Trim(h, ".,:;!?"))
		for _, x := range out {
			if x == h {
				return
			}
		}
		if h != "" {
			out = append(out, h)
		}
	}
	if botUser != "" {
		mention := "<@" + botUser + ">"
		for rest := text; ; {
			i := strings.Index(rest, mention)
			if i < 0 {
				break
			}
			rest = rest[i+len(mention):]
			if f := strings.Fields(rest); len(f) > 0 {
				add(strings.TrimPrefix(f[0], "@"))
			}
		}
	}
	for _, m := range handleWord.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	return out
}

// clip keeps at most n characters, saying it cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
