package agents

import "github.com/onegator/gator/internal/server/store/db"

// Grant is what a turn may touch, decided before it starts.
type Grant struct {
	Tier string
	// NeedsConsent says the agent works, but nobody has allowed agents on the task: the turn
	// answers in chat and a person is asked whether it may work.
	NeedsConsent bool
}

// TierFor decides what a turn of the agent may touch on the task. A chat agent chats. A work
// agent works only where a person allowed agents on the task (the consent of migration 00033),
// whoever asked — a person or another agent in a chat turn; otherwise it answers in chat and the
// consent is asked for. A mention itself is consent to that one read-only answer, nothing more.
func TierFor(agent db.Agent, task db.Task) Grant {
	if agent.Tier != TierWork {
		return Grant{Tier: TierChat}
	}
	if task.AgentsAllowedAt.Valid {
		return Grant{Tier: TierWork}
	}
	return Grant{Tier: TierChat, NeedsConsent: true}
}
