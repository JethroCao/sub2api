package service

import (
	"context"
	"time"
)

type VideoSubmissionRepository interface {
	CreateTaskAndReserve(context.Context, CreateVideoTaskParams) (*VideoTask, bool, error)
}

type VideoTaskRepository interface {
	VideoSubmissionRepository
	CreateOrGet(context.Context, CreateVideoTaskParams) (*VideoTask, bool, error)
	GetOwned(context.Context, string, int64, int64) (*VideoTask, error)
	GetByRequestID(context.Context, string) (*VideoTask, error)
	AssignAndMarkSubmitting(context.Context, AssignVideoSubmissionParams) error
	MarkSubmitting(context.Context, string, int64, string) error
	MarkSubmitted(context.Context, MarkVideoSubmittedParams) error
	MarkSubmissionUnknown(context.Context, string, int64, VideoTaskError) error
	MarkSubmissionUnknownAt(context.Context, MarkVideoSubmissionUnknownParams) error
	ReleaseAndMarkSubmissionFailed(context.Context, ReleaseAndFailVideoSubmissionParams) (*VideoTask, error)
	LeaseDue(context.Context, string, int, time.Duration, time.Time) ([]VideoTask, error)
	RenewLease(context.Context, RenewVideoTaskLeaseParams) error
	ApplyPollResult(context.Context, ApplyVideoPollResultParams) (*VideoTask, error)
	ApplyRecoveredSubmission(context.Context, RecoverVideoSubmissionParams) (*VideoTask, error)
	ScheduleRetry(context.Context, ScheduleVideoTaskRetryParams) error
	MarkSettled(context.Context, MarkVideoSettledParams) error
	ReleaseLease(context.Context, string, string, time.Time) error
	ClearExpiredMetadata(context.Context, ClearVideoTaskMetadataParams) (int, error)
	AppendEvent(context.Context, VideoTaskEvent) error
	ListAdmin(context.Context, VideoTaskListQuery) ([]VideoTask, int, error)
}
