package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/onegator/gator/internal/server/runners"
)

// Fly starts each cloud runner as a Fly Machine through the Machines API. A machine is created
// with auto_destroy and no restart, so when its runner exits after the job the machine is gone
// and stops costing anything; Stop is the server making sure.
type Fly struct {
	Token  string // FLY_API_TOKEN
	App    string // FLY_APP: the app the machines belong to, created once by the operator
	Image  string // e.g. registry.fly.io/<app>:<tag>, built from Dockerfile.runner
	Region string // optional; Fly picks one when empty
	// CPUs and MemoryMB size the machine. Zero takes 2 shared CPUs and 2 GB: an agent, git
	// and a test suite in one box.
	CPUs     int
	MemoryMB int
	BaseURL  string // default https://api.machines.dev
	Client   *http.Client
}

var _ runners.Provisioner = (*Fly)(nil)

// Name implements runners.Provisioner.
func (f *Fly) Name() string { return "fly" }

func (f *Fly) base() string {
	if f.BaseURL == "" {
		return "https://api.machines.dev"
	}
	return strings.TrimRight(f.BaseURL, "/")
}

func (f *Fly) client() *http.Client {
	if f.Client == nil {
		return &http.Client{Timeout: 60 * time.Second}
	}
	return f.Client
}

type flyGuest struct {
	CPUKind  string `json:"cpu_kind"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
}

type flyRestart struct {
	Policy string `json:"policy"`
}

type flyConfig struct {
	Image       string            `json:"image"`
	Env         map[string]string `json:"env,omitempty"`
	AutoDestroy bool              `json:"auto_destroy"`
	Restart     flyRestart        `json:"restart"`
	Guest       flyGuest          `json:"guest"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type flyCreate struct {
	Name   string    `json:"name"`
	Region string    `json:"region,omitempty"`
	Config flyConfig `json:"config"`
}

// Start implements runners.Provisioner.
func (f *Fly) Start(ctx context.Context, spec runners.MachineSpec) (string, error) {
	if f.Token == "" || f.App == "" || f.Image == "" {
		return "", fmt.Errorf("fly: FLY_API_TOKEN, FLY_APP and the runner image are all required")
	}
	cpus, mem := f.CPUs, f.MemoryMB
	if cpus == 0 {
		cpus = 2
	}
	if mem == 0 {
		mem = 2048
	}
	body := flyCreate{Name: spec.Name, Region: f.Region, Config: flyConfig{
		Image: f.Image, Env: spec.Env, AutoDestroy: true, Restart: flyRestart{Policy: "no"},
		Guest:    flyGuest{CPUKind: "shared", CPUs: cpus, MemoryMB: mem},
		Metadata: map[string]string{"gator_job": spec.JobID, "gator_project": spec.ProjectID},
	}}
	var out struct {
		ID string `json:"id"`
	}
	if err := f.do(ctx, http.MethodPost, "/machines", body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("fly: the machine was created without an id")
	}
	return out.ID, nil
}

// Stop implements runners.Provisioner. A machine that already destroyed itself answers 404,
// which is the outcome Stop wanted.
func (f *Fly) Stop(ctx context.Context, handle string) error {
	err := f.do(ctx, http.MethodDelete, "/machines/"+url.PathEscape(handle)+"?force=true", nil, nil)
	if se, ok := err.(*flyError); ok && se.status == http.StatusNotFound {
		return nil
	}
	return err
}

type flyError struct {
	status int
	body   string
}

func (e *flyError) Error() string { return fmt.Sprintf("fly: %d: %s", e.status, e.body) }

func (f *Fly) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base()+"/v1/apps/"+url.PathEscape(f.App)+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("fly: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &flyError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
