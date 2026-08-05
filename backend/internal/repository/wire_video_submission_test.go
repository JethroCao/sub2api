package repository

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepositoryProviderSetBuildsVideoSubmissionRepositoryInjector(t *testing.T) {
	backendRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	cmd := exec.Command("go", "run", "-mod=mod", "github.com/google/wire/cmd/wire", "check", "./internal/repository/testdata/video_submission_injector")
	cmd.Dir = backendRoot
	cmd.Env = append(os.Environ(), "GOCACHE="+t.TempDir())
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}
