package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// UpdateOpenAIBrokerCredentialsIfUnchanged fences refresh results against
// reauthorization/ownership changes, while preserving concurrently edited metadata.
// The outbox write is atomic with credential persistence.
func (r *accountRepository) UpdateOpenAIBrokerCredentialsIfUnchanged(ctx context.Context, id int64, expected, credentials map[string]any) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	expectedJSON, err := json.Marshal(service.CredentialBrokerAuthSnapshot(expected))
	if err != nil {
		return false, err
	}
	updateJSON, err := json.Marshal(service.CredentialBrokerAuthUpdate(credentials))
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET credentials = COALESCE(a.credentials, '{}'::jsonb) || $1::jsonb,
			updated_at = NOW()
		WHERE a.id = $2 AND a.deleted_at IS NULL
			AND a.platform = $3 AND a.type = $4
			AND a.parent_account_id IS NULL
			AND NOT (COALESCE(a.extra, '{}'::jsonb) ?| ARRAY['openai_credential_source','home_credential_id'])
			AND jsonb_build_object(
				'access_token', a.credentials -> 'access_token',
				'refresh_token', a.credentials -> 'refresh_token',
				'id_token', a.credentials -> 'id_token',
				'client_id', a.credentials -> 'client_id',
				'chatgpt_account_id', a.credentials -> 'chatgpt_account_id',
				'chatgpt_user_id', a.credentials -> 'chatgpt_user_id',
				'organization_id', a.credentials -> 'organization_id',
				'auth_mode', a.credentials -> 'auth_mode',
				'openai_auth_mode', a.credentials -> 'openai_auth_mode'
			) = $5::jsonb
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $6, updated.id, NULL, NULL FROM updated
	`, string(updateJSON), id, service.PlatformOpenAI, service.AccountTypeOAuth, string(expectedJSON), service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}
