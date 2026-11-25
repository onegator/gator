package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/store/db"
)

// mine answers everything about a person. brain.Service is one.
type mine interface {
	Mine(ctx context.Context, r brain.Reader) ([]db.BrainEntry, error)
}

// Resources for a person's client to pin: a project's principles, and their own context.
func Resources(find finder, own mine, named projects) []Resource {
	return []Resource{principles{find: find, named: named}, me{own: own}}
}

// principles is brain://project/<slug>/principles: what a project holds itself to, approved.
type principles struct {
	find  finder
	named projects
}

func (principles) Allowed(c Caller) bool { return !c.IsJob() }

func (p principles) Add(s *sdk.Server, c Caller) {
	s.AddResourceTemplate(&sdk.ResourceTemplate{Name: "principles", URITemplate: "brain://project/{slug}/principles",
		Description: "A project's vision and principles, as approved", MIMEType: "text/markdown"},
		func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			slug := strings.TrimSuffix(strings.TrimPrefix(req.Params.URI, "brain://project/"), "/principles")
			project, err := projectNamed(ctx, p.named, slug)
			if err != nil {
				return nil, sdk.ResourceNotFoundError(req.Params.URI)
			}
			hits, err := p.find.Find(ctx, c.Reader, brain.Query{Scopes: []string{brain.ScopeProject}, Project: project,
				Kinds: []string{brain.KindVision, brain.KindPrinciple}, Statuses: []string{brain.StatusApproved}, Limit: brain.MaxLimit})
			if err != nil {
				return nil, err
			}
			return markdown(req.Params.URI, "Principles of "+slug, hits), nil
		})
}

// me is brain://me: the person's own context, as agents acting for them are lent it.
type me struct{ own mine }

func (me) Allowed(c Caller) bool { return !c.IsJob() }

func (m me) Add(s *sdk.Server, c Caller) {
	s.AddResource(&sdk.Resource{Name: "me", URI: "brain://me", Description: "Your expertise, focus and preferences",
		MIMEType: "text/markdown"},
		func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			rows, err := m.own.Mine(ctx, c.Reader)
			if err != nil {
				return nil, err
			}
			var own []brain.Hit
			for _, e := range rows {
				if brain.Live(e) {
					own = append(own, brain.Hit{Entry: e})
				}
			}
			return markdown(req.Params.URI, "What Gator knows about you", own), nil
		})
}

func markdown(uri, heading string, hits []brain.Hit) *sdk.ReadResourceResult {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", heading)
	for _, h := range hits {
		fmt.Fprintf(&b, "\n## %s: %s %s\n\n%s\n", h.Entry.Kind, h.Entry.Title, brain.Tag(h.Entry.ID), strings.TrimSpace(h.Entry.Content))
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: b.String()}}}
}
