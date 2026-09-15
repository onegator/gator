-- name: InsertUsage :execrows
INSERT INTO usage_records (
    task_id, project_id, phase, job_id, source, actor_id, backend, model,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
    cost_usd, cost_estimated, duration_ms, started_at, finished_at, idempotency_key
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18
)
ON CONFLICT (task_id, idempotency_key) DO NOTHING;

-- name: ListUsageByTask :many
SELECT * FROM usage_records WHERE task_id = $1 ORDER BY created_at, id;

-- name: ProjectMetricsByKind :many
SELECT
    t.kind,
    count(*)::bigint AS tasks,
    count(t.closed_at)::bigint AS closed_tasks,
    COALESCE(avg(EXTRACT(EPOCH FROM (t.closed_at - t.created_at))) FILTER (WHERE t.closed_at IS NOT NULL), 0)::double precision AS avg_lead_seconds,
    COALESCE(sum(u.input_tokens), 0)::bigint AS input_tokens,
    COALESCE(sum(u.output_tokens), 0)::bigint AS output_tokens,
    COALESCE(sum(u.cache_read_tokens), 0)::bigint AS cache_read_tokens,
    COALESCE(sum(u.cache_write_tokens), 0)::bigint AS cache_write_tokens,
    COALESCE(sum(u.cost_usd), 0)::double precision AS cost_usd,
    COALESCE(sum(u.duration_ms), 0)::bigint AS agent_ms,
    COALESCE(sum(u.jobs), 0)::bigint AS jobs
FROM tasks t
LEFT JOIN (
    SELECT task_id,
           sum(input_tokens) AS input_tokens,
           sum(output_tokens) AS output_tokens,
           sum(cache_read_tokens) AS cache_read_tokens,
           sum(cache_write_tokens) AS cache_write_tokens,
           sum(cost_usd) AS cost_usd,
           sum(duration_ms) AS duration_ms,
           count(DISTINCT job_id) AS jobs
    FROM usage_records
    GROUP BY task_id
) u ON u.task_id = t.id
WHERE t.project_id = sqlc.arg(project_id) AND t.created_at >= sqlc.arg(since)
GROUP BY t.kind
ORDER BY t.kind;

-- name: ProjectCostSince :one
-- What a project spent on agents since a moment; the daily budget compares against it.
SELECT COALESCE(sum(cost_usd), 0)::double precision AS cost_usd
FROM usage_records WHERE project_id = sqlc.arg(project_id) AND created_at >= sqlc.arg(since);
