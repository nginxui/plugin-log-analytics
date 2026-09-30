package cgroup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useFixtureRoot points the cgroup readers at a temporary directory for the
// duration of a test.
func useFixtureRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	previous, previousProc, previousStatus := cgroupRoot, procSelfCgroup, procSelfStatus
	cgroupRoot = root
	procSelfCgroup = filepath.Join(root, "proc-self-cgroup-missing")
	procSelfStatus = filepath.Join(root, "proc-self-status-missing")
	t.Cleanup(func() { cgroupRoot, procSelfCgroup, procSelfStatus = previous, previousProc, previousStatus })

	return root
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestCPUQuotaCgroupV2(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "cpu.max"), "150000 100000\n")

	quota, ok := CPUQuota()
	require.True(t, ok)
	assert.InDelta(t, 1.5, quota, 0.0001)
}

func TestCPUQuotaCgroupV2Unlimited(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "cpu.max"), "max 100000\n")

	_, ok := CPUQuota()
	assert.False(t, ok)
}

func TestCPUQuotaCgroupV1(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "cpu", "cpu.cfs_quota_us"), "200000\n")
	writeFixture(t, filepath.Join(root, "cpu", "cpu.cfs_period_us"), "100000\n")

	quota, ok := CPUQuota()
	require.True(t, ok)
	assert.InDelta(t, 2.0, quota, 0.0001)
}

func TestCPUQuotaCgroupV1Unlimited(t *testing.T) {
	root := useFixtureRoot(t)
	// -1 is the kernel's "no bandwidth limit" sentinel.
	writeFixture(t, filepath.Join(root, "cpu", "cpu.cfs_quota_us"), "-1\n")
	writeFixture(t, filepath.Join(root, "cpu", "cpu.cfs_period_us"), "100000\n")

	_, ok := CPUQuota()
	assert.False(t, ok)
}

func TestCPUQuotaWithoutCgroupFilesystem(t *testing.T) {
	useFixtureRoot(t)

	_, ok := CPUQuota()
	assert.False(t, ok)
}

// TestAvailableCPUsClampedByQuota is the core regression guard for issue #1792:
// in an LXC container the affinity mask reports every host CPU, so worker pools
// sized from GOMAXPROCS oversubscribe the container by an order of magnitude.
func TestAvailableCPUsClampedByQuota(t *testing.T) {
	root := useFixtureRoot(t)
	// One core, the typical Proxmox LXC allocation, on a host with many more.
	writeFixture(t, filepath.Join(root, "cpu.max"), "100000 100000\n")

	assert.Equal(t, 1, AvailableCPUs())
}

func TestAvailableCPUsRoundsFractionalQuotaUp(t *testing.T) {
	root := useFixtureRoot(t)
	// 0.5 cores must still allow one worker, never zero.
	writeFixture(t, filepath.Join(root, "cpu.max"), "50000 100000\n")

	assert.Equal(t, 1, AvailableCPUs())
}

func TestAvailableCPUsFallsBackToGOMAXPROCSWithoutQuota(t *testing.T) {
	useFixtureRoot(t)

	assert.Equal(t, runtime.GOMAXPROCS(0), AvailableCPUs())
}

func TestAvailableCPUsNeverExceedsGOMAXPROCS(t *testing.T) {
	root := useFixtureRoot(t)
	// A quota far above the machine's capacity must not inflate the pool size.
	writeFixture(t, filepath.Join(root, "cpu.max"), "102400000 100000\n")

	assert.Equal(t, runtime.GOMAXPROCS(0), AvailableCPUs())
}

func TestMemoryLimitCgroupV2(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "memory.max"), "536870912\n")

	limit, ok := MemoryLimit()
	require.True(t, ok)
	assert.Equal(t, int64(536870912), limit)
}

func TestMemoryLimitCgroupV2Unlimited(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "memory.max"), "max\n")

	_, ok := MemoryLimit()
	assert.False(t, ok)
}

func TestMemoryLimitCgroupV1(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "memory", "memory.limit_in_bytes"), "268435456\n")

	limit, ok := MemoryLimit()
	require.True(t, ok)
	assert.Equal(t, int64(268435456), limit)
}

func TestMemoryLimitIgnoresSentinelValue(t *testing.T) {
	root := useFixtureRoot(t)
	// The classic cgroup v1 "unlimited" sentinel.
	writeFixture(t, filepath.Join(root, "memory", "memory.limit_in_bytes"), "9223372036854771712\n")

	_, ok := MemoryLimit()
	assert.False(t, ok)
}

// useOwnGroup makes the process report the given unified cgroup path.
func useOwnGroup(t *testing.T, root, group string) {
	t.Helper()

	procSelfCgroup = filepath.Join(root, "proc-self-cgroup")
	writeFixture(t, procSelfCgroup, "0::"+group+"\n")
}

