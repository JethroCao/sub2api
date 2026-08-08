package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoTaskRepositoryMarkSettledPreservesPollErrorWithoutSettlementSignal(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	requestID := "vid_0123456789abcdef0123456789abcdef"
	stop := errors.New("settlement update contract verified")
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT status, version, settled_at, lease_owner, lease_expires_at,.*FROM video_tasks WHERE request_id = \$1 FOR UPDATE`).
		WithArgs(requestID, float64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "version", "settled_at", "lease_owner", "lease_expires_at", "database_now", "within_hold"}).
			AddRow(service.VideoTaskFailed, int64(7), nil, "worker-settle", now.Add(time.Minute), now, true))
	mock.ExpectExec(`(?s)UPDATE video_tasks.*last_error_code = CASE WHEN \$8 = '' THEN last_error_code ELSE NULLIF\(\$8, ''\) END,.*last_error_message = CASE WHEN \$8 = '' THEN last_error_message ELSE NULLIF\(\$9, ''\) END,.*last_error_retryable = CASE WHEN \$8 = '' THEN last_error_retryable ELSE \$10 END`).
		WillReturnError(stop)
	mock.ExpectRollback()

	repo := NewVideoTaskRepository(db)
	err = repo.MarkSettled(context.Background(), service.MarkVideoSettledParams{
		RequestID: requestID, ExpectedVersion: 7, LeaseOwner: "worker-settle",
		SettledAmount: 0, BillingStatus: "released", SettledAt: now,
	})
	require.ErrorIs(t, err, stop)
	require.NoError(t, mock.ExpectationsWereMet())
}
