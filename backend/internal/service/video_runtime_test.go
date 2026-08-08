package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestVideoRuntimeDisabledNeverLeases(t *testing.T) {
	repo := &fakeVideoRuntimeRepository{}
	runtime := NewVideoRuntime(repo, &fakeVideoTaskProcessor{}, nil, config.VideoConfig{})
	runtime.Start()
	runtime.Stop()
	require.Zero(t, repo.leaseCount())
}

func TestVideoRuntimeStopBeforeStartPreventsLateWorkerStart(t *testing.T) {
	repo := &fakeVideoRuntimeRepository{tasks: []VideoTask{{RequestID: "vid_0123456789abcdef0123456789abcdef"}}}
	runtime := NewVideoRuntime(repo, &fakeVideoTaskProcessor{}, nil, config.VideoConfig{Enabled: true, WorkerCount: 1, LeaseSeconds: 60, PollIntervalSeconds: 10})
	runtime.wait = blockingVideoRuntimeWait
	runtime.Stop()
	runtime.Start()
	time.Sleep(10 * time.Millisecond)
	require.Zero(t, repo.leaseCount())
}

func TestVideoRuntimeEnabledProcessesWithDistinctStableWorkerOwners(t *testing.T) {
	repo := &fakeVideoRuntimeRepository{tasks: []VideoTask{{RequestID: "vid_0123456789abcdef0123456789abcdef"}, {RequestID: "vid_abcdef0123456789abcdef0123456789"}}}
	processor := &fakeVideoTaskProcessor{processed: make(chan VideoTask, 2)}
	runtime := NewVideoRuntime(repo, processor, nil, config.VideoConfig{Enabled: true, WorkerCount: 2, LeaseSeconds: 60, PollIntervalSeconds: 10})
	runtime.wait = blockingVideoRuntimeWait
	runtime.ownerPrefix = "instance-test"
	runtime.Start()
	require.Eventually(t, func() bool { return processor.count() == 2 }, time.Second, time.Millisecond)
	runtime.Stop()

	owners := repo.ownersSnapshot()
	require.Len(t, owners, 2)
	require.NotEqual(t, owners[0], owners[1])
	require.Contains(t, owners[0], "instance-test-worker-")
	require.Contains(t, owners[1], "instance-test-worker-")
}

func TestVideoRuntimeProviderFlagsDoNotStopAcceptedTaskReconciliation(t *testing.T) {
	repo := &fakeVideoRuntimeRepository{tasks: []VideoTask{{RequestID: "vid_0123456789abcdef0123456789abcdef", Provider: VideoProviderSeedance}}}
	processor := &fakeVideoTaskProcessor{}
	runtime := NewVideoRuntime(repo, processor, nil, config.VideoConfig{Enabled: true, SeedanceEnabled: false, WorkerCount: 1, LeaseSeconds: 60, PollIntervalSeconds: 10})
	runtime.wait = blockingVideoRuntimeWait
	runtime.Start()
	require.Eventually(t, func() bool { return processor.count() == 1 }, time.Second, time.Millisecond)
	runtime.Stop()
}

func TestVideoRuntimeStopCancelsWorkersWaitsAndIsConcurrentIdempotent(t *testing.T) {
	started := make(chan struct{})
	processor := &fakeVideoTaskProcessor{process: func(ctx context.Context, _ VideoTask) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	repo := &fakeVideoRuntimeRepository{tasks: []VideoTask{{RequestID: "vid_0123456789abcdef0123456789abcdef"}}}
	runtime := NewVideoRuntime(repo, processor, nil, config.VideoConfig{Enabled: true, WorkerCount: 1, LeaseSeconds: 60, PollIntervalSeconds: 10})
	runtime.wait = blockingVideoRuntimeWait
	runtime.Start()
	<-started

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); runtime.Stop() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for and terminate workers")
	}
}

func TestVideoRuntimeRunsRetentionAndStopsItsLoop(t *testing.T) {
	retentionRepo := &fakeVideoRetentionRepository{}
	retention := NewVideoRetention(retentionRepo, config.VideoConfig{ResultMetadataRetentionDays: 30})
	retention.now = func() time.Time { return time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC) }
	runtime := NewVideoRuntime(&fakeVideoRuntimeRepository{}, &fakeVideoTaskProcessor{}, retention, config.VideoConfig{Enabled: true, WorkerCount: 1, LeaseSeconds: 60, PollIntervalSeconds: 10})
	runtime.wait = blockingVideoRuntimeWait
	runtime.retentionWait = blockingVideoRuntimeWait
	runtime.Start()
	require.Eventually(t, func() bool { return retentionRepo.callCount() == 1 }, time.Second, time.Millisecond)
	runtime.Stop()
	require.Equal(t, 1, retentionRepo.callCount())
}

type fakeVideoRuntimeRepository struct {
	mu     sync.Mutex
	tasks  []VideoTask
	owners []string
}

func (r *fakeVideoRuntimeRepository) LeaseDue(_ context.Context, owner string, limit int, _ time.Duration, _ time.Time) ([]VideoTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners = append(r.owners, owner)
	if len(r.tasks) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > len(r.tasks) {
		limit = len(r.tasks)
	}
	result := append([]VideoTask(nil), r.tasks[:limit]...)
	r.tasks = r.tasks[limit:]
	return result, nil
}

func (r *fakeVideoRuntimeRepository) leaseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.owners)
}

func (r *fakeVideoRuntimeRepository) ownersSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.owners...)
}

type fakeVideoTaskProcessor struct {
	mu        sync.Mutex
	processed chan VideoTask
	process   func(context.Context, VideoTask) error
	calls     int
}

func (p *fakeVideoTaskProcessor) Process(ctx context.Context, task VideoTask) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.process != nil {
		return p.process(ctx, task)
	}
	if p.processed != nil {
		p.processed <- task
	}
	return nil
}

func (p *fakeVideoTaskProcessor) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func blockingVideoRuntimeWait(ctx context.Context, _ time.Duration) bool {
	<-ctx.Done()
	return false
}