func TestMemoryLimitReadsTheGroupOfTheProcess(t *testing.T) {
	root := useFixtureRoot(t)
	useOwnGroup(t, root, "/nginx-ui/plugins/com.example.plugin")
	writeFixture(t, filepath.Join(root, "nginx-ui", "plugins", "com.example.plugin", "memory.max"), "268435456\n")
	writeFixture(t, filepath.Join(root, "memory.max"), "max\n")

	limit, ok := MemoryLimit()
	require.True(t, ok)
	assert.Equal(t, int64(268435456), limit)
}

func TestMemoryLimitTakesTheSmallestOfTheAncestors(t *testing.T) {
	root := useFixtureRoot(t)
	useOwnGroup(t, root, "/nginx-ui/plugins/com.example.plugin")
	writeFixture(t, filepath.Join(root, "nginx-ui", "plugins", "com.example.plugin", "memory.max"), "1073741824\n")
	writeFixture(t, filepath.Join(root, "nginx-ui", "memory.max"), "536870912\n")

	limit, ok := MemoryLimit()
	require.True(t, ok)
	assert.Equal(t, int64(536870912), limit)
}

func TestCPUQuotaReadsTheGroupOfTheProcess(t *testing.T) {
	root := useFixtureRoot(t)
	useOwnGroup(t, root, "/nginx-ui/plugins/com.example.plugin")
	writeFixture(t, filepath.Join(root, "nginx-ui", "plugins", "com.example.plugin", "cpu.max"), "200000 100000\n")

	quota, ok := CPUQuota()
	require.True(t, ok)
	assert.InDelta(t, 2.0, quota, 0.0001)
}

// stubHostMemory replaces the host memory probe for the duration of a test.
func stubHostMemory(t *testing.T, total, available uint64, err error) {
	t.Helper()

	previous := hostMemory
	hostMemory = func() (uint64, uint64, error) { return total, available, err }
	t.Cleanup(func() { hostMemory = previous })
}

func TestAvailableMemoryPrefersCgroupLimit(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "memory.max"), "536870912\n") // 512MB container
	stubHostMemory(t, 128<<30, 0, nil)                                // 128GB host

	available, ok := AvailableMemory()
	require.True(t, ok)
	assert.Equal(t, int64(536870912), available)
}

func TestAvailableMemoryFallsBackToHostTotal(t *testing.T) {
	useFixtureRoot(t)
	stubHostMemory(t, 2<<30, 0, nil)

	available, ok := AvailableMemory()
	require.True(t, ok)
	assert.Equal(t, int64(2<<30), available)
}

func TestAvailableMemoryIgnoresLimitAboveHostTotal(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "memory.max"), "137438953472\n") // 128GB
	stubHostMemory(t, 1<<30, 0, nil)                                     // 1GB host

	available, ok := AvailableMemory()
	require.True(t, ok)
	assert.Equal(t, int64(1<<30), available)
}

func TestAvailableMemoryUnknown(t *testing.T) {
	useFixtureRoot(t)
	stubHostMemory(t, 0, 0, assert.AnError)

	_, ok := AvailableMemory()
	assert.False(t, ok)
}

func TestAvailableMemoryLeavesOutTheOtherProcessesOfTheGroup(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "memory.max"), "536870912\n")                  // 512 MiB
	writeFixture(t, filepath.Join(root, "memory.stat"), "anon 125829120\nfile 1000\n") // 120 MiB
	writeFixture(t, procSelfStatus, "Name:\tplugin\nRssAnon:\t   20480 kB\n")          // 20 MiB
	stubHostMemory(t, 16<<30, 8<<30, nil)

	available, ok := AvailableMemory()
	require.True(t, ok)
	assert.Equal(t, int64(412<<20), available, "the limit less 100 MiB held by the other processes")
}

func TestAvailableMemoryReadsTheV1GroupUsage(t *testing.T) {
	root := useFixtureRoot(t)
	writeFixture(t, filepath.Join(root, "memory", "memory.limit_in_bytes"), "1073741824\n")
	writeFixture(t, filepath.Join(root, "memory", "memory.stat"), "rss 1\ntotal_rss 268435456\n")
	stubHostMemory(t, 16<<30, 8<<30, nil)

	available, ok := AvailableMemory()
	require.True(t, ok)
	assert.Equal(t, int64(768<<20), available)
}

func TestAvailableMemoryWithoutLimitUsesTheAvailableMemory(t *testing.T) {
	useFixtureRoot(t)
	writeFixture(t, procSelfStatus, "RssAnon:\t102400 kB\n") // 100 MiB
	stubHostMemory(t, 4<<30, 1<<30, nil)

	available, ok := AvailableMemory()
	require.True(t, ok)
	assert.Equal(t, int64(1124<<20), available)
}
