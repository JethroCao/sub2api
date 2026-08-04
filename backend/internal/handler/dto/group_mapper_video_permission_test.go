package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupMappersIncludeVideoGenerationPermission(t *testing.T) {
	group := &service.Group{AllowVideoGeneration: true}

	user := GroupFromService(group)
	admin := GroupFromServiceAdmin(group)

	require.True(t, user.AllowVideoGeneration)
	require.True(t, admin.AllowVideoGeneration)
}
