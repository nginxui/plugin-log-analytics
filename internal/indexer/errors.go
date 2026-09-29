package indexer

import "github.com/nginxui/plugin-log-analytics/internal/apierr"

var (
	e                          = apierr.NewScope("nginx_log.indexer")
	ErrLogParserNotInitialized = e.New(50201, "log parser is not initialized; call indexer.InitLogParser() before use")
)
