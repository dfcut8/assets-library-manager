CREATE TABLE import_sources_v2 (
    id TEXT PRIMARY KEY CHECK(length(id) = 32),
    source_path TEXT NOT NULL CHECK(length(source_path) BETWEEN 1 AND 1024),
    source_type TEXT NOT NULL CHECK(source_type IN ('loose', 'zip')),
    discovery_fingerprint TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('discovered', 'processing', 'ready', 'duplicate', 'blocked', 'failed', 'retained', 'deleted')),
    deletion_state TEXT NOT NULL CHECK(deletion_state IN ('not-eligible', 'eligible', 'pending', 'deleted', 'failed')),
    retained_reason TEXT,
    error_code TEXT,
    error_message TEXT CHECK(error_message IS NULL OR length(error_message) <= 2048),
    discovered_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(source_path, discovery_fingerprint)
) STRICT;

CREATE TABLE import_items_v2 (
    id TEXT PRIMARY KEY CHECK(length(id) = 32),
    source_id TEXT NOT NULL REFERENCES import_sources_v2(id) ON DELETE CASCADE,
    zip_entry_name TEXT,
    staged_path TEXT,
    sha256 BLOB CHECK(sha256 IS NULL OR length(sha256) = 32),
    asset_id TEXT REFERENCES assets(id),
    state TEXT NOT NULL CHECK(state IN ('discovered', 'staged', 'analyzing', 'committing', 'ready', 'duplicate', 'blocked', 'failed')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
    error_code TEXT,
    error_message TEXT CHECK(error_message IS NULL OR length(error_message) <= 2048),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE ai_runs_v2 (
    id TEXT PRIMARY KEY CHECK(length(id) = 32),
    import_item_id TEXT NOT NULL REFERENCES import_items_v2(id) ON DELETE CASCADE,
    asset_id TEXT REFERENCES assets(id) ON DELETE SET NULL,
    provider TEXT NOT NULL CHECK(provider = 'openai'),
    model TEXT NOT NULL,
    reasoning_effort TEXT NOT NULL,
    image_detail TEXT NOT NULL CHECK(image_detail IN ('low', 'high', 'auto')),
    prompt_version TEXT NOT NULL,
    schema_version TEXT NOT NULL,
    attempt_number INTEGER NOT NULL CHECK(attempt_number >= 1),
    started_at TEXT NOT NULL,
    completed_at TEXT,
    latency_ms INTEGER CHECK(latency_ms IS NULL OR latency_ms >= 0),
    request_id TEXT,
    usage_json TEXT,
    outcome TEXT NOT NULL CHECK(outcome IN ('pending', 'accepted', 'retryable-error', 'permanent-error', 'refused', 'invalid-response', 'canceled')),
    error_code TEXT,
    error_message TEXT CHECK(error_message IS NULL OR length(error_message) <= 2048),
    normalized_result_json TEXT
) STRICT;

INSERT INTO import_sources_v2(
    id, source_path, source_type, discovery_fingerprint, state,
    deletion_state, retained_reason, error_code, error_message,
    discovered_at, updated_at
)
SELECT
    id, source_path, source_type, discovery_fingerprint, state,
    deletion_state, retained_reason, error_code, error_message,
    discovered_at, updated_at
FROM import_sources;

INSERT INTO import_items_v2(
    id, source_id, zip_entry_name, staged_path, sha256, asset_id,
    state, attempt_count, error_code, error_message, created_at, updated_at
)
SELECT
    id, source_id, zip_entry_name, staged_path, sha256, asset_id,
    state, attempt_count, error_code, error_message, created_at, updated_at
FROM import_items;

INSERT INTO ai_runs_v2(
    id, import_item_id, asset_id, provider, model, reasoning_effort,
    image_detail, prompt_version, schema_version, attempt_number,
    started_at, completed_at, latency_ms, request_id, usage_json,
    outcome, error_code, error_message, normalized_result_json
)
SELECT
    id, import_item_id, asset_id, provider, model, reasoning_effort,
    image_detail, prompt_version, schema_version, attempt_number,
    started_at, completed_at, latency_ms, request_id, usage_json,
    outcome, error_code, error_message, normalized_result_json
FROM ai_runs;

DROP TABLE ai_runs;
DROP TABLE import_items;
DROP TABLE import_sources;

ALTER TABLE import_sources_v2 RENAME TO import_sources;
ALTER TABLE import_items_v2 RENAME TO import_items;
ALTER TABLE ai_runs_v2 RENAME TO ai_runs;

CREATE UNIQUE INDEX import_items_source_entry_uq
    ON import_items(source_id, ifnull(zip_entry_name, ''));
CREATE INDEX import_items_asset_idx ON import_items(asset_id);
CREATE INDEX ai_runs_item_idx ON ai_runs(import_item_id, attempt_number);
CREATE INDEX ai_runs_asset_idx ON ai_runs(asset_id);
