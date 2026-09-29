package api

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// LogStatusItem is the index state of one log group. It carries every field the
// host log list exposed for the index columns, plus the group path under the
// name main_log_path and the time range as one object. The fields the page
// reads are always present, zero when there is nothing to say.
type LogStatusItem struct {
	Path           string    `json:"path"`
	MainLogPath    string    `json:"main_log_path"`
	Type           string    `json:"type"`
	Name           string    `json:"name"`
	ConfigFile     string    `json:"config_file"`
	IndexStatus    string    `json:"index_status"`
	LastModified   int64     `json:"last_modified"`
	LastSize       int64     `json:"last_size"`
	LastIndexed    int64     `json:"last_indexed"`
	IndexStartTime int64     `json:"index_start_time"`
	IndexDuration  int64     `json:"index_duration"`
	IsCompressed   bool      `json:"is_compressed"`
	HasTimeRange   bool      `json:"has_timerange"`
	TimeRangeStart int64     `json:"timerange_start"`
	TimeRangeEnd   int64     `json:"timerange_end"`
	TimeRange      TimeRange `json:"timerange"`
	DocumentCount  uint64    `json:"document_count"`
	ErrorMessage   string    `json:"error_message"`
	ErrorTime      int64     `json:"error_time"`
	RetryCount     int       `json:"retry_count"`
	QueuePosition  int       `json:"queue_position"`
}

// statusStartWait is how long the status waits for the services to start. It
// is short because the status can answer without them.
var statusStartWait = 5 * time.Second

// LogStatusResponse is the response of GET /logs/status.
type LogStatusResponse struct {
	Items   []LogStatusItem `json:"items"`
	Summary LogListSummary  `json:"summary"`
}

// GetLogsStatus returns the index state of every log group, grouped by main
// log path like the log list of the host, with the summary of the whole list.
// The optional filters are the ones the host list accepted: type, name, path and
// indexed. The state is read from the metadata database, so the call opens no
// shard. Right after the plugin starts it waits a little for the services, so
// the list does not show every log as not indexed for a moment.
func GetLogsStatus(c *gin.Context) {
	// Without the services the list below still answers from the host logs.
	_ = service.WaitReadyWithin(c.Request.Context(), statusStartWait)

	var filters []func(*service.NginxLogWithIndex) bool

	if logType := c.Query("type"); logType != "" {
		filters = append(filters, func(entry *service.NginxLogWithIndex) bool {
			return entry.Type == logType
		})
	}

	if name := c.Query("name"); name != "" {
		filters = append(filters, func(entry *service.NginxLogWithIndex) bool {
			return strings.Contains(entry.Name, name)
		})
	}

	if path, _ := utils.DecodePathParam(c.Query("path")); path != "" {
		filters = append(filters, func(entry *service.NginxLogWithIndex) bool {
			return strings.Contains(entry.Path, path)
		})
	}

	if indexed := c.Query("indexed"); indexed != "" {
		filters = append(filters, func(entry *service.NginxLogWithIndex) bool {
			switch indexed {
			case "true":
				return entry.IndexStatus == service.IndexStatusIndexed
			case "false":
				return entry.IndexStatus == service.IndexStatusNotIndexed
			case "indexing":
				return entry.IndexStatus == service.IndexStatusIndexing
			default:
				return true
			}
		})
	}

	data := service.GetAllLogsWithIndexGrouped(filters...)
	sort.SliceStable(data, func(i, j int) bool { return data[i].Path < data[j].Path })

	c.JSON(http.StatusOK, LogStatusResponse{
		Items:   logStatusItems(data),
		Summary: summarize(data),
	})
}

func logStatusItems(data []*service.NginxLogWithIndex) []LogStatusItem {
	items := make([]LogStatusItem, 0, len(data))
	for _, entry := range data {
		items = append(items, LogStatusItem{
			Path:           entry.Path,
			MainLogPath:    filepath.Clean(entry.Path),
			Type:           entry.Type,
			Name:           entry.Name,
			ConfigFile:     entry.ConfigFile,
			IndexStatus:    entry.IndexStatus,
			LastModified:   entry.LastModified,
			LastSize:       entry.LastSize,
			LastIndexed:    entry.LastIndexed,
			IndexStartTime: entry.IndexStartTime,
			IndexDuration:  entry.IndexDuration,
			IsCompressed:   entry.IsCompressed,
			HasTimeRange:   entry.HasTimeRange,
			TimeRangeStart: entry.TimeRangeStart,
			TimeRangeEnd:   entry.TimeRangeEnd,
			TimeRange:      TimeRange{Start: entry.TimeRangeStart, End: entry.TimeRangeEnd},
			DocumentCount:  entry.DocumentCount,
			ErrorMessage:   entry.ErrorMessage,
			ErrorTime:      entry.ErrorTime,
			RetryCount:     entry.RetryCount,
			QueuePosition:  entry.QueuePosition,
		})
	}
	return items
}

// summarize computes the totals of a log list. The document count comes from
// the indexer when it has open shards, and from the stored per file counts
// otherwise.
func summarize(data []*service.NginxLogWithIndex) LogListSummary {
	summary := LogListSummary{TotalFiles: len(data)}

	var totalDocuments uint64
	if indexer := service.GetIndexer(); indexer != nil {
		if stats := indexer.GetStats(); stats != nil {
			totalDocuments = stats.TotalDocuments
		}
	}

	var stored uint64
	for _, log := range data {
		stored += log.DocumentCount
		switch log.IndexStatus {
		case service.IndexStatusIndexed:
			summary.IndexedFiles++
		case service.IndexStatusIndexing:
			summary.IndexingFiles++
		}
	}

	// The stored counts are the fallback when no shard is open or the indexer
	// reports nothing yet, the same rule the host list applied.
	if totalDocuments == 0 && summary.IndexedFiles > 0 {
		totalDocuments = stored
	}
	summary.DocumentCount = int(totalDocuments)
	return summary
}
