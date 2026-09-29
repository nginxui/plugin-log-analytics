// Package memory sets the soft memory limit of the Go runtime from the cgroup
// limit of the plugin process, so the garbage collector works harder as the
// process nears the limit instead of the kernel killing it.
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

// EnvGoMemLimit is the standard environment variable of the runtime limit. When
// it is set, it wins and nothing is changed here.
const EnvGoMemLimit = "GOMEMLIMIT"

// Limit returns the soft limit for a cgroup limit, or false when none applies:
// the cgroup does not cap memory, or GOMEMLIMIT is set explicitly.
func Limit(cgroupLimit int64, hasCgroupLimit bool, goMemLimitSet bool) (int64, bool) {
	if goMemLimitSet || !hasCgroupLimit || cgroupLimit <= 0 {
		return 0, false
	}
	return cgroupLimit / 100 * limitPercent, true
}

// Apply sets the runtime memory limit to 80% of the cgroup memory limit of the
// process, unless GOMEMLIMIT is set. It returns the limit it set, or zero.
func Apply() int64 {
	_, envSet := os.LookupEnv(EnvGoMemLimit)
	cgroupLimit, ok := cgroup.MemoryLimit()

	limit, apply := Limit(cgroupLimit, ok, envSet)
	if !apply {
		return 0
	}

	debug.SetMemoryLimit(limit)
	logger.Infof("Memory limit of the process is %d MiB, the garbage collector aims for %d MiB", cgroupLimit>>20, limit>>20)
	return limit
}
