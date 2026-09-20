package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/onegator/gator/plugin"
)

// External references the plugin keeps on tasks.
const (
	refIssue    = "github_issue"     // owner/name#12
	refIssueURL = "github_issue_url" //
	refPR       = "github_pr"        // owner/name#34
	refPRURL    = "github_pr_url"    //
	refBranch   = "github_branch"    // the job's branch
)

const configSchema = `{"type":"object","properties":{
	"repo":{"type":"string","description":"owner/name of the repository"},
	"token":{"type":"string","x-secret":true,"description":"fine-grained token for this repository: Contents, Issues and Pull requests read and write; Checks and Metadata read. Without Checks this plugin cannot read CI results, and a gate waiting on one never learns it finished"},
	"webhook_secret":{"type":"string","x-secret":true,"description":"the secret set on the repository webhook"},
	"api_url":{"type":"string","description":"API base; default https://api.github.com"},
	"base_branch":{"type":"string","description":"branch pull requests target; default: the repository's default branch"},
	"trigger_label":{"type":"string","description":"only issues with this label become tasks; empty: every opened issue"},
	"label_prefix":{"type":"string","description":"prefix of phase labels; default gator:"},
	"check_phase":{"type":"string","description":"phase whose gate gets CI checks; default implementation"}
	},"required":["repo","token","webhook_secret"]}`

var manifest = plugin.Manifest{
	Name:         "github",
	Version:      "0.1.0",
	Capabilities: []string{"tracker", "vcs", "ci"},
	UI:           []string{"tab", "chip"},
	ConfigSchema: json.RawMessage(configSchema),
	Webhook:      &plugin.WebhookSpec{DeliveryHeader: "X-GitHub-Delivery"},
}

// prInfo is what the plugin remembers about a pull request, for the gate and the UI.
type prInfo struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"` // open | merged | closed
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
}

type gh struct{ http *http.Client }

func (g *gh) handlers() plugin.Handlers {
	return plugin.Handlers{Manifest: manifest, Webhook: g.webhook, PhaseTransition: g.phaseTransition,
		GateEvaluate: g.gateEvaluate, RenderUI: g.renderUI, JobFinish: g.jobFinish}
}

func setting(core *plugin.Core, key, def string) string {
	if v := core.Setting(key); v != "" {
		return v
	}
	return def
}

func (g *gh) client(core *plugin.Core) *Client {
	return &Client{Base: strings.TrimRight(setting(core, "api_url", "https://api.github.com"), "/"),
		Token: core.Secret("token"), Repo: core.Setting("repo"), HTTP: g.http}
}

// asPluginErr turns GitHub's answers into plugin errors. A bad request is this repository's
// business and does not count toward disabling; outages, rate limits and a refused token do.
func asPluginErr(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	// A token GitHub will not accept stays broken until a person fixes it. Reporting that as a
	// bad request buried it: invalid params are not counted, so the plugin failed quietly for
	// as long as nobody read the server log. Counting it trips the breaker, and the reason
	// appears where the plugin is configured.
	if ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden {
		return plugin.Errorf(plugin.CodeInternal,
			"GitHub refused the token (%v). It needs Contents, Issues and Pull requests: read and write, and Checks: read — Checks is the one usually missed", ae)
	}
	if ae.Status < 500 && ae.Status != http.StatusTooManyRequests {
		return plugin.Errorf(plugin.CodeInvalidParams, "%v", ae)
	}
	return err
}

func header(h map[string]string, name string) string { return h[http.CanonicalHeaderKey(name)] }

