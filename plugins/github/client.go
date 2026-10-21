package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client is the slice of the GitHub REST API the plugin uses, for one repository.
type Client struct {
	Base  string // https://api.github.com, or a GitHub Enterprise or test server
	Token string
	Repo  string // owner/name
	HTTP  *http.Client
}

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Status  int
	Message string `json:"message"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (e *APIError) Error() string {
	var msg strings.Builder
	msg.WriteString(e.Message)
	for _, x := range e.Errors {
		if x.Message != "" {
			msg.WriteString(": " + x.Message)
		}
	}
	return fmt.Sprintf("github %d: %s", e.Status, msg.String())
}

// PR is a pull request.
type PR struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Merged  bool   `json:"merged"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// CheckRun is one CI check on a commit.
type CheckRun struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HTMLURL      string `json:"html_url"`
	HeadSHA      string `json:"head_sha"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if c.Token == "" {
		return errors.New("github: no token configured")
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gator-plugin-github")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		e := &APIError{Status: res.StatusCode}
		_ = json.Unmarshal(data, e)
		if e.Message == "" {
			e.Message = http.StatusText(res.StatusCode)
		}
		return e
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) repo() string { return "/repos/" + c.Repo }

// DefaultBranch is the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(ctx, http.MethodGet, c.repo(), nil, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// CreatePR opens a pull request from a branch of this repository.
func (c *Client) CreatePR(ctx context.Context, title, head, base, body string) (PR, error) {
	var pr PR
	err := c.do(ctx, http.MethodPost, c.repo()+"/pulls", map[string]string{"title": title, "head": head, "base": base, "body": body}, &pr)
	return pr, err
}

// FindOpenPR returns the open pull request from branch, or nil.
func (c *Client) FindOpenPR(ctx context.Context, branch string) (*PR, error) {
	owner, _, _ := strings.Cut(c.Repo, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}}
	var prs []PR
	if err := c.do(ctx, http.MethodGet, c.repo()+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// UpdatePRBody replaces a pull request's description.
func (c *Client) UpdatePRBody(ctx context.Context, number int, body string) (PR, error) {
	var pr PR
	err := c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", c.repo(), number), map[string]string{"body": body}, &pr)
	return pr, err
}

// AddLabels adds labels to an issue or pull request; GitHub creates missing labels.
func (c *Client) AddLabels(ctx context.Context, number int, labels ...string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/labels", c.repo(), number), map[string][]string{"labels": labels}, nil)
}

// RemoveLabel removes a label; a label that is not there is fine.
func (c *Client) RemoveLabel(ctx context.Context, number int, label string) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/%d/labels/%s", c.repo(), number, url.PathEscape(label)), nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// CheckRuns lists the check runs of a commit.
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var r struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", c.repo(), url.PathEscape(sha)), nil, &r)
	return r.CheckRuns, err
}

// Issue is an issue as the plugin reads it. Pull requests come back from the issues API too,
// with PullRequest set.
type Issue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	HTMLURL     string `json:"html_url"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request"`
}

// OpenIssues lists one page of open issues, newest first, with label if it is set.
func (c *Client) OpenIssues(ctx context.Context, label string, page int) ([]Issue, error) {
	q := url.Values{"state": {"open"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}, "sort": {"created"}, "direction": {"desc"}}
	if label != "" {
		q.Set("labels", label)
	}
	var out []Issue
	err := c.do(ctx, http.MethodGet, c.repo()+"/issues?"+q.Encode(), nil, &out)
	return out, err
}

// GetIssue reads one issue.
func (c *Client) GetIssue(ctx context.Context, number int) (Issue, error) {
	var out Issue
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", c.repo(), number), nil, &out)
	return out, err
}

// CloseIssue closes an issue as completed.
func (c *Client) CloseIssue(ctx context.Context, number int) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", c.repo(), number),
		map[string]string{"state": "closed", "state_reason": "completed"}, nil)
}

// Hook is a repository webhook.
type Hook struct {
	ID     int64    `json:"id"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// Hooks lists the repository's webhooks.
func (c *Client) Hooks(ctx context.Context) ([]Hook, error) {
	var out []Hook
	err := c.do(ctx, http.MethodGet, c.repo()+"/hooks?per_page=100", nil, &out)
	return out, err
}

func hookBody(hookURL, secret string, events []string) map[string]any {
	return map[string]any{"active": true, "events": events,
		"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret, "insecure_ssl": "0"}}
}

// CreateHook adds a webhook.
func (c *Client) CreateHook(ctx context.Context, hookURL, secret string, events []string) error {
	b := hookBody(hookURL, secret, events)
	b["name"] = "web"
	return c.do(ctx, http.MethodPost, c.repo()+"/hooks", b, nil)
}

// UpdateHook sets a webhook's secret and events; GitHub never shows the secret, so the only
// way to be sure it matches is to write it.
func (c *Client) UpdateHook(ctx context.Context, id int64, hookURL, secret string, events []string) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/hooks/%d", c.repo(), id), hookBody(hookURL, secret, events), nil)
}
