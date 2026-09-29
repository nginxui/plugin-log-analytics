package service

import "github.com/nginxui/plugin-log-analytics/internal/apierr"

// The error codes keep the numbers and messages of the handlers before they
// moved into the plugin, so clients translate them as before.
var (
	e                                = apierr.NewScope("nginx_log")
	ErrIndexerNotAvailable           = e.New(50011, "log indexer not available")
	ErrAnalyticsServiceNotAvailable  = e.New(50012, "analytics service not available")
	ErrCannotAccessLogFile           = e.New(50015, "cannot access log file")
	ErrBackgroundServiceNotAvailable = e.New(50016, "background log service not available")
	ErrFilePathRequired              = e.New(50017, "file path is required")
	ErrFailedToRebuildIndex          = e.New(50018, "failed to rebuild index")
	ErrFailedToRebuildFileIndex      = e.New(50019, "failed to rebuild file index")
	ErrFailedToDeleteFileIndex       = e.New(50020, "failed to delete file index")
	ErrFailedToDeleteAllIndexes      = e.New(50021, "failed to delete all indexes")
	ErrFailedToGetIndexStats         = e.New(50022, "failed to get index status")
	ErrFailedToGetPersistenceStats   = e.New(50023, "failed to get persistence stats")
	ErrModernSearcherNotAvailable    = e.New(50026, "modern searcher service not available")
	ErrModernAnalyticsNotAvailable   = e.New(50027, "modern analytics service not available")
	ErrModernIndexerNotAvailable     = e.New(50028, "modern indexer service not available")
)
