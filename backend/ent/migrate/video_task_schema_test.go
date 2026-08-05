package migrate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnifiedVideoLedgerTablesAreOwnedByVersionedMigration(t *testing.T) {
	names := make([]string, 0, len(Tables))
	for _, table := range Tables {
		names = append(names, table.Name)
	}
	require.NotContains(t, names, "video_tasks")
	require.NotContains(t, names, "video_task_events")
}
