package plugintest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildEcho(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "echo")
	if out, err := exec.Command("go", "build", "-o", bin, "../../internal/server/plugins/testdata/echo").CombinedOutput(); err != nil {
		t.Fatalf("build echo: %v\n%s", err, out)
	}
	return bin
}

func TestScenarioAgainstTheEchoPlugin(t *testing.T) {
	bin := buildEcho(t)
	s := Scenario{Config: map[string]any{"greeting": "hi"}, Secrets: map[string]string{"token": "tok-1"}}
	s.Project.Slug = "demo"
	raw := `{"tasks":[{"id":"t1","kind":"feature","title":"Search","phase":"planning"}],"steps":[
		{"hook":"webhook","body":{"id":"42","title":"Crash on login"},"expect_core":["task.find","task.create"]},
		{"hook":"webhook","body":{"id":"42","title":"Crash on login"},"expect_core":["task.find"]},
		{"hook":"phaseTransition","task":"t1","from":"planning","to":"implementation"},
		{"hook":"gateEvaluate","task":"t1"},
		{"hook":"renderUI","task":"t1"},
		{"hook":"jobPrepare","task":"t1"},
		{"hook":"jobFinish","task":"t1","expect_core":["artifact.put"]},
		{"hook":"webhook","body":{"check_task":"nope"},"expect_error":true},
		{"hook":"webhook","body":{"fail":true},"expect_error":true}]}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	rep, err := Run(context.Background(), []string{bin}, s, &logs, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		b, _ := json.MarshalIndent(rep.Steps, "", "  ")
		t.Fatalf("scenario failed:\n%s", b)
	}
	if rep.Manifest.Name != "echo" || len(rep.Tasks) != 2 {
		t.Fatalf("manifest %q, tasks %d", rep.Manifest.Name, len(rep.Tasks))
	}
	var last string
	if json.Unmarshal(rep.KV["last_transition"], &last); last != "planning->implementation" {
		t.Fatalf("kv: %s", rep.KV["last_transition"])
	}
	if c := rep.Checks["t1"]; len(c) != 1 || c[0].Name != "echo-ci" {
		t.Fatalf("checks: %+v", c)
	}
	if !strings.Contains(string(rep.Steps[4].Result), "hi token=tok-1") {
		t.Fatalf("renderUI should see the secret from its environment: %s", rep.Steps[4].Result)
	}
	if !strings.Contains(string(rep.Steps[5].Result), "Echo says: hi") || len(rep.Artifacts) != 1 {
		t.Fatalf("jobPrepare %s, artifacts %+v", rep.Steps[5].Result, rep.Artifacts)
	}
}

func TestUnmetExpectationsFailTheRun(t *testing.T) {
	bin := buildEcho(t)
	var s Scenario
	s.Config = map[string]any{"greeting": "hi"}
	_ = json.Unmarshal([]byte(`{"steps":[
		{"hook":"webhook","body":{"fail":true}},
		{"hook":"webhook","body":{"sleep_ms":1},"expect_core":["task.create"]},
		{"hook":"artifactApproved","task":"t1"},
		{"hook":"gateEvaluate"}]}`), &s)
	rep, err := Run(context.Background(), []string{bin}, s, os.Stderr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"echo failed on purpose", "expected a task.create call", `the manifest does not declare "artifactApproved"`, `needs "task"`}
	for i, w := range want {
		if !strings.Contains(rep.Steps[i].Problem, w) {
			t.Errorf("step %d: problem %q, want %q", i+1, rep.Steps[i].Problem, w)
		}
	}
	if !rep.Failed() {
		t.Fatal("the run must fail")
	}
}

func TestSignAndCoreRules(t *testing.T) {
	if got := Sign("secret", []byte("body"), "sha256="); got != "sha256=dc46983557fea127b43af721467eb9b3fde2338fe3e14f51952aa8478c13d355" {
		t.Fatalf("sign: %s", got)
	}
	c := NewCore("")
	h := c.Handler()
	ctx := context.Background()
	if _, err := h(ctx, "task.create", json.RawMessage(`{"kind":"epic","title":"x"}`)); err == nil {
		t.Fatal("unknown kind")
	}
	if _, err := h(ctx, "gate.setCheck", json.RawMessage(`{"task_id":"missing","name":"ci","status":"pass"}`)); err == nil {
		t.Fatal("a missing task is not found")
	}
	if _, err := h(ctx, "release.record", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("release.record without a version: %v", err)
	}
	// The same fault twice is one incident with one task, which is the rule a monitoring
	// plugin is written against.
	for range 2 {
		if _, err := h(ctx, "incident.upsert", json.RawMessage(`{"fingerprint":"f1","title":"It broke"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if inc := c.Incidents()["f1"]; inc.Count != 2 || inc.TaskID == "" {
		t.Fatalf("incident = %+v", inc)
	}
	if _, err := h(ctx, "incident.close", json.RawMessage(`{"fingerprint":"f1"}`)); err != nil {
		t.Fatal(err)
	}
	if !c.Incidents()["f1"].Closed {
		t.Fatal("closing should be remembered")
	}
	if n := len(c.Calls()); n != 6 {
		t.Fatalf("every call is recorded, got %d", n)
	}
}
