package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func cmdTaskLogs(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("task logs")
	follow := fs.Bool("f", false, "keep printing until the job ends")
	fs.BoolVar(follow, "follow", false, "same as -f")
	jobID := fs.String("job", "", "a particular job; the task's latest when omitted")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	id := ""
	if *jobID == "" || len(positional) > 0 {
		if id, err = oneArg(positional, "task id"); err != nil {
			return err
		}
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	job, err := pickJob(ctx, s.client, id, *jobID)
	if err != nil {
		return err
	}
	if !g.json {
		fmt.Fprintf(env.Stderr, "Job %s · %s · %s · %s\n", job.ID, job.Phase, job.Role, job.Status)
	}
	return streamJob(ctx, env, s, job, g.json, *follow)
}

// pickJob is the job a person means: the one named, else the task's latest. An id that is not
// a task is tried as a job, since both are UUIDs and easy to mix up.
func pickJob(ctx context.Context, c *Client, taskID, jobID string) (Job, error) {
	if jobID != "" {
		return c.Job(ctx, jobID)
	}
	jobs, err := c.TaskJobs(ctx, taskID)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		if j, jerr := c.Job(ctx, taskID); jerr == nil {
			return j, nil
		}
	}
	if err != nil {
		return Job{}, err
	}
	if len(jobs) == 0 {
		return Job{}, coded(exitUserErr, "task %s has no jobs yet; start one with gator task run %s", taskID, taskID)
	}
	sort.SliceStable(jobs, func(i, k int) bool { return jobs[i].CreatedAt.Before(jobs[k].CreatedAt) })
	return jobs[len(jobs)-1], nil
}

// streamJob prints what the job has said so far and, when following, what it says next until
// it ends. Live output comes over the WebSocket (topic job:<id>); where that cannot be opened —
// a proxy that drops upgrades, an older server — it polls the stored events instead.
func streamJob(ctx context.Context, env *Env, s *session, job Job, jsonOut, follow bool) error {
	p := &printer{env: env, json: jsonOut}
	var live *websocket.Conn
	if follow && !job.Finished() {
		// Subscribe before reading the backlog, so nothing said in between is lost; the seq
		// numbers drop what arrives twice.
		var err error
		live, err = dialJob(ctx, s, job.ID)
		if err != nil {
			fmt.Fprintf(env.Stderr, "(no live stream: %v; checking every %s)\n", err, pollInterval(env))
		} else {
			defer live.CloseNow()
		}
	}
	if err := p.catchUp(ctx, s.client, job.ID); err != nil {
		return err
	}
	if !follow || job.Finished() {
		return p.end(job, follow)
	}
	if live != nil {
		done, err := followLive(ctx, env, s, p, live, job.ID)
		if err == nil {
			return p.end(done, true)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(env.Stderr, "(live stream lost: %v; checking every %s)\n", err, pollInterval(env))
	}
	done, err := followPolling(ctx, env, s, p, job.ID)
	if err != nil {
		return err
	}
	return p.end(done, true)
}

func pollInterval(env *Env) time.Duration {
	if env.PollInterval > 0 {
		return env.PollInterval
	}
	return 2 * time.Second
}

func dialJob(ctx context.Context, s *session, jobID string) (*websocket.Conn, error) {
	wsURL := "ws" + strings.TrimPrefix(s.server, "http") + "/api/v1/ws"
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: s.client.HTTP,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + s.client.Token}},
	})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	sub, _ := json.Marshal(map[string][]string{"subscribe": {"job:" + jobID}})
	if err := conn.Write(dialCtx, websocket.MessageText, sub); err != nil {
		conn.CloseNow()
		return nil, err
	}
	return conn, nil
}

// hubEvent is one message on the WebSocket.
type hubEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// followLive prints output as it arrives until the job ends. Every so often it also asks for
// the job itself: the hub drops events for a slow reader, and a missed job.finished must not
// leave the command waiting forever.
func followLive(ctx context.Context, env *Env, s *session, p *printer, conn *websocket.Conn, jobID string) (Job, error) {
	msgs := make(chan hubEvent)
	readErr := make(chan error, 1)
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			_, raw, err := conn.Read(readCtx)
			if err != nil {
				readErr <- err
				return
			}
			var e hubEvent
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			select {
			case msgs <- e:
			case <-readCtx.Done():
				return
			}
		}
	}()
	check := time.NewTicker(15 * time.Second)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return Job{}, ctx.Err()
		case err := <-readErr:
			return Job{}, err
		case e := <-msgs:
			switch {
			case e.Type == "job.output":
				var ev JobEvent
				if json.Unmarshal(e.Payload, &ev) != nil {
					continue
				}
				if ev.Seq > p.last+1 {
					// Something was dropped on the way; the stored events have it.
					if err := p.catchUp(ctx, s.client, jobID); err != nil {
						return Job{}, err
					}
				}
				p.event(ev)
			case e.Type == "job.finished":
				return finish(ctx, s, p, jobID)
			}
		case <-check.C:
			j, err := s.client.Job(ctx, jobID)
			if err != nil {
				return Job{}, err
			}
			if j.Finished() {
				return finish(ctx, s, p, jobID)
			}
		}
	}
}

