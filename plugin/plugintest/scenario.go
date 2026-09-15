package plugintest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/onegator/gator/plugin"
)

// Scenario is a project, its settings, seeded tasks and the hooks to call in order.
type Scenario struct {
	Project struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"project"`
	Config  map[string]any    `json:"config"`
	Secrets map[string]string `json:"secrets"`
	Tasks   []plugin.TaskRef  `json:"tasks"`
	Steps   []Step            `json:"steps"`
}

// Step is one hook call. Fields apply to the hooks that use them.
type Step struct {
	Hook        string            `json:"hook"`
	Task        string            `json:"task,omitempty"` // id of a seeded or created task
	From        string            `json:"from,omitempty"`
	To          string            `json:"to,omitempty"`
	Rollback    bool              `json:"rollback,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Phase       string            `json:"phase,omitempty"`
	Name        string            `json:"name,omitempty"` // schedule name
	Job         *plugin.JobRef    `json:"job,omitempty"`
	Receipt     json.RawMessage   `json:"receipt,omitempty"`
	Delivery    string            `json:"delivery,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        json.RawMessage   `json:"body,omitempty"`      // webhook body, sent as these bytes
	BodyFile    string            `json:"body_file,omitempty"` // or a file next to the scenario
	Sign        *Signature        `json:"sign,omitempty"`
	ExpectError bool              `json:"expect_error,omitempty"`
	ExpectCore  []string          `json:"expect_core,omitempty"` // core methods the plugin must call in this step
}

// Signature adds an HMAC-SHA256 of the webhook body, keyed with a secret setting, the way
// GitHub-style senders sign deliveries.
type Signature struct {
	Header string `json:"header"` // e.g. X-Hub-Signature-256
	Secret string `json:"secret"` // name of the secret setting holding the key
	Prefix string `json:"prefix"` // e.g. "sha256="
}

// Sign returns prefix + hex(HMAC-SHA256(key, body)).
func Sign(key string, body []byte, prefix string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write(body)
	return prefix + hex.EncodeToString(m.Sum(nil))
}

// LoadScenario reads a scenario file; relative body files resolve next to it.
func LoadScenario(path string) (Scenario, error) {
	var s Scenario
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for i := range s.Steps {
		if f := s.Steps[i].BodyFile; f != "" && !filepath.IsAbs(f) {
			s.Steps[i].BodyFile = filepath.Join(dir, f)
		}
	}
	return s, nil
}

// StepResult is what one step did.
type StepResult struct {
	Hook      string          `json:"hook"`
	Error     string          `json:"error,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	CoreCalls []Call          `json:"core_calls"`
	Problem   string          `json:"problem,omitempty"` // an unmet expectation; fails the run
}

// Report is a scenario run: the manifest, each step and the core's final state.
type Report struct {
	Manifest  plugin.Manifest            `json:"manifest"`
	Steps     []StepResult               `json:"steps"`
	Tasks     []Task                     `json:"tasks"`
	Checks    map[string][]plugin.Check  `json:"checks"`
	Artifacts []Artifact                 `json:"artifacts"`
	KV        map[string]json.RawMessage `json:"kv"`
	Logs      []plugin.LogParams         `json:"logs"`
}

// Failed reports whether any step missed its expectation.
func (r Report) Failed() bool {
	for _, s := range r.Steps {
		if s.Problem != "" {
			return true
		}
	}
	return false
}

// Run starts the plugin, plays the scenario and stops the plugin.
func Run(ctx context.Context, command []string, s Scenario, stderr io.Writer, timeout time.Duration) (Report, error) {
	core := NewCore(s.Project.ID)
	for _, t := range s.Tasks {
		core.AddTask(t)
	}
	slug := s.Project.Slug
	if slug == "" {
		slug = "demo"
	}
	p, err := Start(ctx, command, core, Options{ProjectSlug: slug, Config: s.Config, Secrets: s.Secrets, Stderr: stderr, Timeout: timeout})
	if err != nil {
		return Report{}, err
	}
	defer p.Close()
	rep := Report{Manifest: p.Manifest}
	for n, step := range s.Steps {
		rep.Steps = append(rep.Steps, p.play(ctx, n, step, s.Secrets))
	}
	rep.Tasks, rep.Checks, rep.Artifacts, rep.KV, rep.Logs = core.Tasks(), core.AllChecks(), core.Artifacts(), core.KV(), core.Logs()
	return rep, nil
}

