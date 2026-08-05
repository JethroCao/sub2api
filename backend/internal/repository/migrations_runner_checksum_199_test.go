package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const (
	migration199Filename       = "199_restore_video_billing_ledger_scale.sql"
	migration199Round3Checksum = "5718d87e9da8b15e4ba15e80499842f7f58a31791e1c24bebb9e1c90073e3463"
	migration199Round4Checksum = "4fca876d404357e8a79eb3a4ecf92dabdc291bb4117b35235bc0d8dd7b9e468d"

	migration199Round3SQL = `-- Restore the shared user ledger's established scale so existing batch-image
-- holds retain their historical rounding and sufficiency behavior. Canonicalize
-- video charges to the same quantum in this forward-only correction.

ALTER TABLE users
    ALTER COLUMN balance TYPE DECIMAL(20,8) USING ROUND(balance, 8),
    ALTER COLUMN frozen_balance TYPE DECIMAL(20,8) USING ROUND(frozen_balance, 8);

ALTER TABLE video_tasks
    ALTER COLUMN estimated_amount TYPE DECIMAL(20,8) USING ROUND(estimated_amount, 8),
    ALTER COLUMN frozen_amount TYPE DECIMAL(20,8) USING ROUND(frozen_amount, 8),
    ALTER COLUMN settled_amount TYPE DECIMAL(20,8) USING ROUND(settled_amount, 8);
`
)

func TestMigration199ChecksumCompatibilityIsLimitedToExactPublishedVersions(t *testing.T) {
	round4SQL, err := migrations.FS.ReadFile(migration199Filename)
	require.NoError(t, err)

	require.Equal(t, "447cbb2c913f8e9ee5d693b6262edc383b3a989d9a79b50fb6873b79f4c3851f", rawMigrationChecksum(migration199Round3SQL))
	require.Equal(t, "8768c074dbb35a7394da98a2060ed73dd6870771f4cd7767448f8d8406f31e84", rawMigrationChecksum(string(round4SQL)))
	require.Equal(t, migration199Round3Checksum, migrationChecksum(migration199Round3SQL))
	require.Equal(t, migration199Round4Checksum, migrationChecksum(string(round4SQL)))

	require.True(t, isMigrationChecksumCompatible(migration199Filename, migration199Round3Checksum, migration199Round4Checksum))
	require.True(t, isMigrationChecksumCompatible(migration199Filename, migration199Round4Checksum, migration199Round3Checksum))

	tamperedRound3 := strings.Replace(migration199Round3SQL, "-- Restore", "-- restore", 1)
	require.Equal(t, "404c2225cc99374f05c7712ef5652bd706dc85c25edf8f9ba43dd04296558572", migrationChecksum(tamperedRound3))
	require.False(t, isMigrationChecksumCompatible(migration199Filename, migrationChecksum(tamperedRound3), migration199Round4Checksum))
	require.False(t, isMigrationChecksumCompatible(migration199Filename, migration199Round3Checksum, migrationChecksum(tamperedRound3)))
	require.False(t, isMigrationChecksumCompatible("198_unified_video_billing_precision.sql", migration199Round3Checksum, migration199Round4Checksum))
	require.False(t, isMigrationChecksumCompatible("archive/"+migration199Filename, migration199Round3Checksum, migration199Round4Checksum))
	require.False(t, isMigrationChecksumCompatible(migration199Filename+".bak", migration199Round3Checksum, migration199Round4Checksum))
}

func rawMigrationChecksum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
