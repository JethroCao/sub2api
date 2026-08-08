package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestVideoTaskRepositoryLeaseDueSeparatesBusinessAndDatabaseClocks(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	businessNow := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	stop := errors.New("query contract verified")
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)WITH db_clock AS MATERIALIZED \(SELECT clock_timestamp\(\) AS now\),.*AND \(lease_expires_at IS NULL OR lease_expires_at <= db_clock\.now\).*SET lease_owner = \$1, lease_expires_at = db_clock\.now \+ \$4::bigint \* INTERVAL '1 microsecond', updated_at = \$3.*FROM due, db_clock`).
		WithArgs("worker-db-clock", 50, businessNow, time.Minute.Microseconds()).
		WillReturnError(stop)
	mock.ExpectRollback()

	repo := NewVideoTaskRepository(db)
	_, err = repo.LeaseDue(context.Background(), "worker-db-clock", 50, time.Minute, businessNow)
	require.ErrorIs(t, err, stop)
	require.NoError(t, mock.ExpectationsWereMet())
}
