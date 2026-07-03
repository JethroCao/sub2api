package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type scheduledTestPlanRepoStub struct {
	listDueCalls atomic.Int64
}

func (r *scheduledTestPlanRepoStub) Create(context.Context, *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepoStub) GetByID(context.Context, int64) (*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepoStub) ListByAccountID(context.Context, int64) ([]*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepoStub) ListDue(context.Context, time.Time) ([]*ScheduledTestPlan, error) {
	r.listDueCalls.Add(1)
	return nil, nil
}

func (r *scheduledTestPlanRepoStub) Update(context.Context, *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepoStub) Delete(context.Context, int64) error {
	return nil
}

func (r *scheduledTestPlanRepoStub) UpdateAfterRun(context.Context, int64, time.Time, time.Time) error {
	return nil
}

func TestScheduledTestRunner_SkipsScanWhenNotLeader(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	_, _ = cache.TryAcquireLeaderLock(context.Background(), scheduledTestRunnerLeaderLockKey, "peer", time.Minute)

	repo := &scheduledTestPlanRepoStub{}
	runner := NewScheduledTestRunnerService(repo, nil, nil, nil, &config.Config{})
	runner.SetLeaderLock(cache, nil)

	runner.runScheduledCycle(0)

	require.Zero(t, repo.listDueCalls.Load(), "non-leader must not scan due scheduled tests")
}

func TestScheduledTestRunner_SkipsScanWhenLeaderLockErrors(t *testing.T) {
	repo := &scheduledTestPlanRepoStub{}
	runner := NewScheduledTestRunnerService(repo, nil, nil, nil, &config.Config{})
	runner.SetLeaderLock(&fakeLeaderLockCache{acquireErr: context.DeadlineExceeded}, nil)

	runner.runScheduledCycle(0)

	require.Zero(t, repo.listDueCalls.Load(), "lock errors must not run scheduled test scan ungated")
}

func TestScheduledTestRunner_ScansWhenLeader(t *testing.T) {
	repo := &scheduledTestPlanRepoStub{}
	runner := NewScheduledTestRunnerService(repo, nil, nil, nil, &config.Config{})
	runner.SetLeaderLock(&fakeLeaderLockCache{}, nil)

	runner.runScheduledCycle(0)

	require.Equal(t, int64(1), repo.listDueCalls.Load(), "leader should scan due scheduled tests once")
}

type recordingLeaderLockCache struct {
	fakeLeaderLockCache
	acquireCalled chan struct{}
}

func (c *recordingLeaderLockCache) TryAcquireLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	select {
	case <-c.acquireCalled:
	default:
		close(c.acquireCalled)
	}
	return c.fakeLeaderLockCache.TryAcquireLeaderLock(ctx, key, owner, ttl)
}

func TestScheduledTestRunner_DelaysBeforeTryingLeaderLock(t *testing.T) {
	repo := &scheduledTestPlanRepoStub{}
	cache := &recordingLeaderLockCache{acquireCalled: make(chan struct{})}
	runner := NewScheduledTestRunnerService(repo, nil, nil, nil, &config.Config{})
	runner.SetLeaderLock(cache, nil)

	done := make(chan struct{})
	go func() {
		runner.runScheduledCycle(40 * time.Millisecond)
		close(done)
	}()

	select {
	case <-cache.acquireCalled:
		t.Fatal("leader lock should not be acquired before the scheduled delay")
	case <-time.After(15 * time.Millisecond):
	}

	select {
	case <-cache.acquireCalled:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("leader lock was not acquired after the scheduled delay")
	}

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("runScheduledCycle did not finish")
	}
	require.Equal(t, int64(1), repo.listDueCalls.Load())
}
