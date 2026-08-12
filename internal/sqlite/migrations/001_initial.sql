CREATE TABLE assets (
    id TEXT PRIMARY KEY CHECK(length(id) = 32),
    sha256 BLOB NOT NULL UNIQUE CHECK(length(sha256) = 32),
    original_filename TEXT NOT NULL CHECK(length(original_filename) BETWEEN 1 AND 255),
    managed_path TEXT NOT NULL UNIQUE CHECK(length(managed_path) BETWEEN 1 AND 1024),
    format TEXT NOT NULL CHECK(format IN ('png', 'jpeg', 'webp', 'gif')),
    mime_type TEXT NOT NULL CHECK(mime_type IN ('image/png', 'image/jpeg', 'image/webp', 'image/gif')),
    file_size_bytes INTEGER NOT NULL CHECK(file_size_bytes >= 0),
    display_width INTEGER NOT NULL CHECK(display_width > 0),
    display_height INTEGER NOT NULL CHECK(display_height > 0),
    orientation_class TEXT NOT NULL CHECK(orientation_class IN ('square', 'portrait', 'landscape')),
    has_alpha INTEGER NOT NULL CHECK(has_alpha IN (0, 1)),
    has_transparency INTEGER NOT NULL CHECK(has_transparency IN (0, 1)),
    encoded_animated INTEGER NOT NULL CHECK(encoded_animated IN (0, 1)),
    encoded_frame_count INTEGER NOT NULL CHECK(encoded_frame_count >= 1),
    dominant_colors_json TEXT NOT NULL DEFAULT '[]',
    title TEXT,
    description TEXT,
    primary_type TEXT CHECK(primary_type IS NULL OR primary_type IN (
        'character', 'creature', 'terrain', 'environment', 'prop', 'building',
        'vehicle', 'weapon-tool', 'collectible', 'ui', 'icon', 'background',
        'texture-material', 'vfx', 'decal', 'other'
    )),
    style TEXT,
    pixel_art INTEGER CHECK(pixel_art IS NULL OR pixel_art IN (0, 1)),
    ai_confidence REAL CHECK(ai_confidence IS NULL OR ai_confidence BETWEEN 0.0 AND 1.0),
    layout_kind TEXT NOT NULL DEFAULT 'single' CHECK(layout_kind IN ('single', 'sprite-sheet', 'tile-sheet')),
    search_tags TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK(state IN ('staged', 'ready', 'integrity-failed')),
    version INTEGER NOT NULL DEFAULT 1 CHECK(version >= 1),
    imported_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(id, layout_kind),
    CHECK((encoded_animated = 0 AND encoded_frame_count = 1) OR (encoded_animated = 1 AND encoded_frame_count > 1))
) STRICT;

CREATE TABLE asset_sheet_layouts (
    asset_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK(kind IN ('sprite-sheet', 'tile-sheet')),
    columns_count INTEGER,
    rows_count INTEGER,
    cell_width INTEGER,
    cell_height INTEGER,
    frame_count INTEGER CHECK(frame_count IS NULL OR frame_count > 0),
    animation_label TEXT CHECK(animation_label IS NULL OR length(animation_label) BETWEEN 1 AND 64),
    updated_at TEXT NOT NULL,
    FOREIGN KEY(asset_id, kind) REFERENCES assets(id, layout_kind) ON DELETE CASCADE,
    CHECK(
        (columns_count IS NULL AND rows_count IS NULL AND cell_width IS NULL AND cell_height IS NULL)
        OR
        (columns_count > 0 AND rows_count > 0 AND cell_width > 0 AND cell_height > 0)
    ),
    CHECK(frame_count IS NULL OR columns_count IS NULL OR frame_count <= columns_count * rows_count),
    CHECK(kind = 'sprite-sheet' OR (frame_count IS NULL AND animation_label IS NULL))
) STRICT;