func followPolling(ctx context.Context, env *Env, s *session, p *printer, jobID string) (Job, error) {
	t := time.NewTicker(pollInterval(env))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return Job{}, ctx.Err()
		case <-t.C:
		}
		if err := p.catchUp(ctx, s.client, jobID); err != nil {
			return Job{}, err
		}
		j, err := s.client.Job(ctx, jobID)
		if err != nil {
			return Job{}, err
		}
		if j.Finished() {
			return finish(ctx, s, p, jobID)
		}
	}
}

// finish reads the last of the output, which can land after the job's own end, and the job.
func finish(ctx context.Context, s *session, p *printer, jobID string) (Job, error) {
	if err := p.catchUp(ctx, s.client, jobID); err != nil {
		return Job{}, err
	}
	return s.client.Job(ctx, jobID)
}

// printer writes job output once each, in seq order, as text or as one JSON object a line.
type printer struct {
	env  *Env
	json bool
	last int64
}

func (p *printer) catchUp(ctx context.Context, c *Client, jobID string) error {
	for {
		events, err := c.JobEvents(ctx, jobID, p.last)
		if err != nil {
			return err
		}
		for _, e := range events {
			p.event(e)
		}
		if len(events) < 1000 {
			return nil
		}
	}
}

func (p *printer) event(e JobEvent) {
	if e.Seq <= p.last {
		return
	}
	p.last = e.Seq
	if p.json {
		raw, _ := json.Marshal(e)
		fmt.Fprintln(p.env.Stdout, string(raw))
		return
	}
	if line := renderEvent(e); line != "" {
		fmt.Fprintln(p.env.Stdout, line)
	}
}

// end says how the job ended. A followed job that did not succeed fails the command, so
// `gator task run -f` can gate a script.
func (p *printer) end(j Job, followed bool) error {
	if !j.Finished() {
		if !p.json {
			fmt.Fprintf(p.env.Stderr, "Job %s is %s; follow it with -f.\n", j.ID, j.Status)
		}
		return nil
	}
	if !p.json {
		line := fmt.Sprintf("Job %s %s", j.ID, j.Status)
		if j.StopReason != "" {
			line += ": " + j.StopReason
		}
		fmt.Fprintln(p.env.Stderr, line)
	}
	if followed && j.Status != "done" {
		return coded(exitOther, "job %s %s", j.ID, orDefault(j.StopReason, j.Status))
	}
	return nil
}

// renderEvent turns one item of agent output into a line a person can skim.
func renderEvent(e JobEvent) string {
	str := func(k string) string { s, _ := e.Payload[k].(string); return s }
	switch e.Type {
	case "text":
		return strings.TrimRight(str("text"), "\n")
	case "tool_call":
		return "→ " + str("name") + " " + summarize(e.Payload["input"])
	case "tool_result":
		mark := "  ←"
		if isErr, _ := e.Payload["is_error"].(bool); isErr {
			mark = "  ✗"
		}
		return mark + " " + firstLine(str("content"), 160)
	case "result":
		line := "— " + orDefault(str("subtype"), "finished")
		if n, ok := e.Payload["num_turns"].(float64); ok {
			line += fmt.Sprintf(", %d turns", int(n))
		}
		if cost, ok := e.Payload["cost_usd"].(float64); ok {
			line += fmt.Sprintf(", $%.2f", cost)
		}
		return line
	case "session":
		return "· session started" + parenthesized(str("model"))
	case "rate_limit":
		return "· rate limit" + parenthesized(str("status"))
	}
	return "· " + e.Type
}

func parenthesized(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// summarize shows what a tool was asked to do: the command or path when there is one, the
// input itself otherwise, cut to a line.
func summarize(input any) string {
	if m, ok := input.(map[string]any); ok {
		for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description"} {
			if s, ok := m[k].(string); ok && s != "" {
				return firstLine(s, 160)
			}
		}
	}
	if s, ok := input.(string); ok {
		return firstLine(s, 160)
	}
	raw, _ := json.Marshal(input)
	return firstLine(string(raw), 160)
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	cut := false
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s, cut = s[:i], true
	}
	if r := []rune(s); len(r) > max {
		s, cut = string(r[:max]), true
	}
	if cut {
		s += " …"
	}
	return s
}
