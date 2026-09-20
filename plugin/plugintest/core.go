// Package plugintest runs Gator plugins without gator-server: an in-memory core that answers
// the core methods the way the server does, a process runner, and JSON scenarios that drive
// hooks. The gator-plugin dev command is a thin wrapper around it; Go plugins can use it in
// their own tests.
package plugintest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/onegator/gator/plugin"
)

// firstPhase mirrors the server's default templates.
var firstPhase = map[string]string{"feature": "idea", "bug": "planning", "incident": "implementation", "chore": "implementation"}

// Task is a task in the fake core.
type Task struct {
	plugin.TaskRef
	Description string `json:"description,omitempty"`
	Urgency     int    `json:"urgency,omitempty"`
}

// Artifact is a document or link a plugin stored.
type Artifact struct {
	TaskID  string `json:"task_id"`
	Phase   string `json:"phase"`
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	URL     string `json:"url,omitempty"`
	Version int    `json:"version"`
}

// Call is one core method call made by the plugin.
type Call struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Incident is what the fake core remembers about a fault: one per fingerprint, however many
// times a monitoring tool repeats itself, which is the behaviour a plugin must not work around.
type Incident struct {
	plugin.IncidentUpsertParams
	ID     string `json:"id"`
	Count  int    `json:"count"`
	TaskID string `json:"task_id,omitempty"`
	Closed bool   `json:"closed"`
}

// Core is an in-memory stand-in for the core methods of one project.
type Core struct {
	ProjectID string

	mu        sync.Mutex
	tasks     []*Task
	checks    map[string][]plugin.Check
	artifacts []Artifact
	kv        map[string]json.RawMessage
	releases  []plugin.ReleaseRecordParams
	incidents map[string]*Incident
	logs      []plugin.LogParams
	calls     []Call
}

// NewCore creates an empty project.
func NewCore(projectID string) *Core {
	if projectID == "" {
		projectID = newID()
	}
	return &Core{ProjectID: projectID, checks: map[string][]plugin.Check{}, kv: map[string]json.RawMessage{},
		incidents: map[string]*Incident{}}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// AddTask seeds a task; empty ID and phase are filled in.
func (c *Core) AddTask(t plugin.TaskRef) plugin.TaskRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.ID == "" {
		t.ID = newID()
	}
	if t.Phase == "" {
		t.Phase = firstPhase[t.Kind]
	}
	t.ProjectID = c.ProjectID
	if t.ExternalRefs == nil {
		t.ExternalRefs = map[string]string{}
	}
	c.tasks = append(c.tasks, &Task{TaskRef: t})
	return t
}

// Task returns a task by id.
func (c *Core) Task(id string) (plugin.TaskRef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.find(id); t != nil {
		return t.TaskRef, true
	}
	return plugin.TaskRef{}, false
}

func (c *Core) find(id string) *Task {
	for _, t := range c.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Tasks returns every task.
func (c *Core) Tasks() []Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Task, len(c.tasks))
	for i, t := range c.tasks {
		out[i] = *t
	}
	return out
}

// Checks returns a task's gate checks.
func (c *Core) Checks(taskID string) []plugin.Check {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]plugin.Check(nil), c.checks[taskID]...)
}

// AllChecks returns the checks of every task.
func (c *Core) AllChecks() map[string][]plugin.Check {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]plugin.Check{}
	for k, v := range c.checks {
		out[k] = append([]plugin.Check(nil), v...)
	}
	return out
}

// SetCheck stores a check the way the server does: one per name on the current gate.
func (c *Core) SetCheck(taskID string, ch plugin.Check) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCheck(taskID, ch)
}

func (c *Core) setCheck(taskID string, ch plugin.Check) {
	list := c.checks[taskID]
	for i := range list {
		if list[i].Name == ch.Name {
			list[i] = ch
			return
		}
	}
	c.checks[taskID] = append(list, ch)
}

// Artifacts returns what the plugin stored.
func (c *Core) Artifacts() []Artifact {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Artifact(nil), c.artifacts...)
}

// KV returns the plugin's key-value store.
func (c *Core) KV() map[string]json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]json.RawMessage{}
	for k, v := range c.kv {
		out[k] = v
	}
	return out
}

// Logs returns what the plugin logged through the core.
// Releases are what the plugin has said reached an environment, in the order it said so.
func (c *Core) Releases() []plugin.ReleaseRecordParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]plugin.ReleaseRecordParams(nil), c.releases...)
}

// Incidents are the faults reported, by fingerprint.
func (c *Core) Incidents() map[string]Incident {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]Incident{}
	for k, v := range c.incidents {
		out[k] = *v
	}
	return out
}

func (c *Core) Logs() []plugin.LogParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]plugin.LogParams(nil), c.logs...)
}

// Calls returns every core call so far.
func (c *Core) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Call(nil), c.calls...)
}

// Handler answers the plugin's calls and records them.
func (c *Core) Handler() plugin.Handler {
	return func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		res, err := c.handle(method, raw)
		call := Call{Method: method, Params: raw}
		if err != nil {
			call.Error = err.Error()
		} else if res != nil {
			call.Result, _ = json.Marshal(res)
		}
		c.mu.Lock()
		c.calls = append(c.calls, call)
		c.mu.Unlock()
		return res, err
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, plugin.Errorf(plugin.CodeInvalidParams, "%v", err)
	}
	return v, nil
}

