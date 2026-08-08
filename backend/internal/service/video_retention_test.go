package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestVideoRetentionClearsOnlyExpiredMetadataAtConfiguredCutoff(t *testing.T) {
	repo := &fakeVideoRetentionRepository{cleared: 3}
	retention := NewVideoRetention(repo, config.VideoConfig{ResultMetadataRetentionDays: 30})
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	retention.now = func() time.Time { return now }

	cleared, err := retention.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, cleared)
	require.WithinDuration(t, now, repo.params.Now, time.Nanosecond)
	require.WithinDuration(t, now.AddDate(0, 0, -30), repo.params.RetentionBefore, time.Nanosecond)
	require.Equal(t, 200, repo.params.Limit)
}

func TestVideoRetentionDisabledOrInvalidDependencyIsSafe(t *testing.T) {
	retention := NewVideoRetention(&fakeVideoRetentionRepository{}, config.VideoConfig{})
	cleared, err := retention.RunOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, cleared)

	retention = NewVideoRetention(nil, config.VideoConfig{ResultMetadataRetentionDays: 30})
	_, err = retention.RunOnce(context.Background())
	require.ErrorIs(t, err, ErrVideoServiceUnavailable)
}

type fakeVideoRetentionRepository struct {
	mu      sync.Mutex
	params  ClearVideoTaskMetadataParams
	calls   int
	cleared int
	err     error
}

func (r *fakeVideoRetentionRepository) ClearExpiredMetadata(_ context.Context, params ClearVideoTaskMetadataParams) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.params = params
	return r.cleared, r.err
}

func (r *fakeVideoRetentionRepository) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}
