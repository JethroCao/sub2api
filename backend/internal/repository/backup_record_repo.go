package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type backupRecordRepository struct {
	sql sqlExecutor
}

func NewBackupRecordRepository(sqlDB *sql.DB) service.BackupRecordRepository {
	return &backupRecordRepository{sql: sqlDB}
}

const backupRecordSelectColumns = `
	id, status, backup_type, file_name, s3_key, size_bytes, triggered_by,
	error_message, started_at, finished_at, expires_at, progress,
	restore_status, restore_error, restored_at
`

func (r *backupRecordRepository) List(ctx context.Context) ([]service.BackupRecord, error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT `+backupRecordSelectColumns+`
		FROM backup_records
		ORDER BY started_at DESC, id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	records := make([]service.BackupRecord, 0)
	for rows.Next() {
		record, err := scanBackupRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

func (r *backupRecordRepository) Get(ctx context.Context, id string) (*service.BackupRecord, error) {
	record := &service.BackupRecord{}
	err := scanSingleRow(ctx, r.sql, `
		SELECT `+backupRecordSelectColumns+`
		FROM backup_records
		WHERE id = $1
	`, []any{id},
		&record.ID,
		&record.Status,
		&record.BackupType,
		&record.FileName,
		&record.S3Key,
		&record.SizeBytes,
		&record.TriggeredBy,
		&record.ErrorMsg,
		&record.StartedAt,
		&record.FinishedAt,
		&record.ExpiresAt,
		&record.Progress,
		&record.RestoreStatus,
		&record.RestoreError,
		&record.RestoredAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrBackupNotFound
	}
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (r *backupRecordRepository) Upsert(ctx context.Context, record *service.BackupRecord) error {
	if record == nil {
		return nil
	}
	_, err := r.sql.ExecContext(ctx, `
		INSERT INTO backup_records (
			id, status, backup_type, file_name, s3_key, size_bytes, triggered_by,
			error_message, started_at, finished_at, expires_at, progress,
			restore_status, restore_error, restored_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
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
			updated_at = NOW()
	`, backupRecordArgs(record)...)
	if err != nil {
		return err
	}
	return r.prune(ctx)
}

func (r *backupRecordRepository) Update(ctx context.Context, record *service.BackupRecord) error {
	if record == nil {
		return service.ErrBackupNotFound
	}
	args := append([]any{record.ID}, backupRecordUpdateArgs(record)...)
	result, err := r.sql.ExecContext(ctx, `
		UPDATE backup_records SET
			status = $2,
			backup_type = $3,
			file_name = $4,
			s3_key = $5,
			size_bytes = $6,
			triggered_by = $7,
			error_message = $8,
			started_at = $9,
			finished_at = $10,
			expires_at = $11,
			progress = $12,
			restore_status = $13,
			restore_error = $14,
			restored_at = $15,
			updated_at = NOW()
		WHERE id = $1
	`, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrBackupNotFound
	}
	return nil
}

func (r *backupRecordRepository) Delete(ctx context.Context, id string) error {
	result, err := r.sql.ExecContext(ctx, `DELETE FROM backup_records WHERE id = $1`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrBackupNotFound
	}
	return nil
}

func (r *backupRecordRepository) prune(ctx context.Context) error {
	_, err := r.sql.ExecContext(ctx, `
		DELETE FROM backup_records
		WHERE id IN (
			SELECT id
			FROM backup_records
			WHERE status <> 'running'
			  AND restore_status <> 'running'
			ORDER BY started_at DESC, id DESC
			OFFSET $1
		)
	`, maxBackupRecordsForRepo)
	return err
}

func scanBackupRecord(row scannable) (*service.BackupRecord, error) {
	record := &service.BackupRecord{}
	if err := row.Scan(
		&record.ID,
		&record.Status,
		&record.BackupType,
		&record.FileName,
		&record.S3Key,
		&record.SizeBytes,
		&record.TriggeredBy,
		&record.ErrorMsg,
		&record.StartedAt,
		&record.FinishedAt,
		&record.ExpiresAt,
		&record.Progress,
		&record.RestoreStatus,
		&record.RestoreError,
		&record.RestoredAt,
	); err != nil {
		return nil, err
	}
	return record, nil
}

func backupRecordArgs(record *service.BackupRecord) []any {
	return append([]any{record.ID}, backupRecordUpdateArgs(record)...)
}

func backupRecordUpdateArgs(record *service.BackupRecord) []any {
	return []any{
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

const maxBackupRecordsForRepo = 100
