//go:build unit

package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func backupRecordTestRows(records ...service.BackupRecord) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "status", "backup_type", "file_name", "s3_key", "size_bytes", "triggered_by",
		"error_message", "started_at", "finished_at", "expires_at", "progress",
		"restore_status", "restore_error", "restored_at",
	})
	for _, record := range records {
		rows.AddRow(
			record.ID,
			record.Status,
			record.BackupType,
			record.FileName,
			record.S3Key,
			record.SizeBytes,
			record.TriggeredBy,
			record.ErrorMsg,
			record.StartedAt,
			record.FinishedAt,
			record.ExpiresAt,
			record.Progress,
			record.RestoreStatus,
			record.RestoreError,
			record.RestoredAt,
		)
	}
	return rows
}

func backupRecordTestArgs(record *service.BackupRecord) []driver.Value {
	return []driver.Value{
		record.ID,
		record.Status,
		record.BackupType,
		record.FileName,
		record.S3Key,
		record.SizeBytes,
		record.TriggeredBy,
		record.ErrorMsg,
		record.StartedAt,
		record.FinishedAt,
		record.ExpiresAt,
		record.Progress,
		record.RestoreStatus,
		record.RestoreError,
		record.RestoredAt,
	}
}

func TestBackupRecordRepositoryListAndGet(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewBackupRecordRepository(db)
	record := service.BackupRecord{
		ID:          "abc123",
		Status:      "completed",
		BackupType:  "postgres",
		FileName:    "db.sql.gz",
		S3Key:       "backups/db.sql.gz",
		SizeBytes:   42,
		TriggeredBy: "manual",
		StartedAt:   "2026-07-03T00:00:00Z",
	}

	mock.ExpectQuery("SELECT .* FROM backup_records\\s+ORDER BY started_at DESC, id DESC").
		WillReturnRows(backupRecordTestRows(record))
	mock.ExpectQuery("SELECT .* FROM backup_records\\s+WHERE id = \\$1").
		WithArgs(record.ID).
		WillReturnRows(backupRecordTestRows(record))

	records, err := repo.List(context.Background())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, record.ID, records[0].ID)

	got, err := repo.Get(context.Background(), record.ID)
	require.NoError(t, err)
	require.Equal(t, record.S3Key, got.S3Key)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBackupRecordRepositoryGetNotFound(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewBackupRecordRepository(db)

	mock.ExpectQuery("SELECT .* FROM backup_records\\s+WHERE id = \\$1").
		WithArgs("missing").
		WillReturnRows(backupRecordTestRows())

	_, err := repo.Get(context.Background(), "missing")
	require.ErrorIs(t, err, service.ErrBackupNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBackupRecordRepositoryUpsertPrunes(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewBackupRecordRepository(db)
	record := &service.BackupRecord{
		ID:          "abc123",
		Status:      "running",
		BackupType:  "postgres",
		FileName:    "db.sql.gz",
		S3Key:       "backups/db.sql.gz",
		TriggeredBy: "manual",
		StartedAt:   "2026-07-03T00:00:00Z",
	}

	mock.ExpectExec("INSERT INTO backup_records").
		WithArgs(backupRecordTestArgs(record)...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM backup_records[\\s\\S]+status <> 'running'[\\s\\S]+restore_status <> 'running'").
		WithArgs(100).
		WillReturnResult(sqlmock.NewResult(0, 0))

	require.NoError(t, repo.Upsert(context.Background(), record))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBackupRecordRepositoryUpdateNotFoundDoesNotUpsert(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewBackupRecordRepository(db)
	record := &service.BackupRecord{
		ID:         "deleted",
		Status:     "completed",
		BackupType: "postgres",
		StartedAt:  "2026-07-03T00:00:00Z",
	}

	mock.ExpectExec("UPDATE backup_records SET").
		WithArgs(backupRecordTestArgs(record)...).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.Update(context.Background(), record)
	require.ErrorIs(t, err, service.ErrBackupNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBackupRecordRepositoryDeleteNotFound(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewBackupRecordRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM backup_records WHERE id = $1")).
		WithArgs("missing").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.Delete(context.Background(), "missing")
	require.ErrorIs(t, err, service.ErrBackupNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBackupRecordRepositoryPropagatesRowsAffectedError(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := NewBackupRecordRepository(db)
	record := &service.BackupRecord{ID: "abc123"}

	mock.ExpectExec("UPDATE backup_records SET").
		WithArgs(backupRecordTestArgs(record)...).
		WillReturnResult(sqlmock.NewErrorResult(sql.ErrConnDone))

	err := repo.Update(context.Background(), record)
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.NoError(t, mock.ExpectationsWereMet())
}
