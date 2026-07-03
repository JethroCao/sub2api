//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type channelMonitorRepoStub struct {
	monitor       *ChannelMonitor
	historyWrites int
	markWrites    int
}

func (r *channelMonitorRepoStub) Create(context.Context, *ChannelMonitor) error { return nil }
func (r *channelMonitorRepoStub) Update(context.Context, *ChannelMonitor) error { return nil }
func (r *channelMonitorRepoStub) Delete(context.Context, int64) error           { return nil }
func (r *channelMonitorRepoStub) List(context.Context, ChannelMonitorListParams) ([]*ChannelMonitor, int64, error) {
	return nil, 0, nil
}
func (r *channelMonitorRepoStub) ListEnabled(context.Context) ([]*ChannelMonitor, error) {
	if r.monitor == nil || !r.monitor.Enabled {
		return nil, nil
	}
	return []*ChannelMonitor{r.monitor}, nil
}
func (r *channelMonitorRepoStub) GetByID(context.Context, int64) (*ChannelMonitor, error) {
	if r.monitor == nil {
		return nil, ErrChannelMonitorNotFound
	}
	copy := *r.monitor
	return &copy, nil
}
func (r *channelMonitorRepoStub) MarkChecked(context.Context, int64, time.Time) error {
	r.markWrites++
	return nil
}
func (r *channelMonitorRepoStub) InsertHistoryBatch(context.Context, []*ChannelMonitorHistoryRow) error {
	r.historyWrites++
	return nil
}
func (r *channelMonitorRepoStub) DeleteHistoryBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (r *channelMonitorRepoStub) ListHistory(context.Context, int64, string, int) ([]*ChannelMonitorHistoryEntry, error) {
	return nil, nil
}
func (r *channelMonitorRepoStub) ListLatestPerModel(context.Context, int64) ([]*ChannelMonitorLatest, error) {
	return nil, nil
}
func (r *channelMonitorRepoStub) ComputeAvailability(context.Context, int64, int) ([]*ChannelMonitorAvailability, error) {
	return nil, nil
}
func (r *channelMonitorRepoStub) ListLatestForMonitorIDs(context.Context, []int64) (map[int64][]*ChannelMonitorLatest, error) {
	return nil, nil
}
func (r *channelMonitorRepoStub) ComputeAvailabilityForMonitors(context.Context, []int64, int) (map[int64][]*ChannelMonitorAvailability, error) {
	return nil, nil
}
func (r *channelMonitorRepoStub) ListRecentHistoryForMonitors(context.Context, []int64, map[int64]string, int) (map[int64][]*ChannelMonitorHistoryEntry, error) {
	return nil, nil
}
func (r *channelMonitorRepoStub) UpsertDailyRollupsFor(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (r *channelMonitorRepoStub) DeleteRollupsBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (r *channelMonitorRepoStub) LoadAggregationWatermark(context.Context) (*time.Time, error) {
	return nil, nil
}
func (r *channelMonitorRepoStub) UpdateAggregationWatermark(context.Context, time.Time) error {
	return nil
}

func TestChannelMonitorService_RunScheduledCheckRejectsDisabledMonitor(t *testing.T) {
	repo := &channelMonitorRepoStub{monitor: &ChannelMonitor{
		ID:              42,
		Enabled:         false,
		Provider:        MonitorProviderOpenAI,
		Endpoint:        "https://api.example.com",
		APIKey:          "ENC:test-key",
		PrimaryModel:    "gpt-test",
		IntervalSeconds: 60,
	}}
	svc := NewChannelMonitorService(repo, &plainEncryptor{})

	_, err := svc.RunScheduledCheck(context.Background(), 42)

	require.ErrorIs(t, err, ErrChannelMonitorDisabled)
	require.Zero(t, repo.historyWrites, "disabled scheduled checks must not write history")
	require.Zero(t, repo.markWrites, "disabled scheduled checks must not update last_checked_at")
}

func TestChannelMonitorService_RunCheckDoesNotRejectDisabledManualCheck(t *testing.T) {
	repo := &channelMonitorRepoStub{monitor: &ChannelMonitor{
		ID:              42,
		Enabled:         false,
		Provider:        MonitorProviderOpenAI,
		Endpoint:        "https://api.example.com",
		APIKey:          "legacy-plaintext-key",
		PrimaryModel:    "gpt-test",
		IntervalSeconds: 60,
	}}
	svc := NewChannelMonitorService(repo, &plainEncryptor{})

	_, err := svc.RunCheck(context.Background(), 42)

	require.ErrorIs(t, err, ErrChannelMonitorAPIKeyDecryptFailed)
	require.NotErrorIs(t, err, ErrChannelMonitorDisabled)
	require.Zero(t, repo.historyWrites)
	require.Zero(t, repo.markWrites)
}
