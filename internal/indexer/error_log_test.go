package indexer

import (
	"path/filepath"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

const errorLogLines = `2026/09/07 15:32:29 [error] 12#12: *1 open() "/srv/favicon.ico" failed (2: No such file or directory), client: 203.0.113.9, server: example.com, request: "GET /favicon.ico HTTP/1.1", host: "example.com"
2026/09/07 15:32:30 [warn] 12#12: *2 an upstream response is buffered to a temporary file, client: 198.51.100.4, server: example.com, request: "POST /upload HTTP/1.1", upstream: "http://127.0.0.1:9000/upload", host: "example.com"
2026/09/07 15:33:00 [notice] 1#1: signal process started
 a line that continues the previous message
2026/09/07 15:34:00 [crit] 12#12: *3 SSL_do_handshake() failed, client: 203.0.113.9, server: 0.0.0.0:443
`

func TestSyncIndexesAnErrorLogWithItsLevels(t *testing.T) {
	e := newSyncEnv(t)
	e.logPath = filepath.Join(e.dir, "error.log")
	utils.SetHostLogs([]utils.HostLog{{Path: e.logPath, Type: utils.ErrorLogType, Source: "config"}})

	e.write("error.log", errorLogLines)
	group := e.sync()
	require.NotNil(t, group)
	// The continuation line is not an entry of its own
	assert.Equal(t, uint64(4), group.Files[e.logPath].Docs)

	count := func(level string) uint64 {
		var total uint64
		for _, shard := range e.pi.shardManager.GetAllShards() {
			q := bleve.NewTermQuery(level)
			q.SetField("level")
			result, err := shard.Search(bleve.NewSearchRequest(q))
			require.NoError(t, err)
			total += result.Total
		}
		return total
	}
	assert.Equal(t, uint64(1), count("error"))
	assert.Equal(t, uint64(1), count("warn"))
	assert.Equal(t, uint64(1), count("crit"))

	var found bool
	for _, shard := range e.pi.shardManager.GetAllShards() {
		q := bleve.NewTermQuery("/upload")
		q.SetField("path_exact")
		request := bleve.NewSearchRequest(q)
		request.Fields = []string{"level", "ip", "method", "main_log_path"}
		result, err := shard.Search(request)
		require.NoError(t, err)
		for _, hit := range result.Hits {
			found = true
			assert.Equal(t, "warn", hit.Fields["level"])
			assert.Equal(t, "198.51.100.4", hit.Fields["ip"])
			assert.Equal(t, "POST", hit.Fields["method"])
			assert.Equal(t, e.logPath, hit.Fields["main_log_path"])
		}
	}
	assert.True(t, found, "the entry with a request is found by its path")

	// Appended entries are read once
	e.appendTo("error.log", "2026/09/07 15:35:00 [error] 12#12: *4 late entry, client: 192.0.2.1\n")
	group = e.sync()
	assert.Equal(t, uint64(1), group.Files[e.logPath].Docs)
	assert.Equal(t, uint64(2), count("error"))
}
