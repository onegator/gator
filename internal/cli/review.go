package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// --- what each turn changed, and what agents wait for a person on ---

// FileChange is one file a turn changed.
type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Status  string `json:"status"`
	Binary  bool   `json:"binary,omitempty"`
}

// Checkpoint is one turn of a job, checkpointed.
type Checkpoint struct {
	ID         string       `json:"id"`
	JobID      string       `json:"jobId"`
	Seq        int          `json:"seq"`
	Repo       string       `json:"repo"`
	Files      []FileChange `json:"files"`
	Truncated  bool         `json:"truncated"`
	WholeFiles []FileChange `json:"wholeFiles"`
	TookMS     int          `json:"tookMs"`
	CreatedAt  time.Time    `json:"createdAt"`
}

// RuntimeRequest is an agent waiting for a person.
type RuntimeRequest struct {
	ID        string         `json:"id"`
	JobID     string         `json:"jobId"`
	Kind      string         `json:"kind"`
	Tool      string         `json:"tool,omitempty"`
	Input     map[string]any `json:"input"`
	Rule      string         `json:"rule"`
	Status    string         `json:"status"`
	Feedback  string         `json:"feedback,omitempty"`
	ExpiresAt time.Time      `json:"expiresAt"`
	CreatedAt time.Time      `json:"createdAt"`
}

func (c *Client) Checkpoints(ctx context.Context, jobID string) ([]Checkpoint, error) {
	var out []Checkpoint
	return out, c.do(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(jobID)+"/checkpoints", nil, nil, &out)
}

// JobDiff is a job's diff over turns from..to (0: the first, the last), as text.
func (c *Client) JobDiff(ctx context.Context, jobID string, from, to int, ignoreWhitespace bool) ([]byte, error) {
	q := url.Values{}
	if from > 0 {
		q.Set("from", strconv.Itoa(from))
	}
	if to > 0 {
		q.Set("to", strconv.Itoa(to))
	}
	if ignoreWhitespace {
		q.Set("ignoreWhitespace", "true")
	}
	return c.text(ctx, "/api/v1/jobs/"+url.PathEscape(jobID)+"/diff", q)
}

func (c *Client) WaitingRequests(ctx context.Context, projectID string) ([]RuntimeRequest, error) {
	var out []RuntimeRequest
	q := url.Values{}
	if projectID != "" {
		q.Set("projectId", projectID)
	}
	return out, c.do(ctx, http.MethodGet, "/api/v1/runtime-requests", q, nil, &out)
}

func (c *Client) Decide(ctx context.Context, id, decision, feedback string) (RuntimeRequest, error) {
	in := map[string]string{"decision": decision}
	if feedback != "" {
		in["feedback"] = feedback
	}
	var out RuntimeRequest
	return out, c.do(ctx, http.MethodPost, "/api/v1/runtime-requests/"+url.PathEscape(id)+"/decide", nil, in, &out)
}

// text is a GET answered with text (a diff), with the same errors as do.
func (c *Client) text(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, coded(exitUserErr, "%v", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, coded(exitNetwork, "cannot reach Gator at %s: %v", c.Base, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if res.StatusCode/100 != 2 {
		return nil, &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(bytes.TrimSpace(raw)))}
	}
	return raw, nil
}

// cmdJobCheckpoints lists what each turn of a job changed.
func cmdJobCheckpoints(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("job checkpoints")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return coded(exitUserErr, "usage: gator job checkpoints JOB_ID")
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	cps, err := s.client.Checkpoints(ctx, rest[0])
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, cps)
	}
	if len(cps) == 0 {
		fmt.Fprintln(env.Stdout, "No checkpoints: the job did not change files turn by turn (or reads only).")
		return nil
	}
	tw := table(env)
	fmt.Fprintln(tw, "TURN\tREPO\tFILES\tCHANGES\tTOOK")
	for _, cp := range cps {
		added, removed := 0, 0
		for _, f := range cp.Files {
			added, removed = added+f.Added, removed+f.Removed
		}
		note := ""
		if cp.Truncated {
			note = " (diff cut)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t+%d −%d%s\t%dms\n", cp.Seq, cp.Repo, len(cp.Files), added, removed, note, cp.TookMS)
	}
	return tw.Flush()
}

// cmdJobDiff prints a job's diff: all of it, or some turns.
func cmdJobDiff(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("job diff")
	from := fs.Int("from", 0, "the first turn")
	to := fs.Int("to", 0, "the last turn")
	ws := fs.Bool("ignore-whitespace", false, "leave out changes of whitespace only")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return coded(exitUserErr, "usage: gator job diff JOB_ID [--from N] [--to N] [--ignore-whitespace]")
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	diff, err := s.client.JobDiff(ctx, rest[0], *from, *to, *ws)
	if err != nil {
		return err
	}
	_, err = env.Stdout.Write(diff)
	return err
}

// cmdApprovals lists what agents wait for the caller to decide.
func cmdApprovals(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("approvals")
	project := fs.String("project", "", "only this project (slug)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	projectID := ""
	if *project != "" {
		p, err := resolveProject(ctx, s.client, *project)
		if err != nil {
			return err
		}
		projectID = p.ID
	}
	reqs, err := s.client.WaitingRequests(ctx, projectID)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, reqs)
	}
	if len(reqs) == 0 {
		fmt.Fprintln(env.Stdout, "Nothing waits for you.")
		return nil
	}
	tw := table(env)
	fmt.Fprintln(tw, "ID\tKIND\tTOOL\tWHAT\tEXPIRES")
	for _, r := range reqs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Kind, orDefault(r.Tool, "-"), what(r), r.ExpiresAt.Format(time.Kitchen))
	}
	return tw.Flush()
}

// what is one line of what a request is about.
func what(r RuntimeRequest) string {
	for _, k := range []string{"command", "url", "file_path", "question"} {
		if s, ok := r.Input[k].(string); ok && s != "" {
			s = strings.Join(strings.Fields(s), " ")
			if len(s) > 60 {
				s = s[:60] + "…"
			}
			return s
		}
	}
	return orDefault(r.Rule, "-")
}

// cmdDecide decides what an agent waits on: allow, allow-session, deny or stop.
func cmdDecide(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("decide")
	reason := fs.String("reason", "", "why (deny), or the answer (a question)")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	decisions := map[string]string{"allow": "allow", "allow-session": "allow_for_session", "deny": "deny", "stop": "stop", "answer": "allow"}
	if len(rest) != 2 || decisions[rest[1]] == "" {
		return coded(exitUserErr, "usage: gator decide REQUEST_ID allow|allow-session|deny|stop|answer [--reason TEXT]")
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	r, err := s.client.Decide(ctx, rest[0], decisions[rest[1]], *reason)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, r)
	}
	fmt.Fprintf(env.Stdout, "%s: %s\n", r.ID, r.Status)
	return nil
}
