package conversations

import (
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/store/db"
)

func TestFirstLineMakesATitle(t *testing.T) {
	for in, want := range map[string]string{
		"## Login loops on Safari\nIt redirects forever.": "Login loops on Safari",
		"\n\n> - **quoted** item":                         "**quoted** item",
		"   ":                                             "",
		strings.Repeat("a", 100):                          strings.Repeat("a", 80) + "…",
	} {
		if got := firstLine(in, maxTitleRunes); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPromotionDescriptionQuotesAndLinksBack(t *testing.T) {
	msg := db.Message{ConversationID: newID(3), Seq: 7, Body: "Safari loops on login.\nSeen twice today."}
	replies := []db.Message{
		{Kind: messageText, Body: "Me too,\non 17.2"},
		{Kind: messageText, Body: "gone", DeletedAt: ts(time.Now())},
		{Kind: messageCard, Body: "**card**"},
	}
	got := promotionDescription(msg, replies, MessageLink(msg))
	for _, want := range []string{
		"> Safari loops on login.\n> Seen twice today.",
		"- Me too, on 17.2",
		"From the conversation: gator://conversations/" + uuidString(msg.ConversationID) + "?seq=7",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("description lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "gone") || strings.Contains(got, "card") {
		t.Fatalf("a deleted reply or a card was quoted:\n%s", got)
	}
}
