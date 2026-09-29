package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/service"
)

// TestFirstRequestsWaitForShardsToOpen fires many requests at once right after the
// services start, while no shard is open. Every one of them has to succeed.
func TestFirstRequestsWaitForShardsToOpen(t *testing.T) {
	env := startHTTPEnv(t, 200)

	service.StopServices()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service.InitializeServices(ctx)
	require.Zero(t, len(service.GetIndexer().GetAllShards()))

	const clients = 16
	codes := make(chan int, clients)
	for i := 0; i < clients; i++ {
		go func(i int) {
			path := "/search"
			body := map[string]any{"log_path": env.logPath, "limit": 10}
			if i%3 == 1 {
				path = "/dashboard"
				body = map[string]any{"log_path": env.logPath, "start_date": "2026-08-12", "end_date": "2026-08-13"}
			}
			if i%3 == 2 {
				path = "/geo/world"
				body = map[string]any{"path": env.logPath, "start_time": 1, "end_time": time.Now().Unix()}
			}
			codes <- env.do(t, http.MethodPost, path, body).Code
		}(i)
	}
	for i := 0; i < clients; i++ {
		require.Equal(t, http.StatusOK, <-codes)
	}
}
