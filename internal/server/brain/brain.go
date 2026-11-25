// Package brain is one place for what a team and its agents know: about the product, about how it
// is built, about its people and about what was decided. Agents read it and propose to it; people
// approve. Only approved entries reach a prompt, every entry says where it came from, and a change
// is a new version rather than an overwrite.
//
// The rules of who reads what are one Policy, asked by the REST API, the MCP server and the
// package a job is handed alike. Searching (Searcher), packing (Packer) and proposing (Service)
// are separate pieces over one store.
package brain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Scopes: who an entry is about, and so who reads it.
const (
	ScopeWorkspace = "workspace" // everyone: code conventions, security standards, the company glossary
	ScopeProject   = "project"   // a project's members: vision, personas, decisions, roadmap, runbooks
	ScopeComponent = "component" // a project's members; handed to jobs about that component and its neighbours
	ScopeUser      = "user"      // its owner, unless it is shared: expertise, focus, preferences
	ScopeAgent     = "agent"     // a project's members: an agent's own lessons
)

// Scopes lists them in the order a package reads them.
var Scopes = []string{ScopeWorkspace, ScopeProject, ScopeComponent, ScopeUser, ScopeAgent}

// Kinds of entries.
const (
	KindVision     = "vision"
	KindPrinciple  = "principle"
	KindPersona    = "persona"
	KindGlossary   = "glossary"
	KindDecision   = "decision"
	KindLesson     = "lesson"
	KindRoadmap    = "roadmap"
	KindConvention = "convention"
	KindRunbook    = "runbook"
	KindFact       = "fact"
	KindExpertise  = "expertise"
	KindFocus      = "focus"
	KindPreference = "preference"
)

// Kinds lists every kind.
var Kinds = []string{KindVision, KindPrinciple, KindPersona, KindGlossary, KindDecision, KindLesson, KindRoadmap,
	KindConvention, KindRunbook, KindFact, KindExpertise, KindFocus, KindPreference}

// ProductKinds are what a project says about its own product: the old product context.
var ProductKinds = []string{KindVision, KindPrinciple, KindPersona, KindGlossary, KindDecision, KindLesson, KindRoadmap}

// UserKinds are what a person says, or Gator works out, about themselves.
var UserKinds = []string{KindExpertise, KindFocus, KindPreference}

// Topics are the shelves conventions sit on.
var Topics = []string{"language", "architecture", "requirements", "testing", "security"}

// Statuses of an entry.
const (
	StatusProposed   = "proposed"
	StatusApproved   = "approved"
	StatusRejected   = "rejected"
	StatusSuperseded = "superseded"
	StatusArchived   = "archived"
)

// Visibilities. VisibilityScope is the scope's own rule; a user entry is its owner's unless it
// says project or workspace.
const (
	VisibilityPrivate   = "private"
	VisibilityScope     = "scope"
	VisibilityProject   = "project"
	VisibilityWorkspace = "workspace"
)

// Confidences: said by somebody, or worked out by Gator.
const (
	ConfidenceStated   = "stated"
	ConfidenceInferred = "inferred"
)

// Who wrote an entry (created_by_kind).
const (
	ByUser   = "user"
	ByRunner = "runner" // an agent, in a job
	ByPlugin = "plugin"
	BySystem = "system"
)

// Errors the service answers with; callers map them to their own words.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
)

// InvalidError is input the Brain refuses, with a sentence a person can act on.
type InvalidError struct{ Reason string }

func (e InvalidError) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return InvalidError{Reason: fmt.Sprintf(format, args...)}
}

// IsInvalid reports whether err is input the Brain refused.
func IsInvalid(err error) bool {
	var ie InvalidError
	return errors.As(err, &ie)
}

// Source is where an entry came from: {kind, id, url, excerpt}.
type Source struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	URL     string `json:"url,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
	// Name is who the source is when it has a name of its own: a plugin, a pack.
	Name string `json:"name,omitempty"`
}

// SourceOf reads an entry's source.
func SourceOf(e db.BrainEntry) Source {
	var s Source
	_ = json.Unmarshal(e.Source, &s)
	if s.Kind == "" {
		s.Kind = SourceManual
	}
	return s
}

// Live says whether e may reach a prompt now: approved, not folded away and still true.
func Live(e db.BrainEntry) bool {
	return e.Status == StatusApproved && !e.ArchivedAt.Valid && !e.ValidUntil.Valid
}

func validScope(s string) bool { return slices.Contains(Scopes, s) }
func validKind(k string) bool  { return slices.Contains(Kinds, k) }

// UUIDString is u in its usual form, empty for none.
func UUIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ParseUUID reads s; ok is false for anything that is not a uuid.
func ParseUUID(s string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	if err := u.Scan(strings.TrimSpace(s)); err != nil || !u.Valid {
		return pgtype.UUID{}, false
	}
	return u, true
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// now is the clock entries are stamped with.
var now = time.Now