CREATE TABLE thumbnails (
    asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    mime_type TEXT NOT NULL CHECK(mime_type = 'image/png'),
    width INTEGER NOT NULL CHECK(width BETWEEN 1 AND 320),
    height INTEGER NOT NULL CHECK(height BETWEEN 1 AND 320),
    byte_length INTEGER NOT NULL CHECK(byte_length > 0),
    data BLOB NOT NULL CHECK(length(data) = byte_length)
) STRICT;

CREATE TABLE tags (
    id INTEGER PRIMARY KEY,
    facet TEXT NOT NULL CHECK(length(facet) BETWEEN 1 AND 64),
    slug TEXT NOT NULL CHECK(length(slug) BETWEEN 1 AND 64),
    label TEXT NOT NULL CHECK(length(label) BETWEEN 1 AND 128),
    UNIQUE(facet, slug),
    CHECK(slug NOT LIKE 'not-%' AND slug <> 'non-pixel-art')
) STRICT;

CREATE TABLE asset_tags (
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK(origin IN ('ai', 'deterministic', 'user')),
    created_at TEXT NOT NULL,
    PRIMARY KEY(asset_id, tag_id)
) STRICT;

CREATE TABLE import_sources (
    id TEXT PRIMARY KEY CHECK(length(id) = 32),
    source_path TEXT NOT NULL UNIQUE CHECK(length(source_path) BETWEEN 1 AND 1024),
    source_type TEXT NOT NULL CHECK(source_type IN ('loose', 'zip')),
    discovery_fingerprint TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('discovered', 'processing', 'ready', 'duplicate', 'blocked', 'failed', 'retained', 'deleted')),
    deletion_state TEXT NOT NULL CHECK(deletion_state IN ('not-eligible', 'eligible', 'pending', 'deleted', 'failed')),
    retained_reason TEXT,
    error_code TEXT,
    error_message TEXT CHECK(error_message IS NULL OR length(error_message) <= 2048),
    discovered_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE import_items (
    id TEXT PRIMARY KEY CHECK(length(id) = 32),
    source_id TEXT NOT NULL REFERENCES import_sources(id) ON DELETE CASCADE,
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

CREATE UNIQUE INDEX import_items_source_entry_uq
    ON import_items(source_id, ifnull(zip_entry_name, ''));
CREATE INDEX import_items_asset_idx ON import_items(asset_id);

CREATE TABLE ai_runs (
    id TEXT PRIMARY KEY CHECK(length(id) = 32),
    import_item_id TEXT NOT NULL REFERENCES import_items(id) ON DELETE CASCADE,
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

CREATE INDEX ai_runs_item_idx ON ai_runs(import_item_id, attempt_number);
CREATE INDEX ai_runs_asset_idx ON ai_runs(asset_id);
CREATE INDEX assets_ready_imported_idx ON assets(state, imported_at DESC, id);
CREATE INDEX assets_primary_type_idx ON assets(primary_type, state);
CREATE INDEX assets_layout_kind_idx ON assets(layout_kind, state);

CREATE VIRTUAL TABLE asset_search USING fts5(
    title,
    description,
    original_filename,
    style,
    search_tags,
    content='assets',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER assets_search_insert AFTER INSERT ON assets BEGIN
    INSERT INTO asset_search(rowid, title, description, original_filename, style, search_tags)
    VALUES (new.rowid, new.title, new.description, new.original_filename, new.style, new.search_tags);
END;

CREATE TRIGGER assets_search_delete AFTER DELETE ON assets BEGIN
    INSERT INTO asset_search(asset_search, rowid, title, description, original_filename, style, search_tags)
    VALUES ('delete', old.rowid, old.title, old.description, old.original_filename, old.style, old.search_tags);
END;

CREATE TRIGGER assets_search_update AFTER UPDATE OF title, description, original_filename, style, search_tags ON assets BEGIN
    INSERT INTO asset_search(asset_search, rowid, title, description, original_filename, style, search_tags)
    VALUES ('delete', old.rowid, old.title, old.description, old.original_filename, old.style, old.search_tags);
    INSERT INTO asset_search(rowid, title, description, original_filename, style, search_tags)
    VALUES (new.rowid, new.title, new.description, new.original_filename, new.style, new.search_tags);
END;
