package cli

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
	"time"
)

// Client is a small hand-written client for the part of /api/v1 a person uses from a terminal.
//
// Not generated from docs/openapi.yaml like the server is: oapi-codegen's client for the whole
// contract is tens of thousands of lines of which this tool needs a dozen calls, and its
// pointer-heavy types make table output clumsy. The contract still binds it —
// client_test.go validates every request it sends against docs/openapi.yaml.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// APIError is an answer outside 2xx, with the server's own message.
type APIError struct {
	Status  int
	Message string
	Code    string
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return e.Message + " — sign in again with gator login"
	}
	return e.Message
}

func (e *APIError) exitCode() int {
	switch {
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return exitAuth
	case e.Status == http.StatusConflict:
		return exitConflict
	case e.Status/100 == 4:
		return exitUserErr
	}
	return exitOther
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return coded(exitUserErr, "%v", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return coded(exitNetwork, "cannot reach Gator at %s: %v", c.Base, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if res.StatusCode/100 != 2 {
		e := &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(raw))}
		var problem struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(raw, &problem) == nil && problem.Error != "" {
			e.Message, e.Code = problem.Error, problem.Code
		}
		if e.Message == "" {
			e.Message = fmt.Sprintf("Gator answered %d %s", res.StatusCode, http.StatusText(res.StatusCode))
		}
		return e
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected answer from %s %s: %v", method, path, err)
	}
	return nil
}

// --- the contract's shapes, as much of them as the CLI shows ---

type Me struct {
	Kind          string `json:"kind"`
	UserID        string `json:"userId,omitempty"`
	Name          string `json:"name"`
	WorkspaceRole string `json:"workspaceRole"`
	Scope         string `json:"scope,omitempty"`
}

type Project struct {
	ID         string     `json:"id"`
	Slug       string     `json:"slug"`
	Name       string     `json:"name"`
	Tags       []string   `json:"tags"`
	CreatedAt  time.Time  `json:"createdAt"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	Role       string     `json:"role,omitempty"`
}

type NewProject struct {
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	RepoURL       *string `json:"repoUrl,omitempty"`
	DefaultBranch *string `json:"defaultBranch,omitempty"`
}

type Task struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"projectId"`
	Kind                string     `json:"kind"`
	Title               string     `json:"title"`
	Description         string     `json:"description,omitempty"`
	Origin              string     `json:"origin,omitempty"`
	OriginSource        string     `json:"originSource,omitempty"`
	AdmittedAt          *time.Time `json:"admittedAt,omitempty"`
	Phase               string     `json:"phase"`
	Urgency             int        `json:"urgency"`
	OwnerKind           string     `json:"ownerKind,omitempty"`
	OwnerID             string     `json:"ownerId,omitempty"`
	RequirementsChanged bool       `json:"requirementsChanged"`
	BlockedReason       string     `json:"blockedReason,omitempty"`
	PhaseEnteredAt      time.Time  `json:"phaseEnteredAt"`
	CreatedAt           time.Time  `json:"createdAt"`
	ClosedAt            *time.Time `json:"closedAt,omitempty"`
}

type TaskDetail struct {
	Task
	Gate   Gate     `json:"gate"`
	Phases []string `json:"phases"`
}

type Gate struct {
	Phase         string  `json:"phase"`
	Kind          string  `json:"kind"`
	Checks        []Check `json:"checks"`
	HumanApproved bool    `json:"humanApproved"`
	BlockedReason string  `json:"blockedReason,omitempty"`
	BlockedBy     string  `json:"blockedBy,omitempty"`
}

type Check struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type NewTask struct {
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Urgency     int    `json:"urgency,omitempty"`
}

// NewJob carries placement only when the person asked for the cloud, so a server that has not
// learned the field yet still runs local jobs from this CLI.
type NewJob struct {
	Backend     string `json:"backend,omitempty"`
	Instruction string `json:"instruction,omitempty"`
	Placement   string `json:"placement,omitempty"`
}

