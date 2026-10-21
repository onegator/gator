package runners

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
)

// Provisioner starts a runner in the cloud for one job and takes it down afterwards. The
// runner it starts runs `gator-runner run --once`: it takes the one job its token allows,
// sends the receipt and exits. Stop is the server making sure, not the plan.
type Provisioner interface {
	// Name says which provisioner this is, for logs and events: "docker", "fly".
	Name() string
	// Start launches a machine and returns a handle Stop understands.
	Start(ctx context.Context, spec MachineSpec) (handle string, err error)
	// Stop tears the machine down. A machine that is already gone is not an error.
	Stop(ctx context.Context, handle string) error
}

// MachineSpec is one cloud runner to start.
type MachineSpec struct {
	// Name is a short, unique, DNS-safe name for the machine: gator-job-<first 8 of the id>.
	Name      string
	JobID     string
	ProjectID string
	Backend   string
	// Env is the machine's whole environment: how its runner reaches the server, its one-job
	// token, and the backend credentials the operator chose to pass (CloudEnv). Secrets are in
	// it, so a provisioner must not put it on a command line or in a log.
	Env map[string]string
}

// RunnerTokenIssuer mints the token of a runner started for one job. auth.Tokens implements it.
type RunnerTokenIssuer interface {
	IssueRunnerForJob(ctx context.Context, jobID, projectID pgtype.UUID, name string, ttl time.Duration) (string, error)
}

// machine is a cloud runner this server started and has not yet seen the end of.
type machine struct {
	handle  string
	started time.Time
}

func (c *Config) cloudDefaults() {
	if c.ProvisionTimeout == 0 {
		c.ProvisionTimeout = 5 * time.Minute
	}
	if c.MaxProvisions == 0 {
		c.MaxProvisions = 3
	}
}

// Provision starts cloud runners for queued cloud jobs that no connected cloud runner can take,
// and stops the ones whose job is over. It is the whole lifecycle: called after a job is
// queued, after one finishes and on every sweep, so a lost call costs a sweep interval and no
// more. Without a provisioner it does nothing, and cloud jobs wait for a long-lived cloud runner.
func (m *Manager) Provision(ctx context.Context) {
	p := m.cfg.Provisioner
	if p == nil {
		return
	}
	m.cloudMu.Lock()
	defer m.cloudMu.Unlock()
	q := db.New(m.pool)

	// Tear down first: a machine whose job ended has nothing left to do.
	for key, mc := range m.machines {
		id := pgtype.UUID{Bytes: key, Valid: true}
		job, err := q.GetJob(ctx, id)
		if err == nil && !terminal(job.Status) {
			continue
		}
		m.stopMachine(ctx, id, mc, "job over")
		delete(m.provisions, key)
	}

	queued, err := q.ListQueuedCloudJobs(ctx, 50)
	if err != nil {
		m.log.Warn("cloud: listing queued jobs", "err", err)
		return
	}
	for _, job := range queued {
		if mc := m.machines[job.ID.Bytes]; mc != nil {
			if m.now().Sub(mc.started) < m.cfg.ProvisionTimeout {
				continue // on its way
			}
			// Started long ago and the job is still waiting: it never connected, or it lost
			// the job and the lease expired. Either way that machine is not doing this job.
			m.stopMachine(ctx, job.ID, mc, "never took the job")
		}
		if m.idleCloudRunner(ctx, q, job) {
			continue
		}
		if m.provisions[job.ID.Bytes] >= m.cfg.MaxProvisions {
			reason := fmt.Sprintf("no cloud runner took the job after %d tries with %s", m.provisions[job.ID.Bytes], p.Name())
			if err := m.StopJob(ctx, job.ID, reason); err != nil {
				m.log.Warn("cloud: giving up on a job", "job", uuidString(job.ID), "err", err)
			}
			delete(m.provisions, job.ID.Bytes)
			continue
		}
		m.startMachine(ctx, p, job)
	}
}

