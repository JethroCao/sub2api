-- Store backup metadata in a shared table so multi-instance deployments do not
-- race on process-local settings JSON.

CREATE TABLE IF NOT EXISTS backup_records (
    id             TEXT PRIMARY KEY,
    status         VARCHAR(32) NOT NULL DEFAULT '',
    backup_type    VARCHAR(32) NOT NULL DEFAULT 'postgres',
    file_name      TEXT NOT NULL DEFAULT '',
    s3_key         TEXT NOT NULL DEFAULT '',
    size_bytes     BIGINT NOT NULL DEFAULT 0,
    triggered_by   VARCHAR(32) NOT NULL DEFAULT '',
    error_message  TEXT NOT NULL DEFAULT '',
    started_at     TEXT NOT NULL DEFAULT '',
    finished_at    TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL DEFAULT '',
    progress       VARCHAR(32) NOT NULL DEFAULT '',
    restore_status VARCHAR(32) NOT NULL DEFAULT '',
    restore_error  TEXT NOT NULL DEFAULT '',
    restored_at    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_backup_records_started_at
    ON backup_records (started_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_backup_records_status
    ON backup_records (status);

DO $$
DECLARE
    raw_records TEXT;
    raw_json    JSONB;
BEGIN
    SELECT value INTO raw_records FROM settings WHERE key = 'backup_records';
    IF raw_records IS NULL OR btrim(raw_records) = '' THEN
        RETURN;
    END IF;

    raw_json := raw_records::jsonb;
    IF jsonb_typeof(raw_json) <> 'array' THEN
        RAISE EXCEPTION 'settings.backup_records must be a JSON array, got %', jsonb_typeof(raw_json);
    END IF;

    INSERT INTO backup_records (
        id, status, backup_type, file_name, s3_key, size_bytes, triggered_by,
        error_message, started_at, finished_at, expires_at, progress,
        restore_status, restore_error, restored_at
    )
    SELECT
        COALESCE(NULLIF(id, ''), substr(md5(random()::text || clock_timestamp()::text), 1, 8)),
        COALESCE(status, ''),
        COALESCE(backup_type, 'postgres'),
        COALESCE(file_name, ''),
        COALESCE(s3_key, ''),
        COALESCE(size_bytes, 0),
        COALESCE(triggered_by, ''),
        COALESCE(error_message, ''),
        COALESCE(started_at, ''),
        COALESCE(finished_at, ''),
        COALESCE(expires_at, ''),
        COALESCE(progress, ''),
        COALESCE(restore_status, ''),
        COALESCE(restore_error, ''),
        COALESCE(restored_at, '')
    FROM jsonb_to_recordset(raw_json) AS x(
        id TEXT,
        status TEXT,
        backup_type TEXT,
        file_name TEXT,
        s3_key TEXT,
        size_bytes BIGINT,
        triggered_by TEXT,
        error_message TEXT,
        started_at TEXT,
        finished_at TEXT,
        expires_at TEXT,
        progress TEXT,
        restore_status TEXT,
        restore_error TEXT,
        restored_at TEXT
    )
    ON CONFLICT (id) DO UPDATE SET
        status = EXCLUDED.status,
        backup_type = EXCLUDED.backup_type,
        file_name = EXCLUDED.file_name,
        s3_key = EXCLUDED.s3_key,
        size_bytes = EXCLUDED.size_bytes,
        triggered_by = EXCLUDED.triggered_by,
        error_message = EXCLUDED.error_message,
        started_at = EXCLUDED.started_at,
        finished_at = EXCLUDED.finished_at,
        expires_at = EXCLUDED.expires_at,
        progress = EXCLUDED.progress,
        restore_status = EXCLUDED.restore_status,
        restore_error = EXCLUDED.restore_error,
        restored_at = EXCLUDED.restored_at,
        updated_at = NOW();
END $$;
