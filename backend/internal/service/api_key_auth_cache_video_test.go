package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotVideoPermissionRoundtrip(t *testing.T) {
	groupID := int64(50)
	apiKey := &APIKey{
		ID:      82,
		UserID:  40,
		GroupID: &groupID,
		Key:     "video-auth-roundtrip",
		Status:  StatusActive,
		User: &User{
			ID:          40,
			Status:      StatusActive,
			Concurrency: 5,
		},
		Group: &Group{
			ID:                   groupID,
			Name:                 "video",
			Platform:             PlatformVideo,
			Status:               StatusActive,
			Hydrated:             true,
			SubscriptionType:     SubscriptionTypeStandard,
			RateMultiplier:       1,
			AllowVideoGeneration: true,
		},
	}
	svc := &APIKeyService{}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.Equal(t, 20, snapshot.Version, "v20 refreshes snapshots with video permission and Grok billing fields")
	require.True(t, snapshot.Group.AllowVideoGeneration)

	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	require.NotNil(t, materialized.Group)
	require.True(t, materialized.Group.AllowVideoGeneration)
}
