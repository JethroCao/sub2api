package repository

import (
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyVideoProjectionMapsPermission(t *testing.T) {
	group := groupEntityToService(&dbent.Group{AllowVideoGeneration: true})

	require.NotNil(t, group)
	require.True(t, group.AllowVideoGeneration)
}
