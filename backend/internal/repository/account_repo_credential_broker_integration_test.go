//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRepositoryBrokerCAS(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "broker-cas", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "old-at", "refresh_token": "old-rt", "chatgpt_account_id": "upstream-id", "model_mapping": map[string]any{"original": "model"}}})
	expected := a.Credentials
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET credentials = credentials || '{"model_mapping":{"edited":"preserved"}}'::jsonb WHERE id=$1`, a.ID)
	require.NoError(t, err)
	ok, err := repo.UpdateOpenAIBrokerCredentialsIfUnchanged(ctx, a.ID, expected, map[string]any{"access_token": "new-at", "refresh_token": "new-rt", "model_mapping": map[string]any{"stale": "bad"}})
	require.NoError(t, err)
	require.True(t, ok)
	current, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "new-rt", current.GetCredential("refresh_token"))
	require.Equal(t, map[string]any{"edited": "preserved"}, current.Credentials["model_mapping"])
	ok, err = repo.UpdateOpenAIBrokerCredentialsIfUnchanged(ctx, a.ID, expected, map[string]any{"refresh_token": "stale-rt"})
	require.NoError(t, err)
	require.False(t, ok)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra = '{"home_credential_id":"rebound"}'::jsonb WHERE id=$1`, a.ID)
	require.NoError(t, err)
	ok, err = repo.UpdateOpenAIBrokerCredentialsIfUnchanged(ctx, a.ID, current.Credentials, map[string]any{"refresh_token": "must-not-write"})
	require.NoError(t, err)
	require.False(t, ok)
}

func TestAccountRepositoryBrokerCASOutboxFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "broker-cas-outbox", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "old-at", "refresh_token": "old-rt"}})
	_, err := tx.ExecContext(ctx, "SAVEPOINT broker_outbox")
	require.NoError(t, err)
	failing := newAccountRepositoryWithSQL(tx.Client(), &failAtomicSchedulerOutboxSQLExecutor{sqlExecutor: tx}, nil)
	ok, err := failing.UpdateOpenAIBrokerCredentialsIfUnchanged(ctx, a.ID, a.Credentials, map[string]any{"access_token": "new-at", "refresh_token": "new-rt"})
	require.Error(t, err)
	require.False(t, ok)
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT broker_outbox")
	require.NoError(t, err)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	current, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "old-rt", current.GetCredential("refresh_token"))
}
