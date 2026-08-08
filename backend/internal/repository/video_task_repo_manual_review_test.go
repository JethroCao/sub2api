package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoTaskRepositoryScheduleRetryAllowsAcceptedTaskManualReviewTransition(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	requestID := "vid_0123456789abcdef0123456789abcdef"
	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	mock.ExpectExec(`(?s)UPDATE video_tasks.*OR \(status IN \('submitting', 'submitted', 'queued', 'running'\) AND \$4 = 'unknown'\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewVideoTaskRepository(db)
	err = repo.ScheduleRetry(context.Background(), service.ScheduleVideoTaskRetryParams{
		RequestID: requestID, ExpectedVersion: 7, LeaseOwner: "worker-review",
		Status: service.VideoTaskUnknown, Error: service.NewVideoTaskError("POLL_ATTEMPTS_EXHAUSTED", "", false),
		NextPollAt: now.Add(time.Hour), UpdatedAt: now,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