func (p *Process) play(ctx context.Context, n int, st Step, secrets map[string]string) StepResult {
	res := StepResult{Hook: st.Hook, CoreCalls: []Call{}}
	if !slices.Contains(plugin.Hooks, st.Hook) {
		res.Problem = fmt.Sprintf("unknown hook %q", st.Hook)
		return res
	}
	if !p.Manifest.HasHook(st.Hook) {
		res.Problem = fmt.Sprintf("the manifest does not declare %q", st.Hook)
		return res
	}
	params, problem := p.params(n, st, secrets)
	if problem != "" {
		res.Problem = problem
		return res
	}
	before := len(p.Core.Calls())
	var raw json.RawMessage
	err := p.Call(ctx, st.Hook, params, &raw)
	res.CoreCalls = append(res.CoreCalls, p.Core.Calls()[before:]...)
	if err != nil {
		res.Error = err.Error()
	} else if len(raw) > 0 && string(raw) != "null" {
		res.Result = raw
	}
	if st.Hook == plugin.MethodGateEvaluate && err == nil {
		var r plugin.GateEvaluateResult
		if json.Unmarshal(raw, &r) == nil {
			for _, c := range r.Checks {
				p.Core.SetCheck(st.Task, c)
			}
		}
	}
	switch {
	case st.ExpectError && err == nil:
		res.Problem = "expected an error"
	case !st.ExpectError && err != nil:
		res.Problem = err.Error()
	}
	for _, m := range st.ExpectCore {
		if !slices.ContainsFunc(res.CoreCalls, func(c Call) bool { return c.Method == m }) {
			res.Problem = fmt.Sprintf("expected a %s call", m)
		}
	}
	return res
}

func (p *Process) params(n int, st Step, secrets map[string]string) (any, string) {
	var task plugin.TaskRef
	if st.Task != "" {
		t, ok := p.Core.Task(st.Task)
		if !ok {
			return nil, fmt.Sprintf("no task %q in the scenario", st.Task)
		}
		task = t
	}
	needTask := func() string {
		if st.Task == "" {
			return "this hook needs \"task\""
		}
		return ""
	}
	job := plugin.JobRef{ID: fmt.Sprintf("job-%d", n+1), Role: "worker", Phase: task.Phase, Backend: "claude", Status: "done"}
	if st.Job != nil {
		job = *st.Job
	}
	phase := st.Phase
	if phase == "" {
		phase = task.Phase
	}
	switch st.Hook {
	case plugin.MethodWebhook:
		body := []byte(st.Body)
		if st.BodyFile != "" {
			b, err := os.ReadFile(st.BodyFile)
			if err != nil {
				return nil, err.Error()
			}
			body = b
		}
		headers := map[string]string{}
		for k, v := range st.Headers {
			headers[http.CanonicalHeaderKey(k)] = v
		}
		if st.Sign != nil {
			key, ok := secrets[st.Sign.Secret]
			if !ok {
				return nil, fmt.Sprintf("sign: no secret %q in the scenario", st.Sign.Secret)
			}
			headers[http.CanonicalHeaderKey(st.Sign.Header)] = Sign(key, body, st.Sign.Prefix)
		}
		delivery := st.Delivery
		if delivery == "" {
			delivery = fmt.Sprintf("delivery-%d", n+1)
		}
		return plugin.WebhookParams{DeliveryID: delivery, Headers: headers, Body: body}, ""
	case plugin.MethodPhaseTransition:
		return plugin.PhaseTransitionParams{Task: task, From: st.From, To: st.To, Rollback: st.Rollback, Reason: st.Reason}, needTask()
	case plugin.MethodGateEvaluate:
		return plugin.GateEvaluateParams{Task: task, Phase: phase}, needTask()
	case plugin.MethodArtifactApproved:
		return plugin.ArtifactApprovedParams{Task: task, Phase: phase}, needTask()
	case plugin.MethodSchedule:
		return plugin.ScheduleParams{Name: st.Name}, ""
	case plugin.MethodRenderUI:
		return plugin.RenderUIParams{Task: task}, needTask()
	case plugin.MethodJobPrepare:
		return plugin.JobPrepareParams{Task: task, Job: job}, needTask()
	case plugin.MethodJobFinish:
		receipt := st.Receipt
		if len(receipt) == 0 {
			receipt = json.RawMessage(`{"status":"done"}`)
		}
		return plugin.JobFinishParams{Task: task, Job: job, Receipt: receipt}, needTask()
	}
	return nil, fmt.Sprintf("%s cannot be played yet", st.Hook)
}
