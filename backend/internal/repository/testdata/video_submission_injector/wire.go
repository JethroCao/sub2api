//go:build wireinject

package video_submission_injector

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/wire"
)

func initializeVideoSubmissionRepository(cfg *config.Config) (service.VideoSubmissionRepository, error) {
	wire.Build(repository.ProviderSet)
	return nil, nil
}