// startMachine mints the job's runner token and starts a machine with it.
func (m *Manager) startMachine(ctx context.Context, p Provisioner, job db.Job) {
	if m.cfg.RunnerTokens == nil || m.cfg.CloudServerURL == "" {
		m.log.Warn("cloud: a provisioner needs a token issuer and the URL machines reach the server at")
		return
	}
	id := uuidString(job.ID)
	name := "gator-job-" + strings.ReplaceAll(id, "-", "")[:12]
	var bounds proto.Bounds
	_ = json.Unmarshal(job.Bounds, &bounds)
	// Long enough for the job and its retries' waits, short enough to die on its own if the
	// finish never comes: the token is revoked with the job, and this is the backstop.
	ttl := time.Duration(bounds.TimeoutSeconds)*time.Second + time.Hour
	token, err := m.cfg.RunnerTokens.IssueRunnerForJob(ctx, job.ID, job.ProjectID, name, ttl)
	if err != nil {
		m.log.Warn("cloud: minting a runner token", "job", id, "err", err)
		return
	}
	env := maps.Clone(m.cfg.CloudEnv)
	if env == nil {
		env = map[string]string{}
	}
	// Set last, so a passed-through variable cannot redirect the runner or replace its token.
	env["GATOR_RUNNER_SERVER_URL"] = m.cfg.CloudServerURL
	env["GATOR_RUNNER_TOKEN"] = token
	env["GATOR_RUNNER_NAME"] = name
	env["GATOR_RUNNER_PLACEMENT"] = proto.PlacementCloud
	env["GATOR_RUNNER_LOCATION"] = "other"
	env["GATOR_RUNNER_BACKENDS"] = job.Backend
	env["GATOR_RUNNER_PROJECTS"] = uuidString(job.ProjectID)
	env["GATOR_RUNNER_ONCE"] = "1"
	m.provisions[job.ID.Bytes]++
	handle, err := p.Start(ctx, MachineSpec{Name: name, JobID: id, ProjectID: uuidString(job.ProjectID), Backend: job.Backend, Env: env})
	if err != nil {
		m.log.Warn("cloud: starting a runner", "job", id, "provisioner", p.Name(), "err", err)
		_ = m.tx(ctx, func(q *db.Queries) error {
			return emit(ctx, q, "job.provision_failed", "job", job.ID, map[string]any{
				"task_id": uuidString(job.TaskID), "provisioner": p.Name(), "error": err.Error()})
		})
		return
	}
	m.machines[job.ID.Bytes] = &machine{handle: handle, started: m.now()}
	m.log.Info("cloud: started a runner", "job", id, "provisioner", p.Name(), "machine", handle)
	_ = m.tx(ctx, func(q *db.Queries) error {
		return emit(ctx, q, "job.provisioned", "job", job.ID, map[string]any{
			"task_id": uuidString(job.TaskID), "provisioner": p.Name(), "machine": name})
	})
}

func (m *Manager) stopMachine(ctx context.Context, jobID pgtype.UUID, mc *machine, why string) {
	if err := m.cfg.Provisioner.Stop(ctx, mc.handle); err != nil {
		// Kept in the map, so the next pass tries again rather than leaving it to bill.
		m.log.Warn("cloud: stopping a runner", "job", uuidString(jobID), "machine", mc.handle, "err", err)
		return
	}
	delete(m.machines, jobID.Bytes)
	m.log.Info("cloud: stopped a runner", "job", uuidString(jobID), "machine", mc.handle, "why", why)
}

// idleCloudRunner reports whether a long-lived cloud runner is connected that can take the job
// now. Then nothing needs starting: the next offer hands it over.
func (m *Manager) idleCloudRunner(ctx context.Context, q *db.Queries, job db.Job) bool {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		if s.placementOrLocal() != proto.PlacementCloud || s.onlyJob.Valid {
			continue
		}
		if len(s.projects) > 0 && !slices.Contains(s.projects, job.ProjectID) {
			continue
		}
		if !slices.Contains(s.leasable(), job.Backend) {
			continue
		}
		if free, err := s.freeSlots(ctx, q, 0); err == nil && free > 0 {
			return true
		}
	}
	return false
}

// provisionSoon runs Provision shortly, off the caller's request. The pause lets connected
// runners lease the job first, so a machine is not started for work a runner just took.
func (m *Manager) provisionSoon() {
	if m.cfg.Provisioner == nil {
		return
	}
	time.AfterFunc(m.cfg.ProvisionDelay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		m.Provision(ctx)
	})
}
