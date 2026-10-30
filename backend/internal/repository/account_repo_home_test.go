package repository

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestHomeRepositoryRejectsStaleFullRowBindingRemoval(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).WillReturnRows(sqlmock.NewRows(openCodeGoMergeMockColumns()).AddRow(false, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, `{"openai_credential_source":"home","home_credential_id":"eb1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}`))
	a := &service.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "stale"}}
	_, err := lockAndMergeAccountProbeExtra(context.Background(), client, a, nil, nil)
	require.ErrorIs(t, err, service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestHomeRepositoryCredentialsWriteHasAtomicOwnershipPredicate(t *testing.T) {
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	repo := &accountRepository{client: client}
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*WHERE id = \$2 AND deleted_at IS NULL.*AND NOT.*openai_credential_source.*home_credential_id`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()
	err := repo.UpdateCredentials(context.Background(), 7, map[string]any{"refresh_token": "stale"})
	require.ErrorIs(t, err, service.ErrHomeOwnershipChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}