// validSignature checks X-Hub-Signature-256 in constant time.
func validSignature(secret string, body []byte, got string) bool {
	if secret == "" || !strings.HasPrefix(got, "sha256=") {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal([]byte(got), []byte("sha256="+hex.EncodeToString(m.Sum(nil))))
}

func refFor(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

// refNumber returns n of "owner/name#n" when it belongs to repo.
func refNumber(ref, repo string) (int, bool) {
	r, n, ok := strings.Cut(ref, "#")
	if !ok || !strings.EqualFold(r, repo) {
		return 0, false
	}
	num, err := strconv.Atoi(n)
	return num, err == nil && num > 0
}

// checkStatus maps a check run onto a gate check.
func checkStatus(status, conclusion string) string {
	if status != "completed" {
		return "pending"
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return "pass"
	}
	return "fail"
}

func (g *gh) webhook(ctx context.Context, core *plugin.Core, p plugin.WebhookParams) error {
	if !validSignature(core.Secret("webhook_secret"), p.Body, header(p.Headers, "X-Hub-Signature-256")) {
		return plugin.Errorf(plugin.CodeForbidden, "bad or missing X-Hub-Signature-256")
	}
	var env struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(p.Body, &env); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "body: %v", err)
	}
	event := header(p.Headers, "X-GitHub-Event")
	if event == "ping" {
		return nil
	}
	repo := core.Setting("repo")
	if !strings.EqualFold(env.Repository.FullName, repo) {
		return core.Log(ctx, "warn", "ignoring a delivery for another repository", map[string]any{"repository": env.Repository.FullName})
	}
	switch event {
	case "issues":
		return g.onIssue(ctx, core, repo, env.Action, p.Body)
	case "pull_request":
		return g.onPullRequest(ctx, core, repo, env.Action, p.Body)
	case "check_run":
		return g.onCheckRun(ctx, core, repo, p.Body)
	}
	return nil
}

func (g *gh) onIssue(ctx context.Context, core *plugin.Core, repo, action string, body []byte) error {
	if action != "opened" && action != "reopened" && action != "labeled" {
		return nil
	}
	var ev struct {
		Issue struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			Labels  []struct {
				Name string `json:"name"`
			} `json:"labels"`
			PullRequest *struct{} `json:"pull_request"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "issue: %v", err)
	}
	is := ev.Issue
	if is.PullRequest != nil || is.Number == 0 {
		return nil
	}
	has := func(name string) bool {
		for _, l := range is.Labels {
			if strings.EqualFold(l.Name, name) {
				return true
			}
		}
		return false
	}
	if t := core.Setting("trigger_label"); t != "" && !has(t) {
		return nil
	}
	ref := refFor(repo, is.Number)
	// Deliveries repeat and labels fire again: one issue, one task.
	if t, err := core.FindTask(ctx, refIssue, ref); err != nil || t != nil {
		return err
	}
	kind := "feature"
	if has("bug") {
		kind = "bug"
	}
	_, err := core.CreateTask(ctx, plugin.TaskCreateParams{Kind: kind, Title: is.Title,
		Description:  strings.TrimSpace(is.Body + "\n\n" + is.HTMLURL),
		ExternalRefs: map[string]string{refIssue: ref, refIssueURL: is.HTMLURL}})
	return err
}

func prState(pr PR) string {
	switch {
	case pr.Merged:
		return "merged"
	case pr.State == "closed":
		return "closed"
	}
	return "open"
}

func (g *gh) onPullRequest(ctx context.Context, core *plugin.Core, repo, action string, body []byte) error {
	var ev struct {
		PullRequest PR `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "pull_request: %v", err)
	}
	pr := ev.PullRequest
	ref := refFor(repo, pr.Number)
	t, err := core.FindTask(ctx, refPR, ref)
	if err == nil && t == nil && pr.Head.Ref != "" {
		t, err = core.FindTask(ctx, refBranch, pr.Head.Ref)
	}
	if err != nil || t == nil {
		return err // not a pull request Gator knows
	}
	if t.ExternalRefs[refPR] != ref {
		if _, err := core.UpdateTask(ctx, plugin.TaskUpdateParams{TaskID: t.ID, ExternalRefs: map[string]string{refPR: ref, refPRURL: pr.HTMLURL}}); err != nil {
			return err
		}
	}
	state := prState(pr)
	if err := core.KVPut(ctx, "pr:"+ref, prInfo{Number: pr.Number, Title: pr.Title, State: state, URL: pr.HTMLURL, HeadSHA: pr.Head.SHA}); err != nil {
		return err
	}
	switch action {
	case "opened", "reopened", "closed":
		_, err := core.PutArtifact(ctx, plugin.ArtifactPutParams{TaskID: t.ID, Type: "pull_request", URL: pr.HTMLURL,
			Content: fmt.Sprintf("PR #%d %s: %s", pr.Number, state, pr.Title)})
		return err
	}
	return nil
}

