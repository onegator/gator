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
	msg := e.Message
	for _, x := range e.Errors {
		if x.Message != "" {
			msg += ": " + x.Message
		}
	}
	return fmt.Sprintf("github %d: %s", e.Status, msg)
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
