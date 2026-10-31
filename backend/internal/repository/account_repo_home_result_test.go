package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const resultHomeCredentialID = "eb1b3cb1-3de2-47a4-b833-2b8e6644ad6e"

func homeResultMutations() map[string]func(context.Context, *accountRepository) error {
	until := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	return map[string]func(context.Context, *accountRepository) error{
		"error":      func(ctx context.Context, r *accountRepository) error { return r.SetError(ctx, 7, "revoked") },
		"rate_limit": func(ctx context.Context, r *accountRepository) error { return r.SetRateLimited(ctx, 7, until) },
		"temporary_block": func(ctx context.Context, r *accountRepository) error {
			return r.SetTempUnschedulable(ctx, 7, until, "retry")
		},
		"model_limit": func(ctx context.Context, r *accountRepository) error {
			return r.SetModelRateLimit(ctx, 7, "image", until)
		},
		"overload": func(ctx context.Context, r *accountRepository) error { return r.SetOverloaded(ctx, 7, until) },
		"extra": func(ctx context.Context, r *accountRepository) error {
			return r.UpdateExtra(ctx, 7, map[string]any{"codex_usage_updated_at": "2026-10-31T12:00:00Z"})
		},
		"session_window": func(ctx context.Context, r *accountRepository) error {
			return r.UpdateSessionWindow(ctx, 7, nil, &until, "rejected")
		},
		"session_window_end":    func(ctx context.Context, r *accountRepository) error { return r.UpdateSessionWindowEnd(ctx, 7, until) },
		"clear_error":           func(ctx context.Context, r *accountRepository) error { return r.ClearError(ctx, 7) },
		"clear_rate_limit":      func(ctx context.Context, r *accountRepository) error { return r.ClearRateLimit(ctx, 7) },
		"clear_temporary_block": func(ctx context.Context, r *accountRepository) error { return r.ClearTempUnschedulable(ctx, 7) },
		"clear_model_limits":    func(ctx context.Context, r *accountRepository) error { return r.ClearModelRateLimits(ctx, 7) },
		"schedulable":           func(ctx context.Context, r *accountRepository) error { return r.SetSchedulable(ctx, 7, true) },
	}
}

func expectHomeResultLock(mock sqlmock.Sqlmock, matches bool) {
	mock.ExpectQuery(homeResultOwnerLockSQL).
		WithArgs(int64(7), int64(7), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows().AddRow(7, nil, matches)).RowsWillBeClosed()
}

// A response can pass the service's earlier ownership read, then lose its
// binding before this repository call. The lock must span update and outbox.
func TestHomeResultMutationsRejectOwnershipChangedBeforeWrite(t *testing.T) {
	for name, mutate := range homeResultMutations() {
		t.Run(name, func(t *testing.T) {
			client, mock := newOllamaCloudUsageRepositoryTestClient(t)
			repo := newAccountRepositoryWithSQL(client, client, nil)
			mock.ExpectBegin()
			expectHomeResultLock(mock, false)
			mock.ExpectRollback()
			err := mutate(service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID), repo)
			require.ErrorIs(t, err, service.ErrHomeOwnershipChanged)
			require.NoError(t, mock.ExpectationsWereMet(), "rejection must not update the replacement account or emit an outbox event")
		})
	}
}

func TestHomeResultMatchingBindingCommitsMutationAndOutboxTogether(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	mock.ExpectBegin()
	expectHomeResultLock(mock, true)
	mock.ExpectExec(`UPDATE "accounts" SET .*"status" = .*"error_message" = .*"schedulable" = .*WHERE "accounts"\."id" = `).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(7), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err := repo.SetError(service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID), 7, "revoked")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultSameBindingTemporaryBlockNoOpIsNotOwnershipFailure(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	mock.ExpectBegin()
	expectHomeResultLock(mock, true)
	mock.ExpectExec(`(?s)UPDATE accounts.*temp_unschedulable_until IS NULL OR temp_unschedulable_until < \$1`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	err := repo.SetTempUnschedulable(service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID), 7, time.Now(), "retry")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet(), "no-op must not enqueue an outbox update")
}

