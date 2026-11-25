package bridges

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/names"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// LinkTTL is how long a link code works.
const LinkTTL = 10 * time.Minute

// maxCodesPerHour bounds how many codes one person makes.
const maxCodesPerHour = 10

// Errors of the people's side of links.
var (
	ErrInvalid   = errors.New("invalid")
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("not found")
	ErrTooMany   = errors.New("too many link codes; wait an hour")
)

// codePrefix makes a code recognisable where it is typed: "link GTR-ABCD-EFGH".
const codePrefix = "GTR-"

// LinkCode is a code a person made to link an outside account, shown once.
type LinkCode struct {
	Code      string
	Provider  string
	ExpiresAt time.Time
}

// NewLinkCode makes a code that links an account on provider, in project, to the person p. They
// type it where the bridge hears it ("/gator link GTR-…" in Slack). Only its hash is kept.
func (s *Service) NewLinkCode(ctx context.Context, p auth.Principal, project pgtype.UUID, provider string) (LinkCode, error) {
	if p.Kind != auth.KindUser || !p.UserID.Valid {
		return LinkCode{}, ErrForbidden
	}
	if !names.Valid(provider) {
		return LinkCode{}, fmt.Errorf("%w: provider is a plugin's name", ErrInvalid)
	}
	role, err := s.deps.Roles.ProjectRole(ctx, p, project)
	if err != nil {
		return LinkCode{}, err
	}
	if role < auth.RoleViewer {
		return LinkCode{}, ErrNotFound
	}
	now := s.deps.Now()
	n, err := s.q.CountRecentLinkCodes(ctx, db.CountRecentLinkCodesParams{UserID: p.UserID, Since: ts(now.Add(-time.Hour))})
	if err != nil {
		return LinkCode{}, err
	}
	if n >= maxCodesPerHour {
		return LinkCode{}, ErrTooMany
	}
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return LinkCode{}, err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw) // 16 characters, 80 bits
	code := codePrefix + enc[0:4] + "-" + enc[4:8] + "-" + enc[8:12] + "-" + enc[12:16]
	exp := now.Add(LinkTTL)
	if err := s.q.InsertLinkCode(ctx, db.InsertLinkCodeParams{Hash: codeHash(code), ProjectID: project, Provider: provider,
		UserID: p.UserID, ExpiresAt: ts(exp)}); err != nil {
		return LinkCode{}, err
	}
	return LinkCode{Code: code, Provider: provider, ExpiresAt: exp}, nil
}

// codeHash is what is kept of a code: the same code typed with other case or spacing is the same.
func codeHash(code string) []byte {
	c := strings.ToUpper(strings.Join(strings.Fields(code), ""))
	h := sha256.Sum256([]byte(c))
	return h[:]
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// Link is member.link: with a code, the outside account becomes the person who made it, in this
// project and on this bridge only; without one, who it is linked to.
func (s *Service) Link(ctx context.Context, b Bridge, p plugin.MemberLinkParams) (plugin.MemberLinkResult, error) {
	who, err := s.identity(ctx, b, plugin.ExternalAuthor{ExternalID: p.ExternalID, DisplayName: p.DisplayName})
	if err != nil {
		return plugin.MemberLinkResult{}, err
	}
	if strings.TrimSpace(p.Code) != "" {
		if !s.links.Allow(conversations.UUIDString(who.ID)) {
			return plugin.MemberLinkResult{}, forbidden("too many tries; wait a minute")
		}
		code, err := s.q.UseLinkCode(ctx, db.UseLinkCodeParams{Hash: codeHash(p.Code), ProjectID: b.Project, Provider: b.Provider})
		if errors.Is(err, pgx.ErrNoRows) {
			return plugin.MemberLinkResult{}, forbidden("that code is not one, or it expired, or it was made for another project or bridge")
		}
		if err != nil {
			return plugin.MemberLinkResult{}, err
		}
		u, err := s.q.GetUser(ctx, code.UserID)
		if err != nil {
			return plugin.MemberLinkResult{}, err
		}
		if role, err := s.deps.Roles.ProjectRole(ctx, principalOf(u), b.Project); err != nil {
			return plugin.MemberLinkResult{}, err
		} else if role < auth.RoleViewer {
			return plugin.MemberLinkResult{}, forbidden("the person who made that code is not in this project any more")
		}
		if who, err = s.q.LinkExternalIdentity(ctx, db.LinkExternalIdentityParams{ID: who.ID, UserID: u.ID}); err != nil {
			return plugin.MemberLinkResult{}, err
		}
	}
	if !who.UserID.Valid {
		return plugin.MemberLinkResult{}, nil
	}
	u, err := s.q.GetUser(ctx, who.UserID)
	if err != nil {
		return plugin.MemberLinkResult{}, err
	}
	return plugin.MemberLinkResult{UserID: conversations.UUIDString(u.ID), Name: u.Name, Linked: true}, nil
}

func principalOf(u db.User) auth.Principal {
	return auth.Principal{Kind: auth.KindUser, UserID: u.ID, WorkspaceRole: u.WorkspaceRole, Name: u.Name}
}

// Identities are the outside accounts a project's bridges know, for its people to read.
func (s *Service) Identities(ctx context.Context, p auth.Principal, project pgtype.UUID) ([]db.ExternalIdentity, error) {
	role, err := s.deps.Roles.ProjectRole(ctx, p, project)
	if err != nil {
		return nil, err
	}
	if role < auth.RoleViewer {
		return nil, ErrNotFound
	}
	return s.q.ListExternalIdentities(ctx, project)
}

// Unlink takes a link away: the person it is linked to may, and a project admin.
func (s *Service) Unlink(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.ExternalIdentity, error) {
	who, err := s.q.GetExternalIdentity(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ExternalIdentity{}, ErrNotFound
	}
	if err != nil {
		return db.ExternalIdentity{}, err
	}
	role, err := s.deps.Roles.ProjectRole(ctx, p, who.ProjectID)
	if err != nil {
		return db.ExternalIdentity{}, err
	}
	if role < auth.RoleViewer {
		return db.ExternalIdentity{}, ErrNotFound
	}
	mine := p.Kind == auth.KindUser && who.UserID.Valid && who.UserID == p.UserID
	if !mine && role < auth.RoleAdmin {
		return db.ExternalIdentity{}, ErrForbidden
	}
	if !who.UserID.Valid {
		return who, nil
	}
	return s.q.UnlinkExternalIdentity(ctx, id)
}
