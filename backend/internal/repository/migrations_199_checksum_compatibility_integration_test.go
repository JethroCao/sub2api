//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestMigrationsUpgradeFromAppliedMigration199Versions(t *testing.T) {
	round4SQL, err := migrations.FS.ReadFile(migration199Filename)
	require.NoError(t, err)

	for _, fixture := range []struct {
		name     string
		sql      []byte
		checksum string
	}{
		{name: "round_3", sql: []byte(migration199Round3SQL), checksum: migration199Round3Checksum},
		{name: "round_4", sql: round4SQL, checksum: migration199Round4Checksum},
	} {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			resetMigration199ToCurrentOnCleanup(t)
			require.Equal(t, fixture.checksum, migrationChecksum(string(fixture.sql)))

			user := mustCreateUser(t, testEntClient(t), &service.User{
				Email:        fmt.Sprintf("migration-199-%s-%d@example.com", fixture.name, time.Now().UnixNano()),
				PasswordHash: "hash",
				Balance:      123.25,
			})
			t.Cleanup(func() {
				_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, user.ID)
			})

			ctx := context.Background()
			prepareOriginalMigration199State(t, user.ID)
			require.NoError(t, applyMigrationsFS(ctx, integrationDB, fstest.MapFS{
				migration199Filename: {Data: fixture.sql},
			}))

			requireMigrationChecksum(t, migration199Filename, fixture.checksum)
			requireMigration199CurrentState(t, user.ID)

			// Current migration 199 needs ACCESS EXCLUSIVE on users. Holding this
			// weaker lock proves an accepted applied version is skipped, not rerun.
			blocker, err := integrationDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, err = blocker.ExecContext(ctx, `LOCK TABLE users IN ACCESS SHARE MODE`)
			require.NoError(t, err)

			runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			runErr := ApplyMigrations(runCtx, integrationDB)
			cancel()
			require.NoError(t, blocker.Rollback())
			require.NoError(t, runErr)

			requireMigrationChecksum(t, migration199Filename, fixture.checksum)
			requireMigration199CurrentState(t, user.ID)
		})
	}
}

func TestMigrationsRejectTampered199AndUnrelatedChecksumMismatch(t *testing.T) {
	t.Run("tampered_migration_199", func(t *testing.T) {
		resetMigration199ToCurrentOnCleanup(t)
		tamperedRound3 := strings.Replace(migration199Round3SQL, "-- Restore", "-- restore", 1)
		const tamperedChecksum = "404c2225cc99374f05c7712ef5652bd706dc85c25edf8f9ba43dd04296558572"
		require.Equal(t, tamperedChecksum, migrationChecksum(tamperedRound3))

		ctx := context.Background()
		_, err := integrationDB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE filename = $1`, migration199Filename)
		require.NoError(t, err)
		require.NoError(t, applyMigrationsFS(ctx, integrationDB, fstest.MapFS{
			migration199Filename: {Data: []byte(tamperedRound3)},
		}))

		err = ApplyMigrations(ctx, integrationDB)
		require.Error(t, err)
		require.Contains(t, err.Error(), "migration "+migration199Filename+" checksum mismatch")
		require.Contains(t, err.Error(), "db="+tamperedChecksum+" file="+migration199Round4Checksum)
		requireMigrationChecksum(t, migration199Filename, tamperedChecksum)
	})

	t.Run("unrelated_migration", func(t *testing.T) {
		const unrelatedFilename = "198_unified_video_billing_precision.sql"
		var originalChecksum string
		require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
			SELECT checksum FROM schema_migrations WHERE filename = $1
		`, unrelatedFilename).Scan(&originalChecksum))
		t.Cleanup(func() {
			_, _ = integrationDB.ExecContext(context.Background(), `
				UPDATE schema_migrations SET checksum = $1 WHERE filename = $2
			`, originalChecksum, unrelatedFilename)
		})

		const tamperedChecksum = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		_, err := integrationDB.ExecContext(context.Background(), `
			UPDATE schema_migrations SET checksum = $1 WHERE filename = $2
		`, tamperedChecksum, unrelatedFilename)
		require.NoError(t, err)

		err = ApplyMigrations(context.Background(), integrationDB)
		require.Error(t, err)
		require.Contains(t, err.Error(), "migration "+unrelatedFilename+" checksum mismatch")
		require.Contains(t, err.Error(), "db="+tamperedChecksum)
	})
}

func resetMigration199ToCurrentOnCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := integrationDB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE filename = $1`, migration199Filename)
		require.NoError(t, err)
		require.NoError(t, ApplyMigrations(ctx, integrationDB))
		requireMigrationChecksum(t, migration199Filename, migration199Round4Checksum)
	})
}

func requireMigrationChecksum(t *testing.T, filename, expected string) {
	t.Helper()
	var actual string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT checksum FROM schema_migrations WHERE filename = $1
	`, filename).Scan(&actual))
	require.Equal(t, expected, actual)
}

func requireMigration199CurrentState(t *testing.T, userID int64) {
	t.Helper()
	requireNumericColumnOnDB(t, "users", "balance", 20, 8)
	requireNumericColumnOnDB(t, "users", "frozen_balance", 20, 8)
	requireNumericColumnOnDB(t, "video_tasks", "estimated_amount", 20, 8)
	requireNumericColumnOnDB(t, "video_tasks", "frozen_amount", 20, 8)
	requireNumericColumnOnDB(t, "video_tasks", "settled_amount", 20, 8)

	var balance, frozenBalance string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT balance::text, frozen_balance::text FROM users WHERE id = $1
	`, userID).Scan(&balance, &frozenBalance))
	require.Equal(t, "123.25000001", balance)
	require.Equal(t, "0.00000001", frozenBalance)
}

func prepareOriginalMigration199State(t *testing.T, userID int64) {
	t.Helper()
	ctx := context.Background()
	_, err := integrationDB.ExecContext(ctx, `
		ALTER TABLE video_tasks
			ALTER COLUMN estimated_amount TYPE DECIMAL(20,10) USING estimated_amount::DECIMAL(20,10),
			ALTER COLUMN frozen_amount TYPE DECIMAL(20,10) USING frozen_amount::DECIMAL(20,10),
			ALTER COLUMN settled_amount TYPE DECIMAL(20,10) USING settled_amount::DECIMAL(20,10)
	`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		ALTER TABLE users
			ALTER COLUMN balance TYPE DECIMAL(22,10) USING balance::DECIMAL(22,10),
			ALTER COLUMN frozen_balance TYPE DECIMAL(22,10) USING frozen_balance::DECIMAL(22,10)
	`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE users
		SET balance = 123.2500000050, frozen_balance = 0.0000000050
		WHERE id = $1
	`, userID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE filename = $1`, migration199Filename)
	require.NoError(t, err)

	requireNumericColumnOnDB(t, "users", "balance", 22, 10)
	requireNumericColumnOnDB(t, "users", "frozen_balance", 22, 10)
	requireNumericColumnOnDB(t, "video_tasks", "estimated_amount", 20, 10)
	requireNumericColumnOnDB(t, "video_tasks", "frozen_amount", 20, 10)
	requireNumericColumnOnDB(t, "video_tasks", "settled_amount", 20, 10)

	var balance, frozenBalance string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT balance::text, frozen_balance::text FROM users WHERE id = $1
	`, userID).Scan(&balance, &frozenBalance))
	require.Equal(t, "123.2500000050", balance)
	require.Equal(t, "0.0000000050", frozenBalance)
}
