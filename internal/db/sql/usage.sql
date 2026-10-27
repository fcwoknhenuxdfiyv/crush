-- name: CreateUsageEvent :exec
INSERT INTO usage_events (
    session_id,
    provider,
    model,
    source,
    input_tokens,
    output_tokens,
    cache_read_tokens,
    cache_creation_tokens,
    cost,
    estimated,
    created_at
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?
);

-- name: ListUsageEventsSince :many
SELECT
    session_id,
    created_at,
    input_tokens,
    output_tokens,
    cache_read_tokens,
    cache_creation_tokens,
    cost,
    estimated
FROM usage_events
WHERE created_at >= ?
ORDER BY created_at ASC;

-- name: ListRootSessionCosts :many
SELECT
    id,
    created_at,
    cost
FROM sessions
WHERE parent_session_id IS NULL
  AND created_at >= ?;

-- name: ListRootSessionLedgerCost :many
SELECT
    s.id,
    s.created_at,
    s.cost,
    CAST(COALESCE((
        SELECT SUM(e.cost)
        FROM usage_events e
        WHERE e.session_id = s.id
           OR e.session_id IN (
               SELECT c.id FROM sessions c WHERE c.parent_session_id = s.id
           )
    ), 0) AS REAL) AS ledger_cost
FROM sessions s
WHERE s.parent_session_id IS NULL
  AND s.created_at >= ?;
