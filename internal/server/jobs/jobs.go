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

// ReconcileChecksArgs re-asks plugins about gates stuck on a pending check.
type ReconcileChecksArgs struct{}

func (ReconcileChecksArgs) Kind() string { return "reconcile_checks" }

// CheckReconciler is the plugin host. The interface keeps this package off plugins.
type CheckReconciler interface {
	ReconcileChecks(ctx context.Context, before time.Time, max int32) (int, error)
}

// ReconcileChecksWorker refreshes gates whose plugin checks have stopped moving. A webhook is
// delivered at most once, so a dropped one leaves the gate showing a check that finished long
// ago. Asking again is the only way to learn otherwise.
type ReconcileChecksWorker struct {
	river.WorkerDefaults[ReconcileChecksArgs]
	Plugins CheckReconciler
	Stale   time.Duration
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ReconcileChecksWorker) Work(ctx context.Context, _ *river.Job[ReconcileChecksArgs]) error {
	n, err := w.Plugins.ReconcileChecks(ctx, time.Now().Add(-w.Stale), 50)
	if n > 0 {
		w.Log.Info("re-asked plugins about gates that stopped moving", "tasks", n)
	}
	return err
}

// SettleReleasesArgs closes the tasks of releases that were watched and stayed quiet.
type SettleReleasesArgs struct{}

func (SettleReleasesArgs) Kind() string { return "settle_releases" }

// ReleaseSettler is the plugin host; the interface keeps this package off plugins.
type ReleaseSettler interface {
	SettleReleases(ctx context.Context, now time.Time, max int32) (int, error)
}

// SettleReleasesWorker ends the observation window. Nothing reported during it is the answer
// the Monitoring phase waits for, and nobody should have to sit and watch a clock for it.
type SettleReleasesWorker struct {
	river.WorkerDefaults[SettleReleasesArgs]
	Releases ReleaseSettler
	Log      *slog.Logger
}

// Work implements river.Worker.
func (w *SettleReleasesWorker) Work(ctx context.Context, _ *river.Job[SettleReleasesArgs]) error {
	n, err := w.Releases.SettleReleases(ctx, time.Now(), 50)
	if n > 0 {
		w.Log.Info("observation windows ended quietly", "releases", n)
	}
	return err
}

// ScoreQualityArgs runs every project's scorecard.
type ScoreQualityArgs struct{}

func (ScoreQualityArgs) Kind() string { return "score_quality" }

// QualityScorer is the quality service; the interface keeps this package off it.
type QualityScorer interface {
	SweepAll(ctx context.Context) (int, error)
}

// ScoreQualityWorker asks each project's rules on a schedule. A scorecard is a question with a
// date on it: nobody checks whether every component still has an owner by hand.
type ScoreQualityWorker struct {
	river.WorkerDefaults[ScoreQualityArgs]
	Quality QualityScorer
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ScoreQualityWorker) Work(ctx context.Context, _ *river.Job[ScoreQualityArgs]) error {
	n, err := w.Quality.SweepAll(ctx)
	if n > 0 {
		w.Log.Info("scored projects", "projects", n)
	}
	return err
}

// Timeout gives a sweep long enough to ask slow plugins about every component.
func (w *ScoreQualityWorker) Timeout(*river.Job[ScoreQualityArgs]) time.Duration {
	return 10 * time.Minute
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
	// Plugins refreshes gates whose checks stopped moving. Optional: without a plugin host
	// there are no plugin checks to go stale.
	Plugins CheckReconciler
	// Releases ends observation windows. The plugin host implements both.
	Releases ReleaseSettler
	// Quality scores projects against their scorecards. Optional.
	Quality QualityScorer
	// Intervals; zero picks the defaults (1m sweep, 1h backup, 5m reconcile of checks that
	// have not moved for 15m).
	ExpireEvery    time.Duration
	BackupEvery    time.Duration
	ReconcileEvery time.Duration
	StaleAfter     time.Duration
	SettleEvery    time.Duration
	QualityEvery   time.Duration
}

// New builds a River client with workers and periodic jobs registered. Call Start.
func New(o Options) (*river.Client[pgx.Tx], error) {
	if o.ExpireEvery == 0 {
		o.ExpireEvery = time.Minute
	}
	if o.BackupEvery == 0 {
		o.BackupEvery = time.Hour
	}
	if o.ReconcileEvery == 0 {
		o.ReconcileEvery = 5 * time.Minute
	}
	if o.SettleEvery == 0 {
		o.SettleEvery = time.Minute
	}
	if o.QualityEvery == 0 {
		// Daily: these rules change over weeks, and asking every plugin about every component
		// more often would cost more than it tells anyone.
		o.QualityEvery = 24 * time.Hour
	}
	if o.StaleAfter == 0 {
		// Long enough that a check still running is not asked about on every tick, short
		// enough that a person waiting on a gate is not waiting on a lie for an afternoon.
		o.StaleAfter = 15 * time.Minute
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ExpirePhasesWorker{Process: o.Process, Log: o.Log})
	periodic := []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(o.ExpireEvery), func() (river.JobArgs, *river.InsertOpts) {
			return ExpirePhasesArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: activeStates}}
		}, &river.PeriodicJobOpts{RunOnStart: true}),
	}
	if o.Plugins != nil {
		river.AddWorker(workers, &ReconcileChecksWorker{Plugins: o.Plugins, Stale: o.StaleAfter, Log: o.Log})
		periodic = append(periodic, river.NewPeriodicJob(river.PeriodicInterval(o.ReconcileEvery), func() (river.JobArgs, *river.InsertOpts) {
			return ReconcileChecksArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: activeStates}}
		}, &river.PeriodicJobOpts{RunOnStart: false}))
	}
	if o.Releases != nil {
		river.AddWorker(workers, &SettleReleasesWorker{Releases: o.Releases, Log: o.Log})
		periodic = append(periodic, river.NewPeriodicJob(river.PeriodicInterval(o.SettleEvery), func() (river.JobArgs, *river.InsertOpts) {
			return SettleReleasesArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: activeStates}}
		}, &river.PeriodicJobOpts{RunOnStart: false}))
	}
	if o.Quality != nil {
		river.AddWorker(workers, &ScoreQualityWorker{Quality: o.Quality, Log: o.Log})
		periodic = append(periodic, river.NewPeriodicJob(river.PeriodicInterval(o.QualityEvery), func() (river.JobArgs, *river.InsertOpts) {
			return ScoreQualityArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: activeStates}}
		}, &river.PeriodicJobOpts{RunOnStart: false}))
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
