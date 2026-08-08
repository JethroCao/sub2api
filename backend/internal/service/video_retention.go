package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const videoRetentionBatchSize = 200

type VideoRetentionRepository interface {
	ClearExpiredMetadata(context.Context, ClearVideoTaskMetadataParams) (int, error)
}

// VideoRetention removes only temporary delivery metadata and the minimized
// request snapshot. Task, settlement, billing and event rows remain durable.
type VideoRetention struct {
	repo VideoRetentionRepository
	cfg  config.VideoConfig
	now  func() time.Time
}

func NewVideoRetention(repo VideoRetentionRepository, cfg config.VideoConfig) *VideoRetention {
	return &VideoRetention{repo: repo, cfg: cfg, now: time.Now}
}

func (r *VideoRetention) RunOnce(ctx context.Context) (int, error) {
	if r == nil || r.repo == nil {
		return 0, ErrVideoServiceUnavailable
	}
	if r.cfg.ResultMetadataRetentionDays <= 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	if r.now != nil {
		now = r.now().UTC()
	}
	return r.repo.ClearExpiredMetadata(ctx, ClearVideoTaskMetadataParams{
		Now:             now,
		RetentionBefore: now.AddDate(0, 0, -r.cfg.ResultMetadataRetentionDays),
		Limit:           videoRetentionBatchSize,
	})
}