func (c *Core) handle(method string, raw json.RawMessage) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	notFound := func(id string) error {
		return plugin.Errorf(plugin.CodeNotFound, "task %s not found in this project", id)
	}
	switch method {
	case plugin.CoreTaskCreate:
		p, err := decode[plugin.TaskCreateParams](raw)
		if err != nil {
			return nil, err
		}
		phase, ok := firstPhase[p.Kind]
		if !ok || strings.TrimSpace(p.Title) == "" {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "kind must be feature, bug, incident or chore, and title is required")
		}
		refs := map[string]string{}
		for k, v := range p.ExternalRefs {
			refs[k] = v
		}
		t := &Task{TaskRef: plugin.TaskRef{ID: newID(), ProjectID: c.ProjectID, Kind: p.Kind, Title: p.Title, Phase: phase, ExternalRefs: refs},
			Description: p.Description, Urgency: p.Urgency}
		c.tasks = append(c.tasks, t)
		return t.TaskRef, nil
	case plugin.CoreTaskFind:
		p, err := decode[plugin.TaskFindParams](raw)
		if err != nil {
			return nil, err
		}
		for i := len(c.tasks) - 1; i >= 0; i-- {
			if c.tasks[i].ExternalRefs[p.Key] == p.Value {
				return c.tasks[i].TaskRef, nil
			}
		}
		return nil, nil
	case plugin.CoreTaskUpdate:
		p, err := decode[plugin.TaskUpdateParams](raw)
		if err != nil {
			return nil, err
		}
		t := c.find(p.TaskID)
		if t == nil {
			return nil, notFound(p.TaskID)
		}
		for k, v := range p.ExternalRefs {
			t.ExternalRefs[k] = v
		}
		return t.TaskRef, nil
	case plugin.CoreGateSetCheck:
		p, err := decode[plugin.SetCheckParams](raw)
		if err != nil {
			return nil, err
		}
		if c.find(p.TaskID) == nil {
			return nil, notFound(p.TaskID)
		}
		if p.Name == "" || (p.Status != "pass" && p.Status != "fail" && p.Status != "pending") {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "check needs a name and status pass, fail or pending")
		}
		c.setCheck(p.TaskID, p.Check)
		return nil, nil
	case plugin.CoreArtifactPut:
		p, err := decode[plugin.ArtifactPutParams](raw)
		if err != nil {
			return nil, err
		}
		t := c.find(p.TaskID)
		if t == nil {
			return nil, notFound(p.TaskID)
		}
		if p.Type == "" || (p.Content == "" && p.URL == "") {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "type and content or url are required")
		}
		if p.Phase == "" {
			p.Phase = t.Phase
		}
		v := 1
		for _, a := range c.artifacts {
			if a.TaskID == p.TaskID && a.Phase == p.Phase && a.Type == p.Type && a.Version >= v {
				v = a.Version + 1
			}
		}
		c.artifacts = append(c.artifacts, Artifact{TaskID: p.TaskID, Phase: p.Phase, Type: p.Type, Content: p.Content, URL: p.URL, Version: v})
		return plugin.ArtifactPutResult{Version: v}, nil
	case plugin.CoreKVGet:
		p, err := decode[plugin.KVGetParams](raw)
		if err != nil {
			return nil, err
		}
		v, ok := c.kv[p.Key]
		return plugin.KVGetResult{Found: ok, Value: v}, nil
	case plugin.CoreKVPut:
		p, err := decode[plugin.KVPutParams](raw)
		if err != nil {
			return nil, err
		}
		if p.Key == "" || len(p.Key) > 200 || len(p.Value) > 64<<10 || !json.Valid(p.Value) {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "key must be 1-200 bytes and value valid JSON up to 64 KiB")
		}
		c.kv[p.Key] = p.Value
		return nil, nil
	case plugin.CoreLog:
		p, err := decode[plugin.LogParams](raw)
		if err != nil {
			return nil, err
		}
		c.logs = append(c.logs, p)
		return nil, nil
	case plugin.CoreReleaseRecord:
		p, err := decode[plugin.ReleaseRecordParams](raw)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Version) == "" {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "a release needs a version")
		}
		if p.Environment == "" {
			p.Environment = "production"
		}
		for i, r := range c.releases {
			if r.Version == p.Version && r.Environment == p.Environment {
				c.releases[i] = p // deploying the same version again updates it
				return plugin.ReleaseRecordResult{ReleaseID: newID()}, nil
			}
		}
		c.releases = append(c.releases, p)
		return plugin.ReleaseRecordResult{ReleaseID: newID()}, nil
	case plugin.CoreIncidentUpsert:
		p, err := decode[plugin.IncidentUpsertParams](raw)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Fingerprint) == "" || strings.TrimSpace(p.Title) == "" {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "an incident needs a fingerprint and a title")
		}
		if inc, ok := c.incidents[p.Fingerprint]; ok {
			inc.Count++
			inc.IncidentUpsertParams = p
			return plugin.IncidentUpsertResult{IncidentID: inc.ID, TaskID: inc.TaskID}, nil
		}
		task := &Task{TaskRef: plugin.TaskRef{ID: newID(), ProjectID: c.ProjectID, Kind: "incident",
			Title: p.Title, Phase: firstPhase["incident"], ExternalRefs: map[string]string{}}}
		c.tasks = append(c.tasks, task)
		inc := &Incident{IncidentUpsertParams: p, ID: newID(), Count: 1, TaskID: task.ID}
		c.incidents[p.Fingerprint] = inc
		return plugin.IncidentUpsertResult{IncidentID: inc.ID, TaskID: inc.TaskID, Created: true}, nil
	case plugin.CoreIncidentClose:
		p, err := decode[plugin.IncidentCloseParams](raw)
		if err != nil {
			return nil, err
		}
		if inc, ok := c.incidents[p.Fingerprint]; ok {
			inc.Closed = true
		}
		return nil, nil
	}
	return nil, plugin.Errorf(plugin.CodeMethodNotFound, "method %q not found", method)
}
