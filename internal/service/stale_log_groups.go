package service

import (
	"context"

	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// pruneStaleLogGroups removes the index of log groups the host no longer lists
// and whose log file is gone, e.g. the access log of a deleted site. It returns
// the groups to pass to the next call.
//
// A group is removed only when the previous call found it stale too. The list
// the host answers while it boots can still be incomplete, and removing an index
// cannot be undone. An empty list proves nothing either, so it removes nothing.
func pruneStaleLogGroups(seen map[string]bool) map[string]bool {
	manager := GetLogFileManager()
	if manager == nil || Processing().Indexing() || len(utils.HostLogList()) == 0 {
		return seen
	}

	groups, err := manager.StaleLogGroups()
	if err != nil {
		logger.Warnf("Failed to look up stale log groups: %v", err)
		return seen
	}

	next := make(map[string]bool, len(groups))
	var confirmed []string
	for _, group := range groups {
		next[group] = true
		if seen[group] {
			confirmed = append(confirmed, group)
		}
	}
	// Removing a group needs its task lock, so it waits for the scheduler.
	scheduler := GetTaskScheduler()
	if len(confirmed) == 0 || scheduler == nil {
		return next
	}

	// The documents can be in any shard, so all of them have to be open.
	release, err := AcquireQuery(context.Background())
	if err != nil {
		logger.Warnf("Failed to open the index shards to remove stale log groups: %v", err)
		return next
	}
	defer release()

	removed := 0
	for _, group := range confirmed {
		// A group an indexing task holds stays in next and is tried again.
		scheduler.TryRunIndexTask(context.Background(), group, func() {
			if err := manager.RemoveLogGroup(group); err != nil {
				logger.Warnf("Failed to remove the index of stale log group %s: %v", group, err)
				return
			}
			logger.Infof("Removed the index of log group %s: the host no longer lists it and its log file is gone", group)
			delete(next, group)
			removed++
		})
	}

	if removed > 0 {
		UpdateSearcherShards()
	}
	return next
}
