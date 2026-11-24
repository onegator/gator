package notify

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

func id(n byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{n}, Valid: true} }

func TestChatRecipients(t *testing.T) {
	author, ann, bob, cid := id(1), id(2), id(3), id(4)
	for _, tc := range []struct {
		name  string
		facts chatFacts
		want  map[pgtype.UUID]chatReason
	}{
		{"a channel message tells nobody who only wants mentions",
			chatFacts{author: author, members: map[[16]byte]string{ann.Bytes: "mentions", bob.Bytes: "mentions"}},
			map[pgtype.UUID]chatReason{}},
		{"a mention tells the mentioned, member or not",
			chatFacts{author: author, mentioned: []pgtype.UUID{ann, cid}, members: map[[16]byte]string{ann.Bytes: "mentions"}},
			map[pgtype.UUID]chatReason{ann: reasonMention, cid: reasonMention}},
		{"a DM tells its other people",
			chatFacts{author: author, dm: true, members: map[[16]byte]string{author.Bytes: "all", ann.Bytes: "all"}},
			map[pgtype.UUID]chatReason{ann: reasonDM}},
		{"a reply tells who wrote in the thread",
			chatFacts{author: author, thread: []pgtype.UUID{author, bob}, members: map[[16]byte]string{bob.Bytes: "mentions"}},
			map[pgtype.UUID]chatReason{bob: reasonThread}},
		{"all hears everything; a mention says so first",
			chatFacts{author: author, mentioned: []pgtype.UUID{ann}, members: map[[16]byte]string{ann.Bytes: "all", bob.Bytes: "all"}},
			map[pgtype.UUID]chatReason{ann: reasonMention, bob: reasonAll}},
		{"none silences a conversation, mentions too",
			chatFacts{author: author, dm: true, mentioned: []pgtype.UUID{ann}, members: map[[16]byte]string{ann.Bytes: "none"}},
			map[pgtype.UUID]chatReason{}},
		{"nobody hears about their own message",
			chatFacts{author: author, mentioned: []pgtype.UUID{author}, thread: []pgtype.UUID{author}, members: map[[16]byte]string{author.Bytes: "all"}},
			map[pgtype.UUID]chatReason{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chatRecipients(tc.facts)
			if len(got) != len(tc.want) {
				t.Fatalf("recipients %v, want %v", got, tc.want)
			}
			for u, why := range tc.want {
				if got[u.Bytes] != why {
					t.Fatalf("recipients %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestChatMessageWords(t *testing.T) {
	conv := db.Conversation{ID: id(9)}
	msg := db.Message{Seq: 4, Body: "Can you   look at\nthe login?"}
	m := chatMessage(reasonMention, "Ann", "#general", conv, msg)
	if m.Title != "Ann mentioned you in #general" || m.Body != "Can you look at the login?" || m.Link != "gator://conversations/"+uuidString(conv.ID)+"?seq=4" {
		t.Fatalf("mention: %+v", m)
	}
	if m := chatMessage(reasonDM, "Ann", "a direct message", conv, msg); m.Title != "Ann" {
		t.Fatalf("DM: %+v", m)
	}
	if m := chatMessage(reasonThread, "Ann", "a thread in #ops", conv, msg); m.Title != "Ann replied in a thread in #ops" {
		t.Fatalf("thread: %+v", m)
	}
}
