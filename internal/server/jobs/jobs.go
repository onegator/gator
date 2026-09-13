// Package jobs runs background work on River: periodic phase-timeout sweeps, backups and
// later plugin schedules, observation windows and event compaction. Everything here is
// idempotent; a missed tick is caught by the next one.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/onegator/gator/internal/server/backup"
	"github.com/onegator/gator/internal/server/process"
)

// ExpirePhasesArgs is the periodic phase-timeout sweep.
type ExpirePhasesArgs struct{}

func (ExpirePhasesArgs) Kind() string { return "expire_phases" }

// ExpirePhasesWorker blocks tasks whose phase timeout elapsed.
type ExpirePhasesWorker struct {
	river.WorkerDefaults[ExpirePhasesArgs]
	Process *process.Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ExpirePhasesWorker) Work(ctx context.Context, _ *river.Job[ExpirePhasesArgs]) error {
	blocked, err := w.Process.ExpirePhases(ctx, time.Now())
	if len(blocked) > 0 {
		w.Log.Info("phase timeouts", "blocked", len(blocked), "tasks", blocked)
	}
	return err
}

// BackupArgs is the periodic database backup.
type BackupArgs struct{}

func (BackupArgs) Kind() string { return "backup" }

// BackupWorker dumps and uploads the database.
type BackupWorker struct {
	river.WorkerDefaults[BackupArgs]
	Config      backup.Config
	DatabaseURL string
	Log         *slog.Logger
}

// Work implements river.Worker.
func (w *BackupWorker) Work(ctx context.Context, _ *river.Job[BackupArgs]) error {
	return backup.Run(ctx, w.Config, w.DatabaseURL, w.Log)
}

// Timeout gives a backup up to 30 minutes.
func (w *BackupWorker) Timeout(*river.Job[BackupArgs]) time.Duration { return 30 * time.Minute }

// activeStates makes a periodic insert a no-op while a previous run is still pending.
var activeStates = []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}

// Options wires the client.
type Options struct {
	Pool        *pgxpool.Pool
	Process     *process.Service
	Backup      backup.Config
	DatabaseURL string
	Log         *slog.Logger
	// Intervals; zero picks the defaults (1m sweep, 1h backup).
	ExpireEvery time.Duration
	BackupEvery time.Duration
}

// New builds a River client with workers and periodic jobs registered. Call Start.
func New(o Options) (*river.Client[pgx.Tx], error) {
	if o.ExpireEvery == 0 {
		o.ExpireEvery = time.Minute
	}
	if o.BackupEvery == 0 {
		o.BackupEvery = time.Hour
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ExpirePhasesWorker{Process: o.Process, Log: o.Log})
	periodic := []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(o.ExpireEvery), func() (river.JobArgs, *river.InsertOpts) {
			return ExpirePhasesArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: activeStates}}
		}, &river.PeriodicJobOpts{RunOnStart: true}),
	}
	if o.Backup.Enabled() {
		river.AddWorker(workers, &BackupWorker{Config: o.Backup, DatabaseURL: o.DatabaseURL, Log: o.Log})
		periodic = append(periodic, river.NewPeriodicJob(river.PeriodicInterval(o.BackupEvery), func() (river.JobArgs, *river.InsertOpts) {
			return BackupArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: activeStates}}
		}, &river.PeriodicJobOpts{RunOnStart: false}))
	}
	return river.NewClient(riverpgxv5.New(o.Pool), &river.Config{
		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers:      workers,
		PeriodicJobs: periodic,
		Logger:       o.Log,
	})
}
