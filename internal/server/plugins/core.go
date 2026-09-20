package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

const maxKVBytes = 64 << 10

// handleCore serves the plugin's calls into the core, always scoped to its own project.
func (i *instance) handleCore(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	start := time.Now()
	res, err := i.core(ctx, method, raw)
	if method != plugin.CoreLog {
		var rb json.RawMessage
		if err == nil {
			rb, _ = json.Marshal(res)
		}
		i.audit("plugin_to_core", method, raw, rb, err, time.Since(start))
	}
	return res, err
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, plugin.Errorf(plugin.CodeInvalidParams, "%v", err)
	}
	return v, nil
}

func (i *instance) core(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	q := db.New(i.h.pool)
	switch method {
	case plugin.CoreTaskCreate:
		p, err := decode[plugin.TaskCreateParams](raw)
		if err != nil {
			return nil, err
		}
		if p.Kind == "" || strings.TrimSpace(p.Title) == "" {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "kind and title are required")
		}
		urgency := int16(p.Urgency)
		if urgency < 1 || urgency > 4 {
			urgency = 3
		}
		t, err := i.h.proc.Create(ctx, process.CreateParams{ProjectID: i.b.projectID, Kind: p.Kind, Title: p.Title,
			Description: p.Description, Urgency: urgency}, process.Actor{Kind: process.ActorPlugin})
		if err != nil {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "%v", err)
		}
		if len(p.ExternalRefs) > 0 {
			refs, _ := json.Marshal(p.ExternalRefs)
			if t, err = q.MergeTaskExternalRefs(ctx, db.MergeTaskExternalRefsParams{ID: t.ID, Refs: refs}); err != nil {
				return nil, err
			}
		}
		return taskRef(t), nil
	case plugin.CoreTaskFind:
		p, err := decode[plugin.TaskFindParams](raw)
		if err != nil {
			return nil, err
		}
		t, err := q.FindTaskByExternalRef(ctx, db.FindTaskByExternalRefParams{ProjectID: i.b.projectID, Key: p.Key, Value: p.Value})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return taskRef(t), nil
	case plugin.CoreTaskUpdate:
		p, err := decode[plugin.TaskUpdateParams](raw)
		if err != nil {
			return nil, err
		}
		t, err := i.task(ctx, q, p.TaskID)
		if err != nil {
			return nil, err
		}
		refs, _ := json.Marshal(p.ExternalRefs)
		if t, err = q.MergeTaskExternalRefs(ctx, db.MergeTaskExternalRefsParams{ID: t.ID, Refs: refs}); err != nil {
			return nil, err
		}
		return taskRef(t), nil
	case plugin.CoreGateSetCheck:
		p, err := decode[plugin.SetCheckParams](raw)
		if err != nil {
			return nil, err
		}
		t, err := i.task(ctx, q, p.TaskID)
		if err != nil {
			return nil, err
		}
		if err := validCheck(p.Check); err != nil {
			return nil, err
		}
		return nil, i.h.proc.SetCheck(ctx, t.ID, process.Check{Name: p.Name, Source: "plugin:" + i.b.name, Status: p.Status, Detail: p.Detail})
	case plugin.CoreArtifactPut:
		p, err := decode[plugin.ArtifactPutParams](raw)
		if err != nil {
			return nil, err
		}
		t, err := i.task(ctx, q, p.TaskID)
		if err != nil {
			return nil, err
		}
		if p.Type == "" || (p.Content == "" && p.URL == "") {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "type and content or url are required")
		}
		if p.Phase == "" {
			p.Phase = t.Phase
		}
		params := db.CreateArtifactParams{TaskID: t.ID, Phase: p.Phase, Type: p.Type}
		if p.Content != "" {
			params.Content = &p.Content
		}
		if p.URL != "" {
			params.Url = &p.URL
		}
		a, err := q.CreateArtifact(ctx, params)
		if err != nil {
			return nil, err
		}
		_ = emit(ctx, q, "artifact.created", "task", t.ID, map[string]any{"phase": p.Phase, "type": p.Type, "version": a.Version, "plugin": i.b.name})
		return plugin.ArtifactPutResult{Version: int(a.Version)}, nil
	case plugin.CoreKVGet:
		p, err := decode[plugin.KVGetParams](raw)
		if err != nil {
			return nil, err
		}
		v, err := q.PluginKVGet(ctx, db.PluginKVGetParams{ProjectPluginID: i.b.id, Key: p.Key})
		if errors.Is(err, pgx.ErrNoRows) {
			return plugin.KVGetResult{}, nil
		}
		if err != nil {
			return nil, err
		}
		return plugin.KVGetResult{Found: true, Value: v}, nil
	case plugin.CoreKVPut:
		p, err := decode[plugin.KVPutParams](raw)
		if err != nil {
			return nil, err
		}
		if p.Key == "" || len(p.Key) > 200 || len(p.Value) > maxKVBytes || !json.Valid(p.Value) {
			return nil, plugin.Errorf(plugin.CodeInvalidParams, "key must be 1-200 bytes and value valid JSON up to %d bytes", maxKVBytes)
		}
		return nil, q.PluginKVPut(ctx, db.PluginKVPutParams{ProjectPluginID: i.b.id, Key: p.Key, Value: p.Value})
	case plugin.CoreLog:
		p, err := decode[plugin.LogParams](raw)
		if err != nil {
			return nil, err
		}
		lvl := map[string]slog.Level{"debug": slog.LevelDebug, "warn": slog.LevelWarn, "error": slog.LevelError}[p.Level]
		attrs := []any{}
		for k, v := range p.Fields {
			attrs = append(attrs, k, v)
		}
		i.log().Log(ctx, lvl, p.Message, attrs...)
		return nil, nil
	case plugin.CoreReleaseRecord:
		p, err := decode[plugin.ReleaseRecordParams](raw)
		if err != nil {
			return nil, err
		}
		return i.recordRelease(ctx, q, p)
	case plugin.CoreIncidentUpsert:
		p, err := decode[plugin.IncidentUpsertParams](raw)
		if err != nil {
			return nil, err
		}
		return i.upsertIncident(ctx, q, p)
	}
	return nil, plugin.Errorf(plugin.CodeMethodNotFound, "method %q not found", method)
}

// task loads a task of the plugin's own project; others look like they do not exist.
func (i *instance) task(ctx context.Context, q *db.Queries, id string) (db.Task, error) {
	u, ok := parseUUID(id)
	if !ok {
		return db.Task{}, plugin.Errorf(plugin.CodeInvalidParams, "task_id %q is not a uuid", id)
	}
	t, err := q.GetTask(ctx, u)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && t.ProjectID != i.b.projectID) {
		return db.Task{}, plugin.Errorf(plugin.CodeNotFound, "task %s not found in this project", id)
	}
	return t, err
}

func validCheck(c plugin.Check) error {
	if c.Name == "" {
		return plugin.Errorf(plugin.CodeInvalidParams, "check name is required")
	}
	switch c.Status {
	case "pass", "fail", "pending":
		return nil
	}
	return plugin.Errorf(plugin.CodeInvalidParams, "check status must be pass, fail or pending, not %q", c.Status)
}

func taskRef(t db.Task) plugin.TaskRef {
	refs := map[string]string{}
	var raw map[string]any
	if json.Unmarshal(t.ExternalRefs, &raw) == nil {
		for k, v := range raw {
			refs[k] = fmt.Sprint(v)
		}
	}
	return plugin.TaskRef{ID: uuidString(t.ID), ProjectID: uuidString(t.ProjectID), Kind: t.Kind, Title: t.Title, Phase: t.Phase, ExternalRefs: refs}
}
