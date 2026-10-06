-- Restore legacy backup metadata into the upstream record store.
-- Historical tables and migration checksums remain unchanged. Merge common
-- fields from the authoritative legacy table while preserving upstream-only
-- metadata (parts, monthly archives, restore start timestamps) already in JSON.
-- Table membership is authoritative too: the JSON snapshot stopped receiving
-- deletions once migration 161 switched this fork to the table-backed store.
DO $$
DECLARE
    raw_records TEXT;
    existing_records JSONB := '[]'::JSONB;
    merged_records JSONB;
BEGIN
    IF to_regclass('backup_records') IS NULL THEN
        RETURN;
    END IF;

    SELECT value INTO raw_records FROM settings WHERE key = 'backup_records';
    IF raw_records IS NOT NULL AND btrim(raw_records) <> '' THEN
        existing_records := raw_records::JSONB;
        IF jsonb_typeof(existing_records) <> 'array' THEN
            RAISE EXCEPTION 'settings.backup_records must be a JSON array';
        END IF;
    END IF;

    WITH existing AS (
        SELECT item->>'id' AS id, item
        FROM jsonb_array_elements(existing_records) AS entries(item)
    ), legacy AS (
        SELECT id, to_jsonb(record) - 'created_at' - 'updated_at' AS item
        FROM backup_records AS record
    ), merged AS (
        SELECT COALESCE(existing.item, '{}'::JSONB) || legacy.item AS item
        FROM legacy LEFT JOIN existing USING (id)
    )
    SELECT COALESCE(jsonb_agg(item ORDER BY item->>'started_at' DESC, item->>'id' DESC), '[]'::JSONB)
    INTO merged_records FROM merged;

    INSERT INTO settings (key, value, updated_at)
    VALUES ('backup_records', merged_records::TEXT, NOW())
    ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW();
END $$;
