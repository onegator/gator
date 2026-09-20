package plugin

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
)

// Handlers is a Go plugin: its manifest and a function per hook it handles. Hooks left nil
// are not declared; Serve fills Manifest.Hooks from the non-nil ones when it is empty.
type Handlers struct {
	Manifest         Manifest
	Init             func(ctx context.Context, core *Core) error
	Webhook          func(ctx context.Context, core *Core, p WebhookParams) error
	PhaseTransition  func(ctx context.Context, core *Core, p PhaseTransitionParams) error
	GateEvaluate     func(ctx context.Context, core *Core, p GateEvaluateParams) (GateEvaluateResult, error)
	ArtifactApproved func(ctx context.Context, core *Core, p ArtifactApprovedParams) error
	Schedule         func(ctx context.Context, core *Core, p ScheduleParams) error
	RenderUI         func(ctx context.Context, core *Core, p RenderUIParams) (RenderUIResult, error)
	JobPrepare       func(ctx context.Context, core *Core, p JobPrepareParams) (JobPrepareResult, error)
	JobFinish        func(ctx context.Context, core *Core, p JobFinishParams) error
	// Release is the ask a deploy plugin answers: ship this task's work, then call
	// core.RecordRelease with the version once you know it.
	Release func(ctx context.Context, core *Core, p ReleaseParams) error
	// IncidentClosed says the fix landed, so the plugin can resolve the alert in whatever
	// tool raised it.
	IncidentClosed func(ctx context.Context, core *Core, p IncidentClosedParams) error
}

func (h *Handlers) hooks() []string {
	var out []string
	add := func(ok bool, name string) {
		if ok {
			out = append(out, name)
		}
	}
	add(h.Webhook != nil, MethodWebhook)
	add(h.PhaseTransition != nil, MethodPhaseTransition)
	add(h.GateEvaluate != nil, MethodGateEvaluate)
	add(h.ArtifactApproved != nil, MethodArtifactApproved)
	add(h.Schedule != nil, MethodSchedule)
	add(h.RenderUI != nil, MethodRenderUI)
	add(h.JobPrepare != nil, MethodJobPrepare)
	add(h.JobFinish != nil, MethodJobFinish)
	add(h.Release != nil, MethodRelease)
	add(h.IncidentClosed != nil, MethodIncidentClosed)
	return out
}

// Serve runs a plugin on stdin and stdout until the core closes the stream.
func Serve(h Handlers) error { return ServeIO(os.Stdin, os.Stdout, h) }

// ServeIO runs a plugin on the given streams.
func ServeIO(r io.Reader, w io.Writer, h Handlers) error {
	if len(h.Manifest.Hooks) == 0 {
		h.Manifest.Hooks = h.hooks()
	}
	if h.Manifest.Capabilities == nil {
		h.Manifest.Capabilities = []string{}
	}
	core := &Core{}
	conn := NewConn(r, w, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return dispatch(ctx, &h, core, method, params)
	})
	core.conn = conn
	<-conn.Done()
	if err := conn.Err(); err != ErrClosed {
		return err
	}
	return nil
}

func decodeParams[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, Errorf(CodeInvalidParams, "%v", err)
	}
	return v, nil
}

func dispatch(ctx context.Context, h *Handlers, core *Core, method string, raw json.RawMessage) (any, error) {
	notFound := Errorf(CodeMethodNotFound, "hook %q not handled", method)
	switch method {
	case MethodInitialize:
		p, err := decodeParams[InitializeParams](raw)
		if err != nil {
			return nil, err
		}
		core.setInit(p)
		if h.Init != nil && p.ProjectID != "" {
			if err := h.Init(ctx, core); err != nil {
				return nil, err
			}
		}
		return h.Manifest, nil
	case MethodShutdown:
		return nil, nil
	case MethodWebhook:
		if h.Webhook == nil {
			return nil, notFound
		}
		p, err := decodeParams[WebhookParams](raw)
		if err != nil {
			return nil, err
		}
		return nil, h.Webhook(ctx, core, p)
	case MethodPhaseTransition:
		if h.PhaseTransition == nil {
			return nil, notFound
		}
		p, err := decodeParams[PhaseTransitionParams](raw)
		if err != nil {
			return nil, err
		}
		return nil, h.PhaseTransition(ctx, core, p)
	case MethodGateEvaluate:
		if h.GateEvaluate == nil {
			return nil, notFound
		}
		p, err := decodeParams[GateEvaluateParams](raw)
		if err != nil {
			return nil, err
		}
		return h.GateEvaluate(ctx, core, p)
	case MethodArtifactApproved:
		if h.ArtifactApproved == nil {
			return nil, notFound
		}
		p, err := decodeParams[ArtifactApprovedParams](raw)
		if err != nil {
			return nil, err
		}
		return nil, h.ArtifactApproved(ctx, core, p)
	case MethodSchedule:
		if h.Schedule == nil {
			return nil, notFound
		}
		p, err := decodeParams[ScheduleParams](raw)
		if err != nil {
			return nil, err
		}
		return nil, h.Schedule(ctx, core, p)
	case MethodRenderUI:
		if h.RenderUI == nil {
			return nil, notFound
		}
		p, err := decodeParams[RenderUIParams](raw)
		if err != nil {
			return nil, err
		}
		return h.RenderUI(ctx, core, p)
	case MethodJobPrepare:
		if h.JobPrepare == nil {
			return nil, notFound
		}
		p, err := decodeParams[JobPrepareParams](raw)
		if err != nil {
			return nil, err
		}
		return h.JobPrepare(ctx, core, p)
	case MethodJobFinish:
		if h.JobFinish == nil {
			return nil, notFound
		}
		p, err := decodeParams[JobFinishParams](raw)
		if err != nil {
			return nil, err
		}
		return nil, h.JobFinish(ctx, core, p)
	case MethodRelease:
		if h.Release == nil {
			return nil, notFound
		}
		p, err := decodeParams[ReleaseParams](raw)
		if err != nil {
			return nil, err
		}
		return nil, h.Release(ctx, core, p)
	case MethodIncidentClosed:
		if h.IncidentClosed == nil {
			return nil, notFound
		}
		p, err := decodeParams[IncidentClosedParams](raw)
		if err != nil {
			return nil, err
		}
		return nil, h.IncidentClosed(ctx, core, p)
	}
	return nil, Errorf(CodeMethodNotFound, "method %q not found", method)
}

