package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The skeleton compiles, and its own scenario passes against it.
func TestNewSkeletonBuildsAndPassesItsScenario(t *testing.T) {
	dir, err := os.MkdirTemp("testdata", "skeleton-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var out, errb bytes.Buffer
	if code, err := run(context.Background(), []string{"new", "acme-tracker", dir}, &out, &errb); code != 0 || err != nil {
		t.Fatalf("new: %d %v", code, err)
	}
	if code, _ := run(context.Background(), []string{"new", "acme-tracker", dir}, &out, &errb); code == 0 {
		t.Fatal("new must not overwrite")
	}
	bin := filepath.Join(t.TempDir(), "plugin")
	if b, err := exec.Command("go", "build", "-o", bin, "./"+dir).CombinedOutput(); err != nil {
		t.Fatalf("skeleton does not build: %v\n%s", err, b)
	}
	out.Reset()
	code, err := run(context.Background(), []string{"dev", filepath.Join(dir, "scenarios", "basic.json"), "--", bin}, &out, &errb)
	if code != 0 || err != nil || !strings.Contains(out.String(), "PASSED") {
		t.Fatalf("dev: %d %v\n%s\n%s", code, err, out.String(), errb.String())
	}
	for _, want := range []string{"plugin acme-tracker 0.1.0", "→ task.create", "bad signature", "log info: Search: planning -> implementation"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code, err := run(context.Background(), []string{"manifest", "--", bin}, &out, &errb); code != 0 || err != nil || !strings.Contains(out.String(), `"name": "acme-tracker"`) {
		t.Fatalf("manifest: %d %v %s", code, err, out.String())
	}
}

func TestBadInvocations(t *testing.T) {
	var out, errb bytes.Buffer
	for _, args := range [][]string{{}, {"dev"}, {"dev", "x.json"}, {"new"}, {"frobnicate"}, {"new", "Bad_Name"}} {
		if code, _ := run(context.Background(), args, &out, &errb); code == 0 {
			t.Errorf("%v should fail", args)
		}
	}
}
