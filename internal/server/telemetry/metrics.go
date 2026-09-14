package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics are the domain instruments shared across the server. Names are stable and
// documented; dashboards depend on them.
type Metrics struct {
	PhaseTransitions metric.Int64Counter       // gator.task.transitions{kind,to}
	TimeInPhase      metric.Float64Histogram   // gator.task.phase_seconds{kind,phase}
	GateBlocked      metric.Int64Counter       // gator.gate.blocked{phase}
	JobsByStatus     metric.Int64UpDownCounter // gator.jobs{status}
	RunnersOnline    metric.Int64UpDownCounter // gator.runners.online
	PluginCalls      metric.Int64Counter       // gator.plugin.calls{plugin,hook,ok}
	PluginLatency    metric.Float64Histogram   // gator.plugin.call_seconds{plugin,hook}
	TokenCost        metric.Float64Counter     // gator.agent.cost_usd{project}
	AgentTokens      metric.Int64Counter       // gator.agent.tokens{type,backend}
	HTTPRateLimited  metric.Int64Counter       // gator.http.rate_limited{scope}
}

var (
	metricsOnce sync.Once
	metricsInst *Metrics
	metricsErr  error
)

// Instruments returns the process-wide instruments, creating them on first use.
func Instruments() (*Metrics, error) {
	metricsOnce.Do(func() {
		m := Meter()
		var mm Metrics
		var errs []error
		add := func(err error) {
			if err != nil {
				errs = append(errs, err)
			}
		}
		var err error
		mm.PhaseTransitions, err = m.Int64Counter("gator.task.transitions", metric.WithDescription("phase transitions by kind and target"))
		add(err)
		mm.TimeInPhase, err = m.Float64Histogram("gator.task.phase_seconds", metric.WithDescription("seconds a task spent in a phase"), metric.WithUnit("s"))
		add(err)
		mm.GateBlocked, err = m.Int64Counter("gator.gate.blocked", metric.WithDescription("gates blocked by automation"))
		add(err)
		mm.JobsByStatus, err = m.Int64UpDownCounter("gator.jobs", metric.WithDescription("jobs by status"))
		add(err)
		mm.RunnersOnline, err = m.Int64UpDownCounter("gator.runners.online", metric.WithDescription("connected runners"))
		add(err)
		mm.PluginCalls, err = m.Int64Counter("gator.plugin.calls", metric.WithDescription("plugin hook invocations"))
		add(err)
		mm.PluginLatency, err = m.Float64Histogram("gator.plugin.call_seconds", metric.WithDescription("plugin hook latency"), metric.WithUnit("s"))
		add(err)
		mm.TokenCost, err = m.Float64Counter("gator.agent.cost_usd", metric.WithDescription("estimated agent cost"))
		add(err)
		mm.AgentTokens, err = m.Int64Counter("gator.agent.tokens", metric.WithDescription("tokens consumed by agents"))
		add(err)
		mm.HTTPRateLimited, err = m.Int64Counter("gator.http.rate_limited", metric.WithDescription("requests rejected by rate limiting"))
		add(err)
		if len(errs) > 0 {
			metricsErr = errs[0]
			return
		}
		metricsInst = &mm
	})
	return metricsInst, metricsErr
}

// Attr is a shorthand for string attributes.
func Attr(k, v string) attribute.KeyValue { return attribute.String(k, v) }

// Ctx is re-exported so callers do not import context just for signatures.
type Ctx = context.Context
