package api

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/nginxui/plugin-log-analytics/internal/geolite"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
)

const (
	StatusInfo     = "info"
	StatusError    = "error"
	StatusProgress = "progress"
)

type DownloadProgressResp struct {
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Message  string  `json:"message"`
}

// downloadRunning keeps two downloads from writing the same file.
var downloadRunning atomic.Bool

// upgrader upgrades the plugin websockets. Every Origin is accepted: the host has
// already authenticated the request and checked its Origin before it proxied it,
// and through the private socket the Origin no longer matches the Host.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func DownloadGeoLiteDB(c *gin.Context) {
	// Upgrade HTTP to WebSocket
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Error(err)
		return
	}
	defer ws.Close()

	// Stop the download when the client goes away.
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	sendMessage := func(status, message string, progress float64) {
		if err := ws.WriteJSON(DownloadProgressResp{
			Status:   status,
			Progress: progress,
			Message:  message,
		}); err != nil {
			logger.Error("Failed to send WebSocket message:", err)
		}
	}

	if !downloadRunning.CompareAndSwap(false, true) {
		sendMessage(StatusError, "A download is already running", 0)
		return
	}
	defer downloadRunning.Store(false)

	// Check if database already exists
	if geolite.DBExists() {
		sendMessage(StatusInfo, "Database already exists, removing old version...", 0)
		// Optionally remove old database here if you want to force re-download
	}

	sendMessage(StatusInfo, "Starting download...", 0)

	// Download progress channel
	downloadProgressChan := make(chan float64, 100)
	downloadDone := make(chan error, 1)

	// Start download in goroutine
	go func() {
		downloadDone <- geolite.DownloadGeoLiteDB(ctx, downloadProgressChan)
	}()

	// Track download progress (0-50%)
	downloadComplete := false
	for !downloadComplete {
		select {
		case progress := <-downloadProgressChan:
			// Scale download progress to 0-50%
			scaledProgress := progress * 0.5
			sendMessage(StatusProgress, "Downloading GeoLite2 database...", scaledProgress)
		case err := <-downloadDone:
			if err != nil {
				sendMessage(StatusError, "Download failed: "+err.Error(), 0)
				return
			}
			downloadComplete = true
			sendMessage(StatusInfo, "Download complete", 50)
		}
	}

	sendMessage(StatusInfo, "Decompressing database...", 50)

	// Decompress progress channel
	decompressProgressChan := make(chan float64, 100)
	decompressDone := make(chan error, 1)

	// Start decompression in goroutine
	go func() {
		decompressDone <- geolite.DecompressGeoLiteDB(decompressProgressChan)
	}()

	// Track decompression progress (50-100%)
	decompressComplete := false
	for !decompressComplete {
		select {
		case progress := <-decompressProgressChan:
			// Scale decompress progress to 50-100%
			scaledProgress := 50 + (progress * 0.5)
			sendMessage(StatusProgress, "Decompressing database...", scaledProgress)
		case err := <-decompressDone:
			if err != nil {
				sendMessage(StatusError, "Decompression failed: "+err.Error(), 50)
				return
			}
			decompressComplete = true
			sendMessage(StatusInfo, "Database ready", 100)
		}
	}

	sendMessage(StatusInfo, "GeoLite2 database downloaded and installed successfully", 100)
}
