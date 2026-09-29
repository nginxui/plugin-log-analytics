package api

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/service"
)

const (
	// eventsWriteTimeout bounds one write to a websocket client.
	eventsWriteTimeout = 10 * time.Second
	// eventsPingInterval keeps idle connections alive through proxies.
	eventsPingInterval = 30 * time.Second
)

// Events streams the indexing events to the client as {type, data} messages,
// the same types and payloads the host event bus carried: index progress,
// completion, readiness and the processing status. The current processing
// status is sent right after the connection opens.
func Events(c *gin.Context) {
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Error(err)
		return
	}
	defer ws.Close()

	stream, unsubscribe := service.Events().Subscribe()
	defer unsubscribe()

	// Read to notice a closed connection and to process control frames.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	service.Processing().Broadcast()

	ping := time.NewTicker(eventsPingInterval)
	defer ping.Stop()

	for {
		select {
		case event, ok := <-stream:
			if !ok {
				return
			}
			_ = ws.SetWriteDeadline(time.Now().Add(eventsWriteTimeout))
			if err := ws.WriteJSON(event); err != nil {
				return
			}
		case <-ping.C:
			_ = ws.SetWriteDeadline(time.Now().Add(eventsWriteTimeout))
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-closed:
			return
		case <-c.Request.Context().Done():
			return
		}
	}
}