type Job struct {
	ID          string         `json:"id"`
	TaskID      string         `json:"taskId"`
	ProjectID   string         `json:"projectId"`
	Phase       string         `json:"phase"`
	Role        string         `json:"role"`
	Backend     string         `json:"backend"`
	Model       string         `json:"model,omitempty"`
	Instruction string         `json:"instruction"`
	Status      string         `json:"status"`
	Placement   string         `json:"placement,omitempty"`
	RunnerID    string         `json:"runnerId,omitempty"`
	Attempts    int            `json:"attempts"`
	MaxAttempts int            `json:"maxAttempts"`
	StopReason  string         `json:"stopReason,omitempty"`
	Receipt     map[string]any `json:"receipt,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	StartedAt   *time.Time     `json:"startedAt,omitempty"`
	FinishedAt  *time.Time     `json:"finishedAt,omitempty"`
}

// Finished reports whether the job will produce no more output.
func (j Job) Finished() bool {
	switch j.Status {
	case "done", "failed", "stopped":
		return true
	}
	return false
}

type JobEvent struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	At      time.Time      `json:"at"`
	Payload map[string]any `json:"payload"`
}

// --- calls ---

func (c *Client) Me(ctx context.Context) (Me, error) {
	var me Me
	return me, c.do(ctx, http.MethodGet, "/api/v1/auth/me", nil, nil, &me)
}

func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var out []Project
	return out, c.do(ctx, http.MethodGet, "/api/v1/projects", nil, nil, &out)
}

func (c *Client) CreateProject(ctx context.Context, in NewProject) (Project, error) {
	var out Project
	return out, c.do(ctx, http.MethodPost, "/api/v1/projects", nil, in, &out)
}

func (c *Client) Tasks(ctx context.Context, projectID string) ([]Task, error) {
	var out []Task
	return out, c.do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(projectID)+"/tasks", nil, nil, &out)
}

func (c *Client) CreateTask(ctx context.Context, projectID string, in NewTask) (Task, error) {
	var out Task
	return out, c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(projectID)+"/tasks", nil, in, &out)
}

func (c *Client) Task(ctx context.Context, id string) (TaskDetail, error) {
	var out TaskDetail
	return out, c.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(id), nil, nil, &out)
}

func (c *Client) Advance(ctx context.Context, id, reason string) (Task, error) {
	var out Task
	var in any
	if reason != "" {
		in = map[string]string{"reason": reason}
	}
	return out, c.do(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(id)+"/advance", nil, in, &out)
}

func (c *Client) Approve(ctx context.Context, id string) (Task, error) {
	var out Task
	return out, c.do(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(id)+"/approve", nil, nil, &out)
}

func (c *Client) TaskJobs(ctx context.Context, taskID string) ([]Job, error) {
	var out []Job
	return out, c.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID)+"/jobs", nil, nil, &out)
}

func (c *Client) CreateJob(ctx context.Context, taskID string, in NewJob) (Job, error) {
	var out Job
	return out, c.do(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/jobs", nil, in, &out)
}

func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	var out Job
	return out, c.do(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(id), nil, nil, &out)
}

// JobEvents returns stored output after seq, at most 1000 at a time (the contract's limit).
func (c *Client) JobEvents(ctx context.Context, jobID string, after int64) ([]JobEvent, error) {
	var out []JobEvent
	q := url.Values{"after": {fmt.Sprint(after)}, "limit": {"1000"}}
	return out, c.do(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(jobID)+"/events", q, nil, &out)
}

// AuthConfig says whether the server offers sign-in through a provider, and which.
type AuthConfig struct {
	OIDC     bool   `json:"oidc"`
	Provider string `json:"provider"`
}

func (c *Client) AuthConfig(ctx context.Context) (AuthConfig, error) {
	var out AuthConfig
	err := c.do(ctx, http.MethodGet, "/auth/config", nil, nil, &out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		// A server from before native sign-in.
		return AuthConfig{}, nil
	}
	return out, err
}

type tokenRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
	ClientName   string `json:"client_name"`
}

type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Client) ExchangeCode(ctx context.Context, in tokenRequest) (tokenResponse, error) {
	var out tokenResponse
	return out, c.do(ctx, http.MethodPost, "/auth/token", nil, in, &out)
}

func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/auth/logout", nil, nil, nil)
}
