// Package memory sets the soft memory limit of the Go runtime from the cgroup
// limit of the plugin process, or from the system memory without one, so the
// garbage collector works harder as the process nears the limit instead of the
// kernel killing it.
package memory

import (
	"os"
	"runtime/debug"

	"github.com/nginxui/plugin-log-analytics/internal/cgroup"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
)

// limitPercent is the share of the cgroup limit the Go runtime aims to stay
// under. The rest is room for what the runtime does not count: mapped index
// files, stacks and the C-free parts of the standard library.
const limitPercent = 80

// hostLimitPercent is the share of the total system memory the Go runtime aims
// to stay under when no cgroup limit exists. The plugin shares the machine with
// nginx-ui and other services.
const hostLimitPercent = 50

// EnvGoMemLimit is the standard environment variable of the runtime limit. When
// it is set, it wins and nothing is changed here.
const EnvGoMemLimit = "GOMEMLIMIT"

// Limit returns the soft limit, or false when none applies: GOMEMLIMIT is set
// explicitly, or no memory size is known. A cgroup limit wins over the total
// system memory.
func Limit(cgroupLimit int64, hasCgroupLimit bool, totalMemory int64, hasTotalMemory bool, goMemLimitSet bool) (int64, bool) {
	if goMemLimitSet {
		return 0, false
	}
	if hasCgroupLimit && cgroupLimit > 0 {
		return cgroupLimit / 100 * limitPercent, true
	}
	if hasTotalMemory && totalMemory > 0 {
		return totalMemory / 100 * hostLimitPercent, true
	}
	return 0, false
}

// Apply sets the runtime memory limit to 80% of the cgroup memory limit of the
// process, or to half of the system memory when there is no cgroup limit,
// unless GOMEMLIMIT is set. It returns the limit it set, or zero.
func Apply() int64 {
	_, envSet := os.LookupEnv(EnvGoMemLimit)
	cgroupLimit, hasCgroup := cgroup.MemoryLimit()
	total, hasTotal := cgroup.TotalMemory()

	limit, apply := Limit(cgroupLimit, hasCgroup, total, hasTotal, envSet)
	if !apply {
		return 0
	}

	debug.SetMemoryLimit(limit)
	if hasCgroup && cgroupLimit > 0 {
		logger.Infof("Memory limit of the process is %d MiB, the garbage collector aims for %d MiB", cgroupLimit>>20, limit>>20)
	} else {
		logger.Infof("System memory is %d MiB, the garbage collector aims for %d MiB", total>>20, limit>>20)
	}
	return limit
}
