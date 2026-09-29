// Package cgroup exposes the CPU and memory budget the current process is
// actually allowed to consume.
//
// Go sizes runtime.NumCPU/GOMAXPROCS from the CPU affinity mask, and
// /proc/meminfo reports whatever the kernel exposes. Inside a cgroup-limited
// container - Docker with --cpus, Kubernetes limits, and in particular an LXC
// container on Proxmox - neither reflects the real budget: the affinity mask
// still lists every host CPU while the CPU bandwidth controller throttles the
// container to a fraction of one. Sizing worker pools or memory budgets from
// the host numbers therefore oversubscribes the container by an order of
// magnitude.
//
// The helpers here read the cgroup v2 and v1 controller files directly and
// fall back to "unlimited" whenever the information is unavailable, so callers
// can clamp their own defaults without special-casing the platform.
package cgroup

import (
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

// cgroupRoot is the mount point of the cgroup filesystem. It is a variable so
// tests can point the readers at a fixture directory.
var cgroupRoot = "/sys/fs/cgroup"

// maxReasonableMemoryLimit filters the sentinel values some kernels use to mean
// "no limit" (for example math.MaxInt64 rounded down to the page size).
const maxReasonableMemoryLimit = int64(1) << 60

// CPUQuota returns the number of CPUs the cgroup bandwidth controller allows
// this process to use. The second return value is false when no quota is
// configured, when the platform has no cgroup filesystem, or when the values
// cannot be parsed. The group of the process and its ancestors are read, and
// the smallest quota wins.
func CPUQuota() (float64, bool) {
	best := 0.0
	found := false
	consider := func(quota float64) {
		if !found || quota < best {
			best, found = quota, true
		}
	}

	for _, dir := range groupDirs("") {
		// cgroup v2: "<quota> <period>" or "max <period>".
		if raw, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
			fields := strings.Fields(string(raw))
			if len(fields) >= 2 && fields[0] != "max" {
				quota, quotaErr := strconv.ParseInt(fields[0], 10, 64)
				period, periodErr := strconv.ParseInt(fields[1], 10, 64)
				if quotaErr == nil && periodErr == nil && quota > 0 && period > 0 {
					consider(float64(quota) / float64(period))
				}
			}
		}
	}

	for _, dir := range groupDirs("cpu") {
		// cgroup v1: quota and period live in separate files, quota == -1 means no limit.
		quota, quotaOK := readInt64(filepath.Join(dir, "cpu.cfs_quota_us"))
		period, periodOK := readInt64(filepath.Join(dir, "cpu.cfs_period_us"))
		if quotaOK && periodOK && quota > 0 && period > 0 {
			consider(float64(quota) / float64(period))
		}
	}

	return best, found
}

// MemoryLimit returns the cgroup memory limit in bytes. The second return value
// is false when the cgroup does not cap memory. The group of the process and its
// ancestors are read, and the smallest limit wins.
func MemoryLimit() (int64, bool) {
	var best int64
	found := false

	read := func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		value := strings.TrimSpace(string(raw))
		if value == "" || value == "max" {
			return
		}
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit <= 0 || limit >= maxReasonableMemoryLimit {
			return
		}
		if !found || limit < best {
			best, found = limit, true
		}
	}

	for _, dir := range groupDirs("") {
		read(filepath.Join(dir, "memory.max")) // cgroup v2
	}
	for _, dir := range groupDirs("memory") {
		read(filepath.Join(dir, "memory.limit_in_bytes")) // cgroup v1
	}

	return best, found
}

// procSelfCgroup lists the cgroups of the current process. It is a variable so
// tests can point it at a fixture file.
var procSelfCgroup = "/proc/self/cgroup"

// groupDirs returns the directories holding the limits that apply to this
// process, from its own group up to the cgroup root. controller is empty for
// the unified (v2) hierarchy, otherwise the name of a v1 controller. Without
// group information only the root is returned.
func groupDirs(controller string) []string {
	base := cgroupRoot
	if controller != "" {
		base = filepath.Join(cgroupRoot, controller)
	}

	own := ownGroupPath(controller)
	if !strings.HasPrefix(own, "/") {
		return []string{base}
	}

	var dirs []string
	for current := path.Clean(own); ; current = path.Dir(current) {
		dirs = append(dirs, filepath.Join(base, filepath.FromSlash(current)))
		if current == "/" {
			break
		}
	}
	return dirs
}

// ownGroupPath returns the cgroup path of this process for the unified
// hierarchy (controller empty) or for one v1 controller.
func ownGroupPath(controller string) string {
	raw, err := os.ReadFile(procSelfCgroup)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if controller == "" && parts[0] == "0" && parts[1] == "" {
			return parts[2]
		}
		if controller != "" && slices.Contains(strings.Split(parts[1], ","), controller) {
			return parts[2]
		}
	}
	return ""
}

// readInt64 parses a cgroup file holding a single integer.
func readInt64(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// totalMemory is indirected so tests can simulate a host without touching the
// real /proc/meminfo.
var totalMemory = func() (uint64, error) {
	stat, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return stat.Total, nil
}

// AvailableMemory reports the memory budget this process should size itself
// against: the cgroup limit when one is set, otherwise the total system memory.
// The second return value is false when neither number is available.
func AvailableMemory() (int64, bool) {
	limit, hasLimit := MemoryLimit()

	total, err := totalMemory()
	if err != nil || total == 0 || total > uint64(maxReasonableMemoryLimit) {
		return limit, hasLimit
	}

	if hasLimit && limit < int64(total) {
		return limit, true
	}
	return int64(total), true
}

// AvailableCPUs reports how many CPUs may be used for sizing worker pools.
//
// It is the smaller of GOMAXPROCS and the cgroup CPU quota, and never less
// than 1. Callers should use this instead of runtime.GOMAXPROCS/NumCPU when the
// value decides how many goroutines will compete for the CPU: exceeding the
// cgroup quota does not add throughput, it only multiplies peak memory and
// causes the scheduler to thrash against CFS throttling.
func AvailableCPUs() int {
	procs := runtime.GOMAXPROCS(0)
	if procs < 1 {
		procs = 1
	}

	quota, ok := CPUQuota()
	if !ok {
		return procs
	}

	limited := int(math.Ceil(quota))
	if limited < 1 {
		limited = 1
	}
	if limited < procs {
		return limited
	}
	return procs
}
