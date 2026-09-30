package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nginxui/plugin-log-analytics/internal/analytics"
	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

const (
	// cityPointLimit is the number of cities of a hotspot map without a limit.
	cityPointLimit = 500
	// maxCityPointLimit caps the limit a request may ask for.
	maxCityPointLimit = 2000
)

// mapQuery reads a map request and resolves its log group, like the world map.
// It answers the request itself and returns false when it cannot go on.
func mapQuery(c *gin.Context) (AnalyticsRequest, *analytics.GeoQueryRequest, analytics.Service, bool) {
	var req AnalyticsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, err)
		return req, nil, nil, false
	}
	analyticsService := service.GetAnalytics()
	if analyticsService == nil {
		writeError(c, service.ErrModernAnalyticsNotAvailable)
		return req, nil, nil, false
	}

	logPath := ""
	if req.Path != "" {
		decoded, err := decodeAndValidateLogPath(req.Path)
		if err != nil {
			writeError(c, err)
			return req, nil, nil, false
		}
		logPath = decoded
	}
	if rejectErrorLog(c, logPath) {
		return req, nil, nil, false
	}
	if logPath == "" {
		logPath = utils.DefaultAccessLogPath()
	}
	if logPath != "" {
		if err := analyticsService.ValidateLogPath(logPath); err != nil {
			writeError(c, err)
			return req, nil, nil, false
		}
	}

	return req, &analytics.GeoQueryRequest{
		StartTime:      req.StartTime,
		EndTime:        endAfter(req.EndTime),
		LogPath:        logPath,
		LogPaths:       []string{logPath},
		UseMainLogPath: true,
		Limit:          req.Limit,
	}, analyticsService, true
}

// GetRegionMapData answers the requests per subdivision of a country, keyed by
// ISO 3166-2 code, for the region map of a country.
func GetRegionMapData(c *gin.Context) {
	req, geoReq, analyticsService, ok := mapQuery(c)
	if !ok {
		return
	}
	country := strings.ToUpper(strings.TrimSpace(req.Country))
	if len(country) != 2 || strings.Trim(country, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "country must be a two letter ISO code"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	shares, err := analyticsService.GetRegionShares(ctx, geoReq, country)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": shares})
}

// GetCityPointsData answers the busiest cities with their coordinates, of one
// country or of all, for the hotspot map.
func GetCityPointsData(c *gin.Context) {
	req, geoReq, analyticsService, ok := mapQuery(c)
	if !ok {
		return
	}
	switch {
	case geoReq.Limit <= 0:
		geoReq.Limit = cityPointLimit
	case geoReq.Limit > maxCityPointLimit:
		geoReq.Limit = maxCityPointLimit
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	points, err := analyticsService.GetCityPoints(ctx, geoReq, strings.ToUpper(strings.TrimSpace(req.Country)))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": points})
}