// Core is the plugin's handle on gator-server: its settings and the core methods.
type Core struct {
	conn *Conn
	mu   sync.RWMutex
	init InitializeParams
}

func (c *Core) setInit(p InitializeParams) {
	c.mu.Lock()
	c.init = p
	c.mu.Unlock()
}

// ProjectID is the project this process serves.
func (c *Core) ProjectID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.init.ProjectID
}

// Config is the project's non-secret settings.
func (c *Core) Config() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.init.Config
}

// Setting returns a non-secret string setting.
func (c *Core) Setting(key string) string {
	s, _ := c.Config()[key].(string)
	return s
}

// Secret returns a secret setting from the process environment.
func (c *Core) Secret(key string) string { return os.Getenv(SecretEnv(key)) }

// CreateTask creates a task in the project.
func (c *Core) CreateTask(ctx context.Context, p TaskCreateParams) (TaskRef, error) {
	var t TaskRef
	err := c.conn.Call(ctx, CoreTaskCreate, p, &t)
	return t, err
}

// FindTask returns the newest task with external_refs[key] == value, or nil.
func (c *Core) FindTask(ctx context.Context, key, value string) (*TaskRef, error) {
	var t *TaskRef
	err := c.conn.Call(ctx, CoreTaskFind, TaskFindParams{Key: key, Value: value}, &t)
	return t, err
}

// UpdateTask merges external references into a task.
func (c *Core) UpdateTask(ctx context.Context, p TaskUpdateParams) (TaskRef, error) {
	var t TaskRef
	err := c.conn.Call(ctx, CoreTaskUpdate, p, &t)
	return t, err
}

// SetCheck sets a check on the task's current gate.
func (c *Core) SetCheck(ctx context.Context, taskID string, check Check) error {
	return c.conn.Call(ctx, CoreGateSetCheck, SetCheckParams{TaskID: taskID, Check: check}, nil)
}

// PutArtifact stores a document or link on a task.
func (c *Core) PutArtifact(ctx context.Context, p ArtifactPutParams) (int, error) {
	var r ArtifactPutResult
	err := c.conn.Call(ctx, CoreArtifactPut, p, &r)
	return r.Version, err
}

// KVGet reads a value into out; found is false when the key is unset.
func (c *Core) KVGet(ctx context.Context, key string, out any) (bool, error) {
	var r KVGetResult
	if err := c.conn.Call(ctx, CoreKVGet, KVGetParams{Key: key}, &r); err != nil || !r.Found {
		return false, err
	}
	return true, json.Unmarshal(r.Value, out)
}

// KVPut stores a value.
func (c *Core) KVPut(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.conn.Call(ctx, CoreKVPut, KVPutParams{Key: key, Value: b}, nil)
}

// Log writes to the core's log.
// RecordRelease tells the core what reached an environment, and how long to watch it before
// the task is called finished.
func (c *Core) RecordRelease(ctx context.Context, p ReleaseRecordParams) (ReleaseRecordResult, error) {
	var out ReleaseRecordResult
	err := c.conn.Call(ctx, CoreReleaseRecord, p, &out)
	return out, err
}

// ReportIncident reports a fault in production. The same fingerprint reported again raises the
// count rather than opening a second incident.
func (c *Core) ReportIncident(ctx context.Context, p IncidentUpsertParams) (IncidentUpsertResult, error) {
	var out IncidentUpsertResult
	err := c.conn.Call(ctx, CoreIncidentUpsert, p, &out)
	return out, err
}

// CloseIncident says a fault has stopped: production is all right again. Closing one that is
// already closed changes nothing, because a monitoring tool may say so more than once.
func (c *Core) CloseIncident(ctx context.Context, p IncidentCloseParams) error {
	return c.conn.Call(ctx, CoreIncidentClose, p, nil)
}

func (c *Core) Log(ctx context.Context, level, message string, fields map[string]any) error {
	return c.conn.Call(ctx, CoreLog, LogParams{Level: level, Message: message, Fields: fields}, nil)
}
