package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const videoRetentionInterval = 24 * time.Hour

type VideoRuntimeRepository interface {
	LeaseDue(context.Context, string, int, time.Duration, time.Time) ([]VideoTask, error)
}

type VideoTaskProcessor interface {
	Process(context.Context, VideoTask) error
}

type videoRuntimeWait func(context.Context, time.Duration) bool

// VideoRuntime is deliberately replica-local. PostgreSQL leases are the only
// cross-replica coordinator, so every application instance may start one.
type VideoRuntime struct {
	repo          VideoRuntimeRepository
	processor     VideoTaskProcessor
	retention     *VideoRetention
	cfg           config.VideoConfig
	now           func() time.Time
	wait          videoRuntimeWait
	retentionWait videoRuntimeWait
	ownerPrefix   string

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

var videoRuntimeSequence atomic.Uint64

func NewVideoRuntime(repo VideoRuntimeRepository, processor VideoTaskProcessor, retention *VideoRetention, cfg config.VideoConfig) *VideoRuntime {
	sequence := videoRuntimeSequence.Add(1)
	return &VideoRuntime{
		repo: repo, processor: processor, retention: retention, cfg: cfg,
		now: time.Now, wait: waitForVideoRuntime, retentionWait: waitForVideoRuntime,
		ownerPrefix: fmt.Sprintf("video-%d-%d", time.Now().UnixNano(), sequence),
	}
}

func ProvideVideoRuntime(repo VideoTaskRepository, reconciler *VideoReconciler, retention *VideoRetention, cfg *config.Config) *VideoRuntime {
	videoCfg := config.VideoConfig{}
	if cfg != nil {
		videoCfg = cfg.Video
	}
	runtime := NewVideoRuntime(repo, reconciler, retention, videoCfg)
	runtime.Start()
	return runtime
}

func (r *VideoRuntime) Start() {
	if r == nil || !r.cfg.Enabled || r.cfg.WorkerCount <= 0 || r.repo == nil || r.processor == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil || r.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done

	var workers sync.WaitGroup
	workers.Add(r.cfg.WorkerCount)
	for index := 0; index < r.cfg.WorkerCount; index++ {
		owner := fmt.Sprintf("%s-worker-%d", r.ownerPrefix, index)
		go func() {
			defer workers.Done()
			r.runWorker(ctx, owner)
		}()
	}
	if r.retention != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			r.runRetention(ctx)
		}()
	}
	go func() {
		workers.Wait()
		close(done)
	}()
}

func (r *VideoRuntime) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopped = true
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

func (r *VideoRuntime) runWorker(ctx context.Context, owner string) {
	interval := time.Duration(r.cfg.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	lease := time.Duration(r.cfg.LeaseSeconds) * time.Second
	if lease <= 0 {
		lease = time.Minute
	}
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		if r.now != nil {
			now = r.now().UTC()
		}
		tasks, err := r.repo.LeaseDue(ctx, owner, 1, lease, now)
		if err != nil && ctx.Err() == nil {
			log.Printf("[VideoRuntime] lease owner=%s failed: %v", owner, err)
		}
		for _, task := range tasks {
			if err := r.processor.Process(ctx, task); err != nil && ctx.Err() == nil {
				log.Printf("[VideoRuntime] reconcile request_id=%s failed: %v", task.RequestID, err)
			}
		}
		if !r.wait(ctx, interval) {
			return
		}
	}
}

func (r *VideoRuntime) runRetention(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := r.retention.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[VideoRuntime] retention failed: %v", err)
		}
		if !r.retentionWait(ctx, videoRetentionInterval) {
			return
		}
	}
}

func waitForVideoRuntime(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		duration = time.Second
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
