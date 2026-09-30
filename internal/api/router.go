// Package api is the HTTP surface of the plugin. The host proxies
// /api/plugins/com.nginxui.log-analytics/http/* to it over a private socket,
// after it has authenticated the request.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/nginxui/plugin-log-analytics/internal/service"
)

// NewRouter registers every route of the plugin. The paths are relative to the
// plugin HTTP prefix of the host.
func NewRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.Use(gin.Recovery())
	// The routes use fixed paths only, no redirect for a trailing slash.
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false

	// Log list state, no shard is opened for it.
	r.GET("/logs/status", GetLogsStatus)

	// Everything that reads the index goes through queryScope, which opens the
	// shards on demand and keeps them open until the request is done.
	query := r.Group("/", queryScope)
	query.POST("/search", AdvancedSearchLogs)
	query.GET("/entries", GetLogEntries)
	query.POST("/analytics", GetLogAnalytics)
	query.GET("/preflight", GetLogPreflight)
	query.POST("/dashboard", GetDashboardAnalytics)
	query.POST("/geo/world", GetWorldMapData)
	query.POST("/geo/china", GetChinaMapData)
	query.POST("/geo/china/city", GetChinaCityMapData)
	query.POST("/geo/regions", GetRegionMapData)
	query.POST("/geo/points", GetCityPointsData)
	query.POST("/geo/stats", GetGeoStats)

	r.GET("/geo/boundary/:filename", GetGeoBoundaryFile)

	r.POST("/index/rebuild", RebuildIndex)
	r.POST("/warm", Warm)

	r.GET("/geolite/status", GetStatus)
	r.GET("/geolite/download", DownloadGeoLiteDB)

	r.GET("/events", Events)

	return r
}

// queryScope opens the shards a request needs and holds them until it ends.
func queryScope(c *gin.Context) {
	release, err := service.AcquireQuery(c.Request.Context())
	if err != nil {
		writeError(c, err)
		c.Abort()
		return
	}
	defer release()

	c.Next()
}

// Warm opens the shards in the background and answers at once. The page calls
// it as soon as a view that will search is shown, so the shards are usually
// open by the time the first search is sent.
func Warm(c *gin.Context) {
	service.Warm()
	c.JSON(http.StatusAccepted, gin.H{"status": "warming"})
}
