package cmd

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/projects"
	"github.com/stretchr/testify/require"
)

func TestGatherStatsFromProjectsReadsSharedDataDirOnce(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)

	// Two checkouts point at the same central database, a third has its
	// own. The shared database must contribute its sessions only once.
	central := filepath.Join(tmp, "central")
	_, centralSessions := newProjectDB(t, central)
	_, err := centralSessions.Create(t.Context(), "shared one")
	require.NoError(t, err)
	_, err = centralSessions.Create(t.Context(), "shared two")
	require.NoError(t, err)

	own := filepath.Join(tmp, "solo", ".crush")
	_, ownSessions := newProjectDB(t, own)
	_, err = ownSessions.Create(t.Context(), "solo")
	require.NoError(t, err)

	require.NoError(t, projects.Register(filepath.Join(tmp, "checkout-a"), central))
	require.NoError(t, projects.Register(filepath.Join(tmp, "checkout-b"), filepath.Join(tmp, "..", filepath.Base(tmp), "central")))
	require.NoError(t, projects.Register(filepath.Join(tmp, "solo"), own))

	stats, err := gatherStatsFromProjects(t.Context())
	require.NoError(t, err)
	require.Len(t, stats, 2)

	merged := mergeStats(stats)
	require.Equal(t, int64(3), merged.Total.TotalSessions)
}
