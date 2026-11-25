package brain

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// What an entry links to is checked before it is stored, and shown only as far as the reader may
// read it. A link is not a way into another project, nor into somebody's private context: every
// target belongs to the entry's own project (or is an entry its writer may read), and nobody but
// the Brain itself says that a proposal contradicts a decision — approving one supersedes what it
// contradicts.

// Relations of a link.
const (
	RelationRelates     = "relates"
	RelationDependsOn   = "depends_on"
	RelationContradicts = "contradicts"
	RelationImplements  = "implements"
)

// writerRelations are what a writer may say an entry is to its target.
var writerRelations = []string{RelationRelates, RelationDependsOn, RelationImplements}

// targetStore is what the target rules read. *db.Queries is one.
type targetStore interface {
	GetBrainEntry(ctx context.Context, id pgtype.UUID) (db.BrainEntry, error)
	GetTask(ctx context.Context, id pgtype.UUID) (db.Task, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetComponentByKey(ctx context.Context, arg db.GetComponentByKeyParams) (db.Component, error)
}

// targetRule says whether entry e may link to target id, written by r (unset for a part of Gator
// that has already decided it may write).
type targetRule func(ctx context.Context, st targetStore, p Policy, r *Reader, e db.BrainEntry, id string) (bool, error)

// Targets are the kinds of things an entry links to, each with its rule. A new kind of target is
// one more rule.
type Targets map[string]targetRule

// DefaultTargets are entries, tasks, conversations and components.
func DefaultTargets() Targets {
	return Targets{
		"entry": func(ctx context.Context, st targetStore, p Policy, r *Reader, e db.BrainEntry, id string) (bool, error) {
			t, ok, err := lookup(ctx, id, st.GetBrainEntry)
			if err != nil || !ok || (t.ProjectID.Valid && t.ProjectID != e.ProjectID) {
				return false, err
			}
			if r == nil {
				return true, nil
			}
			return p.CanRead(ctx, *r, t)
		},
		"task": func(ctx context.Context, st targetStore, _ Policy, _ *Reader, e db.BrainEntry, id string) (bool, error) {
			t, ok, err := lookup(ctx, id, st.GetTask)
			return ok && e.ProjectID.Valid && t.ProjectID == e.ProjectID, err
		},
		"conversation": func(ctx context.Context, st targetStore, _ Policy, _ *Reader, e db.BrainEntry, id string) (bool, error) {
			c, ok, err := lookup(ctx, id, st.GetConversation)
			return ok && e.ProjectID.Valid && c.ProjectID == e.ProjectID, err
		},
		"component": func(ctx context.Context, st targetStore, _ Policy, _ *Reader, e db.BrainEntry, key string) (bool, error) {
			if !e.ProjectID.Valid {
				return false, nil
			}
			_, err := st.GetComponentByKey(ctx, db.GetComponentByKeyParams{ProjectID: e.ProjectID, Key: key})
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		},
	}
}

// check refuses the links of e a writer may not make.
func (ts Targets) check(ctx context.Context, st targetStore, p Policy, r *Reader, e db.BrainEntry, links []Link) error {
	for _, l := range links {
		if !slices.Contains(writerRelations, l.Relation) {
			return invalid("a link relates, depends_on or implements; the Brain says itself what contradicts what")
		}
		rule, known := ts[l.TargetKind]
		if !known {
			return invalid("a link points at an entry, a task, a conversation or a component")
		}
		ok, err := rule(ctx, st, p, r, e, strings.TrimSpace(l.TargetID))
		if err != nil {
			return err
		}
		if !ok {
			return invalid("%s %s is not this entry's to link to", l.TargetKind, l.TargetID)
		}
	}
	return nil
}

// visibleLinks keeps the links whose targets r may read.
func (ts Targets) visibleLinks(ctx context.Context, st targetStore, p Policy, r Reader, e db.BrainEntry, links []db.BrainLink) ([]db.BrainLink, error) {
	out := make([]db.BrainLink, 0, len(links))
	for _, l := range links {
		ok := false
		var err error
		switch l.TargetKind {
		case "entry":
			ok, err = ts["entry"](ctx, st, p, &r, e, l.TargetID)
		case "conversation":
			ok, err = p.readsConversationID(ctx, r, l.TargetID)
		default:
			ok = r.ReadsProject(e.ProjectID) // a task or a component of the entry's own project
		}
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// lookup reads one row by a uuid given as text; ok is false for nothing there.
func lookup[T any](ctx context.Context, s string, get func(context.Context, pgtype.UUID) (T, error)) (T, bool, error) {
	var zero T
	id, ok := ParseUUID(s)
	if !ok {
		return zero, false, nil
	}
	v, err := get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, false, nil
	}
	return v, err == nil, err
}
