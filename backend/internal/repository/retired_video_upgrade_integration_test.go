//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestRetiredVideoUpgradePreservesLegacyPlatformRows(t *testing.T) {
	tx := testTx(t)
	_, err := tx.Exec(`CREATE TEMP TABLE user_platform_quotas (platform TEXT);
		CREATE TEMP TABLE composite_model_routes (target_platform TEXT);
		INSERT INTO user_platform_quotas VALUES ('video');
		INSERT INTO composite_model_routes VALUES ('video');`)
	require.NoError(t, err)
	content, err := migrations.FS.ReadFile("241_add_typesafe_platform.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), string(content))
	require.NoError(t, err, "removing the runtime must not make legacy rows prevent upgrades")
	_, err = tx.ExecContext(context.Background(), string(content))
	require.NoError(t, err, "platform migration remains reentrant")
	var count int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM user_platform_quotas WHERE platform = 'video'`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = tx.Exec(`INSERT INTO user_platform_quotas VALUES ('not_a_platform')`)
	require.Error(t, err, "retaining historical video rows must not allow arbitrary platforms")
}

func TestRetiredVideoUpgradeRestoresBackupHistory(t *testing.T) {
	tx := testTx(t)
	_, err := tx.Exec(`CREATE TEMP TABLE settings (key TEXT PRIMARY KEY, value TEXT,
		updated_at TIMESTAMPTZ DEFAULT NOW());
		CREATE TEMP TABLE backup_records (LIKE public.backup_records INCLUDING ALL);
		INSERT INTO backup_records (id, status, started_at, size_bytes, s3_key)
		VALUES ('legacy', 'completed', '2026-10-01T00:00:00Z', 123, 'legacy.sql.gz'),
		       ('native', 'completed', '2026-10-02T00:00:00Z', 456, 'native.sql.gz');
		INSERT INTO settings (key, value) VALUES ('backup_records',
		'[ {"id":"legacy","status":"running","parts":[{"index":1,"s3_key":"part-one"}]},
		   {"id":"native","status":"completed","started_at":"2026-10-02T00:00:00Z","monthly_archive":{"month":"2026-10"}},
		   {"id":"deleted-before-upgrade","status":"completed","s3_key":"already-deleted.sql.gz"} ]');`)
	require.NoError(t, err)
	content, err := migrations.FS.ReadFile("242_restore_legacy_backup_record_history.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(context.Background(), string(content))
		require.NoError(t, err)
	}
	var raw string
	require.NoError(t, tx.QueryRow(`SELECT value FROM settings WHERE key = 'backup_records'`).Scan(&raw))
	var records []map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &records))
	require.Len(t, records, 2, "history is imported without duplicates or resurrecting deleted records")
	byID := map[string]map[string]any{}
	for _, record := range records {
		byID[record["id"].(string)] = record
	}
	require.Equal(t, "completed", byID["legacy"]["status"], "table is authoritative over stale settings")
	require.Equal(t, float64(123), byID["legacy"]["size_bytes"])
	require.Equal(t, "legacy.sql.gz", byID["legacy"]["s3_key"])
	require.NotEmpty(t, byID["legacy"]["parts"], "new upstream metadata is not discarded")
	require.NotEmpty(t, byID["native"]["monthly_archive"])
	require.NotContains(t, byID, "deleted-before-upgrade", "the legacy table is authoritative for deletions")
	var count int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM backup_records`).Scan(&count))
	require.Equal(t, 2, count, "legacy table is not purged")
}

func TestRetiredVideoUpgradeRejectsCorruptBackupHistory(t *testing.T) {
	tx := testTx(t)
	_, err := tx.Exec(`CREATE TEMP TABLE settings (key TEXT PRIMARY KEY, value TEXT,
		updated_at TIMESTAMPTZ DEFAULT NOW());
		CREATE TEMP TABLE backup_records (LIKE public.backup_records INCLUDING ALL);
		INSERT INTO settings (key, value) VALUES ('backup_records', '{}');`)
	require.NoError(t, err)
	content, err := migrations.FS.ReadFile("242_restore_legacy_backup_record_history.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), string(content))
	require.Error(t, err, "a corrupt history must not be silently replaced with an empty list")
}