func TestHomeResultGuardUsesCallerTransactionWithoutCommittingIt(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	mock.ExpectBegin()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	ctx := dbent.NewTxContext(service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID), tx)
	expectHomeResultLock(mock, true)
	mock.ExpectExec(`(?s)UPDATE accounts.*temp_unschedulable_until IS NULL OR temp_unschedulable_until < \$1`).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, repo.SetTempUnschedulable(ctx, 7, time.Now(), "retry"))
	require.NoError(t, mock.ExpectationsWereMet())
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultGuardRollsBackFailedWrite(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	writeErr := errors.New("write failed")
	mock.ExpectBegin()
	expectHomeResultLock(mock, true)
	mock.ExpectExec(`UPDATE "accounts"`).WillReturnError(writeErr)
	mock.ExpectRollback()
	err := repo.SetRateLimited(service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID), 7, time.Now())
	require.ErrorIs(t, err, writeErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultGuardPreservesDeletedAccountNotFound(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	mock.ExpectBegin()
	mock.ExpectQuery(homeResultOwnerLockSQL).
		WithArgs(int64(7), int64(7), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows()).RowsWillBeClosed()
	mock.ExpectRollback()
	err := repo.SetError(service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID), 7, "revoked")
	require.ErrorIs(t, err, service.ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultGuardRejectsEmptyExpectedCredential(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	err := repo.SetError(service.WithHomeResultCredentialID(context.Background(), ""), 7, "revoked")
	require.ErrorIs(t, err, service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultMutationsPreserveNativeWritePath(t *testing.T) {
	for name, mutate := range homeResultMutations() {
		t.Run(name, func(t *testing.T) {
			client, mock := newOllamaCloudUsageRepositoryTestClient(t)
			repo := newAccountRepositoryWithSQL(client, client, nil)
			mock.ExpectExec(`UPDATE "?accounts"? SET`).WillReturnResult(sqlmock.NewResult(0, 1))
			if name != "extra" {
				mock.ExpectExec(`INSERT INTO scheduler_outbox`).WillReturnResult(sqlmock.NewResult(0, 1))
			}

			require.NoError(t, mutate(context.Background(), repo))
			require.NoError(t, mock.ExpectationsWereMet(), "native writes must not open a Home lock transaction")
		})
	}
}

func TestHomeResultMutationsPreserveMatchingBindingWritePath(t *testing.T) {
	for name, mutate := range homeResultMutations() {
		t.Run(name, func(t *testing.T) {
			client, mock := newOllamaCloudUsageRepositoryTestClient(t)
			repo := newAccountRepositoryWithSQL(client, client, nil)
			mock.ExpectBegin()
			expectHomeResultLock(mock, true)
			mock.ExpectExec(`UPDATE "?accounts"? SET`).WillReturnResult(sqlmock.NewResult(0, 1))
			if name != "extra" {
				mock.ExpectExec(`INSERT INTO scheduler_outbox`).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectCommit()

			require.NoError(t, mutate(service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID), repo))
			require.NoError(t, mock.ExpectationsWereMet(), "matching Home writes must update and enqueue on the locked transaction")
		})
	}
}

const homeResultOwnerLockSQL = `(?s)SELECT id, parent_account_id,.*extra\s*->>\s*'openai_credential_source'\s*=\s*'home'.*extra\s*->>\s*'home_credential_id'\s*=\s*\$3.*WHERE id IN \(\$1, \$2\) AND deleted_at IS NULL.*ORDER BY id.*FOR NO KEY UPDATE`

func homeResultOwnerRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "parent_account_id", "binding_matches"})
}

func TestHomeResultShadowModelLimitLocksOriginalOwnerAndPreservesTarget(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	ctx := service.WithHomeResultBinding(context.Background(), resultHomeCredentialID, 8, 7)
	mock.ExpectBegin()
	mock.ExpectQuery(homeResultOwnerLockSQL).WithArgs(int64(8), int64(7), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows().AddRow(7, nil, true).AddRow(8, 7, false)).RowsWillBeClosed()
	mock.ExpectExec(`(?s)UPDATE accounts SET.*model_rate_limits.*WHERE id = \$3 AND deleted_at IS NULL`).
		WithArgs("image", sqlmock.AnyArg(), int64(8)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(8), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.SetModelRateLimit(ctx, 8, "image", time.Now().Add(time.Minute)))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultShadowRejectsChangedOwnerOrRelationshipBeforeWrite(t *testing.T) {
	cases := []struct {
		name string
		rows func() *sqlmock.Rows
	}{
		{"owner_binding_changed", func() *sqlmock.Rows { return homeResultOwnerRows().AddRow(7, nil, false).AddRow(8, 7, false) }},
		{"owner_deleted", func() *sqlmock.Rows { return homeResultOwnerRows().AddRow(8, 7, false) }},
		{"owner_became_shadow", func() *sqlmock.Rows { return homeResultOwnerRows().AddRow(7, 6, true).AddRow(8, 7, false) }},
		// The replacement parent may carry the same UUID. The original parent
		// relationship must match; UUID equality alone must never authorize it.
		{"reparented_to_same_uuid_owner", func() *sqlmock.Rows { return homeResultOwnerRows().AddRow(7, nil, true).AddRow(8, 9, false) }},
		{"shadow_became_owner", func() *sqlmock.Rows { return homeResultOwnerRows().AddRow(7, nil, true).AddRow(8, nil, true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, mock := newOllamaCloudUsageRepositoryTestClient(t)
			repo := newAccountRepositoryWithSQL(client, client, nil)
			ctx := service.WithHomeResultBinding(context.Background(), resultHomeCredentialID, 8, 7)
			mock.ExpectBegin()
			mock.ExpectQuery(homeResultOwnerLockSQL).WithArgs(int64(8), int64(7), resultHomeCredentialID).
				WillReturnRows(tc.rows()).RowsWillBeClosed()
			mock.ExpectRollback()

			require.ErrorIs(t, repo.SetModelRateLimit(ctx, 8, "image", time.Now()), service.ErrHomeOwnershipChanged)
			require.NoError(t, mock.ExpectationsWereMet(), "a replaced owner/relationship must not mutate or enqueue state")
		})
	}
}

func TestHomeResultShadowOwnerWriteAlsoChecksSelectedRelationship(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	ctx := service.WithHomeResultBinding(context.Background(), resultHomeCredentialID, 8, 7)
	mock.ExpectBegin()
	mock.ExpectQuery(homeResultOwnerLockSQL).WithArgs(int64(8), int64(7), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows().AddRow(7, nil, true).AddRow(8, 9, false)).RowsWillBeClosed()
	mock.ExpectRollback()

	require.ErrorIs(t, repo.SetError(ctx, 7, "revoked"), service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultBindingCannotMutateAnUnrelatedAccount(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	ctx := service.WithHomeResultBinding(context.Background(), resultHomeCredentialID, 8, 7)

	require.ErrorIs(t, repo.SetError(ctx, 9, "revoked"), service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultUUIDOnlyContextCannotMutateAShadow(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	ctx := service.WithHomeResultCredentialID(context.Background(), resultHomeCredentialID)
	mock.ExpectBegin()
	mock.ExpectQuery(homeResultOwnerLockSQL).WithArgs(int64(8), int64(8), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows().AddRow(8, 7, true)).RowsWillBeClosed()
	mock.ExpectRollback()

	require.ErrorIs(t, repo.SetError(ctx, 8, "revoked"), service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultShadowParentWriteAcceptsMatchingRelationship(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	ctx := service.WithHomeResultBinding(context.Background(), resultHomeCredentialID, 8, 7)
	mock.ExpectBegin()
	mock.ExpectQuery(homeResultOwnerLockSQL).WithArgs(int64(8), int64(7), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows().AddRow(7, nil, true).AddRow(8, 7, false)).RowsWillBeClosed()
	mock.ExpectExec(`UPDATE "accounts" SET`).
		WithArgs(sqlmock.AnyArg(), service.StatusError, "revoked", false, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(7), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.SetError(ctx, 7, "revoked"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultShadowNoOpUsesCallerTransactionAndAscendingLocks(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	mock.ExpectBegin()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	// The owner can have the larger ID (e.g. after an authorized reparent).
	// Always order by row ID rather than assuming parent-before-child order.
	ctx := service.WithHomeResultBinding(dbent.NewTxContext(context.Background(), tx), resultHomeCredentialID, 7, 8)
	mock.ExpectQuery(homeResultOwnerLockSQL).WithArgs(int64(7), int64(8), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows().AddRow(7, 8, false).AddRow(8, nil, true)).RowsWillBeClosed()
	mock.ExpectExec(`(?s)UPDATE accounts.*temp_unschedulable_until IS NULL OR temp_unschedulable_until < \$1`).
		WithArgs(sqlmock.AnyArg(), "retry", int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))

	require.NoError(t, repo.SetTempUnschedulable(ctx, 7, time.Now(), "retry"))
	require.NoError(t, mock.ExpectationsWereMet(), "same-binding no-op must not enqueue or commit the caller transaction")
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultShadowDeletedSelectionPreventsParentWrite(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	ctx := service.WithHomeResultBinding(context.Background(), resultHomeCredentialID, 8, 7)
	mock.ExpectBegin()
	mock.ExpectQuery(homeResultOwnerLockSQL).WithArgs(int64(8), int64(7), resultHomeCredentialID).
		WillReturnRows(homeResultOwnerRows().AddRow(7, nil, true)).RowsWillBeClosed()
	mock.ExpectRollback()

	require.ErrorIs(t, repo.SetError(ctx, 7, "revoked"), service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHomeResultPartialBindingProvenanceIsRejected(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := newAccountRepositoryWithSQL(client, client, nil)
	ctx := service.WithHomeResultBinding(context.Background(), resultHomeCredentialID, 8, 0)

	require.ErrorIs(t, repo.SetError(ctx, 8, "revoked"), service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}