func (g *gh) onCheckRun(ctx context.Context, core *plugin.Core, repo string, body []byte) error {
	var ev struct {
		CheckRun CheckRun `json:"check_run"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "check_run: %v", err)
	}
	cr := ev.CheckRun
	phase := setting(core, "check_phase", "implementation")
	for _, n := range cr.PullRequests {
		t, err := core.FindTask(ctx, refPR, refFor(repo, n.Number))
		if err != nil {
			return err
		}
		if t == nil || t.Phase != phase {
			continue
		}
		detail := strings.TrimSpace(cr.Conclusion + " " + cr.HTMLURL)
		if err := core.SetCheck(ctx, t.ID, plugin.Check{Name: "github/" + cr.Name, Status: checkStatus(cr.Status, cr.Conclusion), Detail: detail}); err != nil {
			return err
		}
	}
	return nil
}

func (g *gh) jobFinish(ctx context.Context, core *plugin.Core, p plugin.JobFinishParams) error {
	var r struct {
		Status  string   `json:"status"`
		Branch  string   `json:"branch"`
		Commits []string `json:"commits"`
		Summary string   `json:"summary"`
	}
	_ = json.Unmarshal(p.Receipt, &r)
	if p.Job.Role != "worker" || r.Status != "done" || r.Branch == "" || len(r.Commits) == 0 {
		return nil // only finished work with commits becomes a pull request
	}
	repo, c := core.Setting("repo"), g.client(core)
	base := core.Setting("base_branch")
	if base == "" {
		b, err := c.DefaultBranch(ctx)
		if err != nil {
			return asPluginErr(err)
		}
		base = b
	}
	issue, _ := refNumber(p.Task.ExternalRefs[refIssue], repo)
	body := prBody(p, r.Summary, len(r.Commits), issue)
	pr, err := c.CreatePR(ctx, p.Task.Title, r.Branch, base, body)
	if err != nil {
		// A retried job or a redelivered hook: the pull request already exists, so refresh it.
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
			return asPluginErr(err)
		}
		existing, ferr := c.FindOpenPR(ctx, r.Branch)
		if ferr != nil || existing == nil {
			return asPluginErr(err)
		}
		if pr, err = c.UpdatePRBody(ctx, existing.Number, body); err != nil {
			return asPluginErr(err)
		}
	}
	ref := refFor(repo, pr.Number)
	if _, err := core.UpdateTask(ctx, plugin.TaskUpdateParams{TaskID: p.Task.ID,
		ExternalRefs: map[string]string{refPR: ref, refPRURL: pr.HTMLURL, refBranch: r.Branch}}); err != nil {
		return err
	}
	if err := core.KVPut(ctx, "pr:"+ref, prInfo{Number: pr.Number, Title: pr.Title, State: prState(pr), URL: pr.HTMLURL, HeadSHA: pr.Head.SHA}); err != nil {
		return err
	}
	if err := c.AddLabels(ctx, pr.Number, setting(core, "label_prefix", "gator:")+p.Task.Phase); err != nil {
		return asPluginErr(err)
	}
	_, err = core.PutArtifact(ctx, plugin.ArtifactPutParams{TaskID: p.Task.ID, Type: "pull_request", URL: pr.HTMLURL,
		Content: fmt.Sprintf("PR #%d open: %s", pr.Number, pr.Title)})
	return err
}

// prBody is the job's report plus where it came from. GitHub caps bodies at 65536 characters.
func prBody(p plugin.JobFinishParams, summary string, commits, issue int) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		s = "_The job left no report._"
	}
	if len(s) > 60000 {
		s = s[:60000] + "\n\n_Report truncated._"
	}
	var b strings.Builder
	b.WriteString(s)
	fmt.Fprintf(&b, "\n\n---\nOpened by Gator for task **%s** (`%s`), job `%s`, %d commit(s).", p.Task.Title, p.Task.ID, p.Job.ID, commits)
	if issue > 0 {
		fmt.Fprintf(&b, "\n\nCloses #%d", issue)
	}
	return b.String()
}

// phaseTransition moves the gator:<phase> label on the task's issue and pull request.
func (g *gh) phaseTransition(ctx context.Context, core *plugin.Core, p plugin.PhaseTransitionParams) error {
	repo, prefix := core.Setting("repo"), setting(core, "label_prefix", "gator:")
	var nums []int
	for _, key := range []string{refIssue, refPR} {
		if n, ok := refNumber(p.Task.ExternalRefs[key], repo); ok {
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return nil
	}
	c := g.client(core)
	for _, n := range nums {
		if p.From != "" {
			if err := c.RemoveLabel(ctx, n, prefix+p.From); err != nil {
				return asPluginErr(err)
			}
		}
		if err := c.AddLabels(ctx, n, prefix+p.To); err != nil {
			return asPluginErr(err)
		}
	}
	return nil
}

func (g *gh) pr(ctx context.Context, core *plugin.Core, t plugin.TaskRef) (prInfo, bool) {
	var info prInfo
	ref := t.ExternalRefs[refPR]
	if ref == "" {
		return info, false
	}
	found, err := core.KVGet(ctx, "pr:"+ref, &info)
	return info, err == nil && found
}

// gateEvaluate reports the pull request's current check runs when the task enters the CI phase.
func (g *gh) gateEvaluate(ctx context.Context, core *plugin.Core, p plugin.GateEvaluateParams) (plugin.GateEvaluateResult, error) {
	out := plugin.GateEvaluateResult{Checks: []plugin.Check{}}
	if p.Phase != setting(core, "check_phase", "implementation") {
		return out, nil
	}
	info, ok := g.pr(ctx, core, p.Task)
	if !ok || info.HeadSHA == "" {
		return out, nil
	}
	runs, err := g.client(core).CheckRuns(ctx, info.HeadSHA)
	if err != nil {
		return out, asPluginErr(err)
	}
	for _, r := range runs {
		out.Checks = append(out.Checks, plugin.Check{Name: "github/" + r.Name, Status: checkStatus(r.Status, r.Conclusion),
			Detail: strings.TrimSpace(r.Conclusion + " " + r.HTMLURL)})
	}
	return out, nil
}

var prColors = map[string]string{"open": "#1a7f37", "merged": "#8250df", "closed": "#6e7781"}

func (g *gh) renderUI(ctx context.Context, core *plugin.Core, p plugin.RenderUIParams) (plugin.RenderUIResult, error) {
	var out plugin.RenderUIResult
	var lines []string
	repo := core.Setting("repo")
	if n, ok := refNumber(p.Task.ExternalRefs[refIssue], repo); ok {
		lines = append(lines, fmt.Sprintf("- Issue: [%s#%d](%s)", repo, n, p.Task.ExternalRefs[refIssueURL]))
		out.Chips = append(out.Chips, plugin.Chip{Text: fmt.Sprintf("#%d", n), URL: p.Task.ExternalRefs[refIssueURL]})
	}
	if info, ok := g.pr(ctx, core, p.Task); ok {
		lines = append(lines, fmt.Sprintf("- Pull request: [#%d %s](%s), %s", info.Number, info.Title, info.URL, info.State))
		out.Chips = append(out.Chips, plugin.Chip{Text: fmt.Sprintf("PR #%d", info.Number), Color: prColors[info.State], URL: info.URL})
	}
	if b := p.Task.ExternalRefs[refBranch]; b != "" {
		lines = append(lines, "- Branch: `"+b+"`")
	}
	if len(lines) > 0 {
		out.Tabs = []plugin.Tab{{Title: "GitHub", Markdown: strings.Join(lines, "\n")}}
	}
	return out, nil
}
