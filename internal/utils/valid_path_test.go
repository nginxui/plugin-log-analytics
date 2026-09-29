package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listLogs(t *testing.T, logs ...HostLog) {
	t.Helper()
	SetHostLogs(logs)
	t.Cleanup(func() { SetHostLogs(nil) })
}

func TestIsValidLogPathOnlyAllowsListedPaths(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.log")
	require.NoError(t, os.WriteFile(access, []byte("x\n"), 0o644))
	other := filepath.Join(dir, "other.log")
	require.NoError(t, os.WriteFile(other, []byte("x\n"), 0o644))

	listLogs(t, HostLog{Path: access, Type: "access", Source: "config"})

	assert.True(t, IsValidLogPath(access))
	assert.False(t, IsValidLogPath(other), "a file next to a listed log is not listed itself")
	assert.False(t, IsValidLogPath(""))
	assert.False(t, IsValidLogPath("access.log"), "relative paths never pass")
	assert.False(t, IsValidLogPath(filepath.Join(dir, "sub", "..", "..", "etc", "passwd")))
}

func TestIsValidLogPathAllowsRotatedSiblings(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.log")
	listLogs(t, HostLog{Path: access, Type: "access", Source: "default"})

	for _, name := range []string{"access.log.1", "access.log.2.gz", "access.log-20240101", "access.log.20240101"} {
		rotated := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(rotated, []byte("x\n"), 0o644))
		assert.True(t, IsValidLogPath(rotated), name)
	}

	stray := filepath.Join(dir, "access.log.evil")
	require.NoError(t, os.WriteFile(stray, []byte("x\n"), 0o644))
	assert.False(t, IsValidLogPath(stray))
}

func TestIsValidLogPathAcceptsMissingListedFile(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "later.log")
	listLogs(t, HostLog{Path: access, Type: "access", Source: "config"})

	assert.True(t, IsValidLogPath(access))
}

func TestIsValidLogPathRejectsDirectoriesAndDevices(t *testing.T) {
	dir := t.TempDir()
	listLogs(t, HostLog{Path: dir, Type: "access", Source: "config"})
	assert.False(t, IsValidLogPath(dir), "a directory is not a log file")

	if _, err := os.Stat("/dev/null"); err == nil {
		listLogs(t, HostLog{Path: "/dev/null", Type: "access", Source: "config"})
		assert.False(t, IsValidLogPath("/dev/null"))
	}
}

func TestIsValidLogPathSymlinks(t *testing.T) {
	dir := t.TempDir()
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "real.log")
	require.NoError(t, os.WriteFile(target, []byte("x\n"), 0o644))

	access := filepath.Join(dir, "access.log")
	require.NoError(t, os.Symlink(target, access))
	rotated := filepath.Join(dir, "access.log.1")
	require.NoError(t, os.Symlink(target, rotated))

	listLogs(t, HostLog{Path: access, Type: "access", Source: "config"})

	assert.True(t, IsValidLogPath(access), "the host vetted the listed symlink")
	assert.False(t, IsValidLogPath(rotated), "a rotated symlink must stay in the log directory")

	local := filepath.Join(dir, "access.log.2")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real2.log"), []byte("x\n"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(dir, "real2.log"), local))
	assert.True(t, IsValidLogPath(local))

	dirLink := filepath.Join(dir, "linkdir")
	require.NoError(t, os.Symlink(elsewhere, dirLink))
	listLogs(t, HostLog{Path: dirLink, Type: "access", Source: "config"})
	assert.False(t, IsValidLogPath(dirLink), "a symlink to a directory is not a log file")
}

func TestDefaultAccessLogPath(t *testing.T) {
	assert.Empty(t, DefaultAccessLogPath())

	listLogs(t,
		HostLog{Path: "/var/log/nginx/site.log", Type: "access", Source: "config"},
		HostLog{Path: "/var/log/nginx/error.log", Type: "error", Source: "default"},
		HostLog{Path: "/var/log/nginx/access.log", Type: "access", Source: "default"},
	)
	assert.Equal(t, "/var/log/nginx/access.log", DefaultAccessLogPath())
}
