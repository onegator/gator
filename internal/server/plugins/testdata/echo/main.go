// Command echo is the test plugin: it exercises every hook and core method, and can be told
// to be slow, crash or fail so the host's defences can be tested.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/onegator/gator/plugin"
)

func main() {
	err := plugin.Serve(plugin.Handlers{
		Manifest: plugin.Manifest{
			Name: "echo", Version: "1.0.0", Capabilities: []string{"ci", "deploy"}, UI: []string{"tab", "chip"},
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{
				"greeting":{"type":"string"},
				"object":{"type":"string"},
				"object_turns":{"type":"string"},
				"token":{"type":"string","x-secret":true}},"required":["greeting"]}`),
			Webhook: &plugin.WebhookSpec{DeliveryHeader: "X-Echo-Delivery"},
		},
		Webhook: func(ctx context.Context, core *plugin.Core, p plugin.WebhookParams) error {
			var b struct {
				ID, Title string
				Crash     bool
				Fail      bool
				Refuse    bool
				SleepMS   int    `json:"sleep_ms"`
				CheckTask string `json:"check_task"`
				Release   *struct {
					Version        string `json:"version"`
					TaskID         string `json:"task_id"`
					ObserveMinutes int    `json:"observe_minutes"`
				} `json:"release"`
				Incident *struct {
					Fingerprint string `json:"fingerprint"`
					Title       string `json:"title"`
					Severity    string `json:"severity"`
					Version     string `json:"version"`
				} `json:"incident"`
				Resolve string `json:"resolve"` // fingerprint of a fault that has stopped
			}
			_ = json.Unmarshal(p.Body, &b)
			switch {
			case b.Crash:
				os.Exit(3)
			case b.SleepMS > 0:
				time.Sleep(time.Duration(b.SleepMS) * time.Millisecond)
				return nil
			case b.Fail:
				return errors.New("echo failed on purpose")
			case b.Refuse:
				return plugin.Errorf(plugin.CodeForbidden, "bad signature")
			case b.CheckTask != "":
				return core.SetCheck(ctx, b.CheckTask, plugin.Check{Name: "echo-hook", Status: "pass"})
			case b.Release != nil:
				_, err := core.RecordRelease(ctx, plugin.ReleaseRecordParams{
					Version: b.Release.Version, TaskID: b.Release.TaskID, ObserveMinutes: b.Release.ObserveMinutes})
				return err
			case b.Resolve != "":
				return core.CloseIncident(ctx, plugin.IncidentCloseParams{Fingerprint: b.Resolve})
			case b.Incident != nil:
				_, err := core.ReportIncident(ctx, plugin.IncidentUpsertParams{
					Fingerprint: b.Incident.Fingerprint, Title: b.Incident.Title,
					Severity: b.Incident.Severity, ReleaseVersion: b.Incident.Version})
				return err
			}
			if t, err := core.FindTask(ctx, "echo_id", b.ID); err != nil || t != nil {
				return err
			}
			_, err := core.CreateTask(ctx, plugin.TaskCreateParams{Kind: "bug", Title: b.Title, ExternalRefs: map[string]string{"echo_id": b.ID}})
			return err
		},
		Release: func(ctx context.Context, core *plugin.Core, p plugin.ReleaseParams) error {
			return core.KVPut(ctx, "asked_to_release", p.Task.ID)
		},
		IncidentClosed: func(ctx context.Context, core *plugin.Core, p plugin.IncidentClosedParams) error {
			return core.KVPut(ctx, "resolved_upstream", p.Fingerprint)
		},
		PhaseTransition: func(ctx context.Context, core *plugin.Core, p plugin.PhaseTransitionParams) error {
			return core.KVPut(ctx, "last_transition", p.From+"->"+p.To)
		},
		GateEvaluate: func(ctx context.Context, core *plugin.Core, p plugin.GateEvaluateParams) (plugin.GateEvaluateResult, error) {
			return plugin.GateEvaluateResult{Checks: []plugin.Check{{Name: "echo-ci", Status: "pass", Detail: "phase " + p.Phase}}}, nil
		},
		RenderUI: func(ctx context.Context, core *plugin.Core, p plugin.RenderUIParams) (plugin.RenderUIResult, error) {
			return plugin.RenderUIResult{
				Tabs:  []plugin.Tab{{Title: "Echo", Markdown: core.Setting("greeting") + " token=" + core.Secret("token")}},
				Chips: []plugin.Chip{{Text: "echo"}},
			}, nil
		},
		JobPrepare: func(ctx context.Context, core *plugin.Core, p plugin.JobPrepareParams) (plugin.JobPrepareResult, error) {
			return plugin.JobPrepareResult{Instructions: "Echo says: " + core.Setting("greeting")}, nil
		},
		TurnEnding: func(ctx context.Context, core *plugin.Core, p plugin.TurnEndingParams) (plugin.TurnEndingResult, error) {
			// Objects for the first "object_turns" endings (default 1), so a test can watch a
			// turn be sent back to work and can also outlast the core's limit.
			turns := 1
			if n, err := strconv.Atoi(core.Setting("object_turns")); err == nil && n > 0 {
				turns = n
			}
			if p.Turn <= turns && core.Setting("object") != "" {
				return plugin.TurnEndingResult{Objection: core.Setting("object")}, nil
			}
			return plugin.TurnEndingResult{}, nil
		},
		JobFinish: func(ctx context.Context, core *plugin.Core, p plugin.JobFinishParams) error {
			_, err := core.PutArtifact(ctx, plugin.ArtifactPutParams{TaskID: p.Task.ID, Type: "echo-note", Content: "job " + p.Job.ID + " " + p.Job.Status})
			return err
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
