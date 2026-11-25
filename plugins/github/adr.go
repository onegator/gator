package main

import (
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/onegator/gator/plugin"
)

// Architecture decision records in the repository become proposals in the project's Brain: an
// ADR that lands on the default branch (docs/adr/*.md) is proposed as a decision, a person
// approves it, and every later job reads it. Each version of a file is proposed once.

// adrPattern is where the repository keeps its decisions.
const adrPattern = "docs/adr/*.md"

// maxADR bounds what one decision carries into the Brain.
const maxADR = 20000

func (g *gh) onPush(ctx context.Context, core *plugin.Core, repo string, body []byte) error {
	var ev struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		Commits []struct {
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "push: %v", err)
	}
	branch := core.Setting("base_branch")
	if branch == "" {
		branch = ev.Repository.DefaultBranch
	}
	if ev.Ref != "refs/heads/"+branch || ev.After == "" {
		return nil // what lands on another branch is not yet what the team decided
	}
	for _, p := range adrPaths(ev.Commits) {
		if err := g.proposeADR(ctx, core, repo, p, ev.After); err != nil {
			return err
		}
	}
	return nil
}

// adrPaths are the decision records a push added or changed, each once, in order.
func adrPaths(commits []struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
}) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range commits {
		for _, p := range append(append([]string{}, c.Added...), c.Modified...) {
			if ok, _ := path.Match(adrPattern, p); ok && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func (g *gh) proposeADR(ctx context.Context, core *plugin.Core, repo, file, ref string) error {
	f, err := g.client(core).File(ctx, file, ref)
	if err != nil {
		return err
	}
	title, content := adrTitle(f.Content, file), strings.TrimSpace(f.Content)
	if content == "" {
		return nil
	}
	if len(content) > maxADR {
		content = strings.ToValidUTF8(content[:maxADR], "") + "\n\n…"
	}
	_, err = core.ProposeToBrain(ctx, plugin.BrainProposeParams{Kind: "decision", Title: title, Content: content,
		SourceID: "adr:" + repo + ":" + file + "@" + f.SHA, URL: f.HTMLURL})
	return err
}

// adrTitle is a record's first heading, or its file name.
func adrTitle(content, file string) string {
	for l := range strings.SplitSeq(content, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "# "); ok && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	return strings.TrimSuffix(path.Base(file), ".md")
}
