package repository

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBrokerCredentialCASPreservesMetadataAndFencesOwnership(t *testing.T) {
	for _, affected := range []int64{0, 1} {
		t.Run(string(rune('0'+affected)), func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(affected)}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)
			applied, err := repo.UpdateOpenAIBrokerCredentialsIfUnchanged(context.Background(), 4, map[string]any{"access_token": "old-at", "refresh_token": "old-rt"}, map[string]any{"access_token": "new-at", "refresh_token": "new-rt", "_token_version": int64(7), "model_mapping": map[string]any{"old": "must-not-overwrite"}})
			require.NoError(t, err)
			require.Equal(t, affected == 1, applied)
			require.Len(t, exec.execQueries, 1)
			sql := normalizeSQLWhitespace(exec.execQueries[0])
			require.Contains(t, sql, "credentials = COALESCE(a.credentials, '{}'::jsonb) || $1::jsonb")
			require.Contains(t, sql, "jsonb_build_object")
			require.Contains(t, sql, "= $5::jsonb")
			require.Contains(t, sql, "parent_account_id IS NULL")
			require.Contains(t, sql, "home_credential_id")
			require.Contains(t, sql, "INSERT INTO scheduler_outbox")
			var update, expected map[string]any
			require.NoError(t, json.Unmarshal([]byte(exec.execArgs[0][0].(string)), &update))
			require.NoError(t, json.Unmarshal([]byte(exec.execArgs[0][4].(string)), &expected))
			require.NotContains(t, update, "model_mapping")
			require.Equal(t, "new-rt", update["refresh_token"])
			require.Equal(t, "old-rt", expected["refresh_token"])
			require.Contains(t, expected, "client_id")
			require.Nil(t, expected["client_id"])
		})
	}
}
