package api

import (
	"context"
	"maps"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nginxui/plugin-log-analytics/internal/analytics"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/parser"
	"github.com/nginxui/plugin-log-analytics/internal/searcher"
	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

type GeoRegionItem struct {
	Code    string  `json:"code"`
	Value   int     `json:"value"`
	Percent float64 `json:"percent"`
}

// decodeAndValidateLogPath normalizes user input before it can be used in any
// path-related operation.
func decodeAndValidateLogPath(rawPath string) (string, error) {
	if rawPath == "" {
		return "", nil
	}

	decodedPath, _ := utils.DecodePathParam(rawPath)
	normalizedPath := filepath.Clean(decodedPath)
	if !utils.IsValidLogPath(normalizedPath) {
		return "", utils.ErrPathNotWhitelisted
	}

	return normalizedPath, nil
}

// AnalyticsRequest represents the request for log analytics
type AnalyticsRequest struct {
	Path      string `json:"path" form:"path"`
	StartTime int64  `json:"start_time" form:"start_time"`
	EndTime   int64  `json:"end_time" form:"end_time"`
	Limit     int    `json:"limit" form:"limit"`
	// Country is the ISO code of a country, for the region and hotspot maps.
	Country string `json:"country" form:"country"`
}

// AdvancedSearchRequest represents the request for advanced log search
type AdvancedSearchRequest struct {
	Query     string `json:"query" form:"query"`
	LogPath   string `json:"log_path" form:"log_path"`
	StartTime int64  `json:"start_time" form:"start_time"`
	EndTime   int64  `json:"end_time" form:"end_time"`
	IP        string `json:"ip" form:"ip"`
	Method    string `json:"method" form:"method"`
	Status    []int  `json:"status" form:"status"`
	Path      string `json:"path" form:"path"`
	UserAgent string `json:"user_agent" form:"user_agent"`
	Referer   string `json:"referer" form:"referer"`
	Browser   string `json:"browser" form:"browser"`
	OS        string `json:"os" form:"os"`
	Device    string `json:"device" form:"device"`
	// Level holds error log levels, comma separated.
	Level     string `json:"level" form:"level"`
	Limit     int    `json:"limit" form:"limit"`
	Offset    int    `json:"offset" form:"offset"`
	SortBy    string `json:"sort_by" form:"sort_by"`
	SortOrder string `json:"sort_order" form:"sort_order"`
}

// SummaryStats Structures to match the frontend's expectations for the search response
type SummaryStats struct {
	UV              int     `json:"uv"`
	PV              int     `json:"pv"`
	TotalTraffic    int64   `json:"total_traffic"`
	UniquePages     int     `json:"unique_pages"`
	AvgTrafficPerPV float64 `json:"avg_traffic_per_pv"`

	// TrafficApproximate reports that the traffic figures were extrapolated
	// because the match set was larger than the stats scan budget.
	TrafficApproximate bool `json:"traffic_approximate"`
}

type AdvancedSearchResponseAPI struct {
	Entries []map[string]interface{} `json:"entries"`
	Total   uint64                   `json:"total"`
	Took    int64                    `json:"took"` // Milliseconds
	Query   string                   `json:"query"`
	Summary SummaryStats             `json:"summary"`
}

// PreflightResponse represents the response for preflight query

// GetLogAnalytics provides comprehensive log analytics
func GetLogAnalytics(c *gin.Context) {
	var req AnalyticsRequest
	if !bindAndValid(c, &req) {
		return
	}

	// Get modern analytics service
	analyticsService := service.GetAnalytics()
	if analyticsService == nil {
		writeError(c, service.ErrModernAnalyticsNotAvailable)
		return
	}

	if req.Path != "" {
		decodedPath, err := decodeAndValidateLogPath(req.Path)
		if err != nil {
			writeError(c, err)
			return
		}
		req.Path = decodedPath
	}

	// Validate log path
	if err := analyticsService.ValidateLogPath(req.Path); err != nil {
		writeError(c, err)
		return
	}

	// Build search request for log entries statistics
	searchReq := &searcher.SearchRequest{
		Limit:         req.Limit,
		UseCache:      true,
		IncludeStats:  true,
		IncludeFacets: true,
		FacetFields:   []string{"path", "ip", "user_agent", "status", "method"},
	}

	if req.StartTime > 0 {
		searchReq.StartTime = &req.StartTime
	}
	if req.EndTime > 0 {
		end := endAfter(req.EndTime)
		searchReq.EndTime = &end
	}

	// Get log entries statistics
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	stats, err := analyticsService.GetLogEntriesStats(ctx, searchReq)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusOK, stats)
}

// GetLogPreflight returns the preflight status for log indexing
func GetLogPreflight(c *gin.Context) {
	// Get optional log path parameter. Decoded before use: the value may arrive
	// base64url-encoded so that a WAF does not read it as a traversal attempt.
	logPath, _ := utils.DecodePathParam(c.Query("log_path"))

	// Create preflight service and perform check
	preflightService := service.NewPreflight()
	internalResponse, err := preflightService.CheckLogPreflight(logPath)
	if err != nil {
		writeError(c, err)
		return
	}

	// Convert internal response to API response
	response := PreflightResponse{
		Available:   internalResponse.Available,
		IndexStatus: internalResponse.IndexStatus,
		Message:     internalResponse.Message,
	}

	if internalResponse.TimeRange != nil {
		response.TimeRange = &TimeRange{
			Start: internalResponse.TimeRange.Start,
			End:   internalResponse.TimeRange.End,
		}
	}

	if internalResponse.FileInfo != nil {
		response.FileInfo = &FileInfo{
			Exists:       internalResponse.FileInfo.Exists,
			Readable:     internalResponse.FileInfo.Readable,
			Size:         internalResponse.FileInfo.Size,
			LastModified: internalResponse.FileInfo.LastModified,
		}
	}

	c.JSON(http.StatusOK, response)
}

// localDay returns the first instant of a YYYY-MM-DD day in the local zone of
// the server.
func localDay(value string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", value, time.Local)
}

// endAfter returns the exclusive end of a request range. The end time of a
// request is the last second it includes, zero leaves the range open.
func endAfter(endTime int64) int64 {
	if endTime <= 0 {
		return endTime
	}
	return endTime + 1
}

// splitCommaSeparated splits a comma-joined filter value (as produced by the
// frontend multi-select inputs) into a slice of trimmed, non-empty values.
func splitCommaSeparated(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func toOptionalString(value interface{}) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(s)
}

func isChineseLanguageRequest(c *gin.Context) bool {
	if c == nil {
		return false
	}

	for _, header := range []string{"Accept-Language", "X-Language", "X-Locale"} {
		value := strings.ToLower(strings.TrimSpace(c.GetHeader(header)))
		if value == "" {
			continue
		}
		if strings.HasPrefix(value, "zh") || strings.Contains(value, "zh-") || strings.Contains(value, "zh_") {
			return true
		}
	}

	return false
}

func displayCountryName(regionCode string, countryNameZH string, useChineseName bool) string {
	code := strings.TrimSpace(regionCode)
	if !useChineseName {
		return code
	}

	if cnName := strings.TrimSpace(countryNameZH); cnName != "" {
		return cnName
	}

	if code == "CN" {
		return "中国"
	}

	return code
}

func buildStructuredIPLocationLabel(entry map[string]interface{}, useChineseName bool) string {
	baseParts := make([]string, 0, 3)
	if regionCode := displayCountryName(
		toOptionalString(entry["region_code"]),
		toOptionalString(entry["country_name_zh"]),
		useChineseName,
	); regionCode != "" {
		baseParts = append(baseParts, regionCode)
	}
	if province := toOptionalString(entry["province"]); province != "" {
		baseParts = append(baseParts, province)
	}
	if city := toOptionalString(entry["city"]); city != "" {
		baseParts = append(baseParts, city)
	}

	customParts := make([]string, 0, 4)
	for _, key := range []string{"c1", "c2", "c3", "c4"} {
		if value := toOptionalString(entry[key]); value != "" {
			customParts = append(customParts, value)
		}
	}

	baseLabel := strings.Join(baseParts, " · ")
	if len(customParts) == 0 {
		return baseLabel
	}

	customLabel := strings.Join(customParts, " · ")
	if baseLabel == "" {
		return customLabel
	}

	return baseLabel + " · " + customLabel
}

func enrichEntryWithIPLocationLabel(entry map[string]interface{}, useChineseName bool) map[string]interface{} {
	if entry == nil {
		return entry
	}

	entry["ip_location_label"] = buildStructuredIPLocationLabel(entry, useChineseName)
	return entry
}

// enrichErrorEntry adds what the line of an error log entry says beyond the
// indexed fields: the message, the server, the whole request, the upstream,
// the host and the process and connection numbers. Access log entries are
// returned unchanged.
func enrichErrorEntry(entry map[string]interface{}) map[string]interface{} {
	level, _ := entry["level"].(string)
	raw, _ := entry["raw"].(string)
	if level == "" || raw == "" {
		return entry
	}
	parsed, ok := parser.ParseErrorLine(raw)
	if !ok {
		return entry
	}
	entry["message"] = parsed.Message
	entry["server"] = parsed.Server
	entry["request"] = parsed.Request
	entry["upstream"] = parsed.Upstream
	entry["host"] = parsed.Host
	entry["pid"] = parsed.PID
	entry["connection"] = parsed.Connection
	return entry
}

// rejectErrorLog answers a dashboard or map request for an error log, whose
// entries have no traffic figures. It reports whether it answered.
func rejectErrorLog(c *gin.Context, logPath string) bool {
	if logPath == "" || !utils.IsErrorLog(logPath) {
		return false
	}
	c.JSON(http.StatusBadRequest, ErrorResponse{Error: "The dashboard and the maps are only available for access logs"})
	return true
}

// AdvancedSearchLogs provides advanced search capabilities for logs
func AdvancedSearchLogs(c *gin.Context) {
	var req AdvancedSearchRequest
	if !bindAndValid(c, &req) {
		return
	}

	searcherService := service.GetSearcher()
	if searcherService == nil {
		writeError(c, service.ErrModernSearcherNotAvailable)
		return
	}

	analyticsService := service.GetAnalytics()
	if analyticsService == nil {
		writeError(c, service.ErrModernAnalyticsNotAvailable)
		return
	}

	rawLogPath := req.LogPath
	safeLogPath := ""

	if rawLogPath != "" {
		decodedPath, err := decodeAndValidateLogPath(rawLogPath)
		if err != nil {
			writeError(c, err)
			return
		}
		safeLogPath = decodedPath
	}

	// Use default access log path if LogPath is empty
	if safeLogPath == "" {
		defaultLogPath := utils.DefaultAccessLogPath()
		if defaultLogPath != "" {
			safeLogPath = defaultLogPath
			logger.Debugf("Using default access log path for search: %s", safeLogPath)
		}
	}

	// Validate log path if provided
	if safeLogPath != "" {
		if err := analyticsService.ValidateLogPath(safeLogPath); err != nil {
			writeError(c, err)
			return
		}
	}

	// Build search request. The summary is computed in the same pass as the
	// page of hits, over the whole match set: visitors, distinct pages and the
	// traffic totals.
	searchReq := &searcher.SearchRequest{
		Query:     req.Query,
		Limit:     req.Limit,
		Offset:    req.Offset,
		SortBy:    req.SortBy,
		SortOrder: req.SortOrder,
		UseCache:  true,
		Timeout:   60 * time.Second,
		Summary: &searcher.SummarySpec{
			DistinctFields: []string{"ip", "path_exact"},
			Bytes:          true,
		},
	}

	// If no sorting is specified, default to sorting by timestamp descending.
	if searchReq.SortBy == "" {
		searchReq.SortBy = "timestamp"
		searchReq.SortOrder = "desc"
	}

	// Expand the base log path to all physical files in the group using filesystem globbing.
	if safeLogPath != "" {
		// lgtm[go/path-injection]
		logPaths, err := service.ExpandLogGroupPath(safeLogPath)
		if err != nil {
			logger.Warnf("Could not expand log group path %s: %v", safeLogPath, err)
			// Fallback to using the raw path when expansion fails
			searchReq.LogPaths = []string{safeLogPath}
		} else if len(logPaths) == 0 {
			// ExpandLogGroupPath succeeded but returned empty slice (file doesn't exist on filesystem)
			// Still search for historical indexed data using the requested path
			logger.Debugf("Log file %s does not exist on filesystem, but searching for historical indexed data", safeLogPath)
			searchReq.LogPaths = []string{safeLogPath}
		} else {
			searchReq.LogPaths = logPaths
		}
		logger.Debugf("Search request LogPaths: %v", searchReq.LogPaths)
	}

	// Add time filters
	if req.StartTime > 0 {
		searchReq.StartTime = &req.StartTime
	}
	if req.EndTime > 0 {
		end := endAfter(req.EndTime)
		searchReq.EndTime = &end
	}
	// If no time range is provided, default to searching all time.
	if searchReq.StartTime == nil && searchReq.EndTime == nil {
		var startTime int64 = 0 // Unix epoch
		end := endAfter(time.Now().Unix())
		searchReq.StartTime = &startTime
		searchReq.EndTime = &end
	}

	// Add field filters
	if req.IP != "" {
		searchReq.IPAddresses = []string{req.IP}
	}
	if req.Method != "" {
		searchReq.Methods = []string{req.Method}
	}
	if req.Path != "" {
		searchReq.Paths = []string{req.Path}
	}
	if req.UserAgent != "" {
		searchReq.UserAgents = []string{req.UserAgent}
	}
	if req.Referer != "" {
		searchReq.Referers = []string{req.Referer}
	}
	if req.Browser != "" {
		searchReq.Browsers = splitCommaSeparated(req.Browser)
	}
	if req.OS != "" {
		searchReq.OSs = splitCommaSeparated(req.OS)
	}
	if req.Device != "" {
		searchReq.Devices = splitCommaSeparated(req.Device)
	}
	for _, value := range splitCommaSeparated(req.Level) {
		if level, ok := parser.ErrorLevelOf(value); ok {
			searchReq.Levels = append(searchReq.Levels, level)
		}
	}
	if len(req.Status) > 0 {
		searchReq.StatusCodes = req.Status
	}

	// Execute search with timeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Minute)
	defer cancel()

	result, err := searcherService.Search(ctx, searchReq)
	if err != nil {
		writeError(c, err)
		return
	}
	useChineseName := isChineseLanguageRequest(c)

	// --- Transform the searcher result to the API response structure ---

	// 1. Extract entries from hits. The hits may be shared with the result
	// cache and with other requests, so the entry is a copy that can be changed.
	entries := make([]map[string]interface{}, len(result.Hits))
	for i, hit := range result.Hits {
		entries[i] = enrichErrorEntry(enrichEntryWithIPLocationLabel(maps.Clone(hit.Fields), useChineseName))
	}

	// 2. Summary stats describe the whole match set, not the returned page
	pv := int(result.TotalHits)
	var uv, uniquePages int
	var totalTraffic int64
	var avgTraffic float64
	if summary := result.Summary; summary != nil {
		uv = summary.Distinct["ip"]
		uniquePages = summary.Distinct["path_exact"]
		totalTraffic = summary.TotalBytes
		if summary.Docs > 0 {
			avgTraffic = float64(summary.TotalBytes) / float64(summary.Docs)
		}
	}

	summary := SummaryStats{
		UV:                 uv,
		PV:                 pv,
		TotalTraffic:       totalTraffic,
		UniquePages:        uniquePages,
		AvgTrafficPerPV:    avgTraffic,
		TrafficApproximate: false,
	}

	// 3. Assemble the final response
	apiResponse := AdvancedSearchResponseAPI{
		Entries: entries,
		Total:   result.TotalHits,
		Took:    result.Duration.Milliseconds(),
		Query:   req.Query,
		Summary: summary,
	}

	c.JSON(http.StatusOK, apiResponse)
}

// GetLogEntries provides simple log entry retrieval
func GetLogEntries(c *gin.Context) {
	var req struct {
		Path  string `json:"path" form:"path"`
		Limit int    `json:"limit" form:"limit"`
		Tail  bool   `json:"tail" form:"tail"` // Get latest entries
	}

	if err := c.ShouldBindQuery(&req); err != nil {
		writeError(c, err)
		return
	}

	req.Path, _ = utils.DecodePathParam(req.Path)

	searcherService := service.GetSearcher()
	if searcherService == nil {
		writeError(c, service.ErrModernSearcherNotAvailable)
		return
	}

	analyticsService := service.GetAnalytics()
	if analyticsService == nil {
		writeError(c, service.ErrModernAnalyticsNotAvailable)
		return
	}

	// Validate log path
	if err := analyticsService.ValidateLogPath(req.Path); err != nil {
		writeError(c, err)
		return
	}

	// Set default limit
	if req.Limit == 0 {
		req.Limit = 100
	}

	// Build search request
	searchReq := &searcher.SearchRequest{
		Limit:     req.Limit,
		UseCache:  false, // Don't cache simple entry requests
		SortBy:    "timestamp",
		SortOrder: "desc", // Latest first by default
	}

	if req.Tail {
		searchReq.SortOrder = "desc" // Latest entries first
	} else {
		searchReq.SortOrder = "asc" // Oldest entries first
	}

	// Execute search
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	result, err := searcherService.Search(ctx, searchReq)
	if err != nil {
		writeError(c, err)
		return
	}
	useChineseName := isChineseLanguageRequest(c)

	// Convert search hits to simple entries format
	var entries []map[string]interface{}
	for _, hit := range result.Hits {
		entries = append(entries, enrichErrorEntry(enrichEntryWithIPLocationLabel(maps.Clone(hit.Fields), useChineseName)))
	}

	c.JSON(http.StatusOK, AnalyticsResponse{
		Entries: entries,
		Count:   len(entries),
	})
}

// DashboardRequest represents the request for dashboard analytics
type DashboardRequest struct {
	LogPath   string `json:"log_path" form:"log_path"`
	StartDate string `json:"start_date" form:"start_date"` // Format: 2006-01-02
	EndDate   string `json:"end_date" form:"end_date"`     // Format: 2006-01-02
}

func bindDashboardRequest(c *gin.Context) (DashboardRequest, bool) {
	var payload DashboardRequest
	if !bindAndValid(c, &payload) {
		return DashboardRequest{}, false
	}

	return payload, true
}

// HourlyStats represents hourly UV/PV statistics
type HourlyStats struct {
	Hour      int   `json:"hour"`      // 0-23
	UV        int   `json:"uv"`        // Unique visitors (unique IPs)
	PV        int   `json:"pv"`        // Page views (total requests)
	Timestamp int64 `json:"timestamp"` // Unix timestamp for the hour
}

// DailyStats represents daily access statistics
type DailyStats struct {
	Date      string `json:"date"`      // YYYY-MM-DD format
	UV        int    `json:"uv"`        // Unique visitors
	PV        int    `json:"pv"`        // Page views
	Timestamp int64  `json:"timestamp"` // Unix timestamp for the day
}

// URLStats represents URL access statistics
type URLStats struct {
	URL     string  `json:"url"`
	Visits  int     `json:"visits"`
	Percent float64 `json:"percent"`
}

// BrowserStats represents browser statistics
type BrowserStats struct {
	Browser string  `json:"browser"`
	Count   int     `json:"count"`
	Percent float64 `json:"percent"`
}

// OSStats represents operating system statistics
type OSStats struct {
	OS      string  `json:"os"`
	Count   int     `json:"count"`
	Percent float64 `json:"percent"`
}

// DeviceStats represents device type statistics
type DeviceStats struct {
	Device  string  `json:"device"`
	Count   int     `json:"count"`
	Percent float64 `json:"percent"`
}

// DashboardResponse represents the dashboard analytics response
type DashboardResponse struct {
	HourlyStats      []HourlyStats  `json:"hourly_stats"`      // 24-hour UV/PV data
	DailyStats       []DailyStats   `json:"daily_stats"`       // Monthly trend data
	TopURLs          []URLStats     `json:"top_urls"`          // TOP 10 URLs
	Browsers         []BrowserStats `json:"browsers"`          // Browser statistics
	OperatingSystems []OSStats      `json:"operating_systems"` // OS statistics
	Devices          []DeviceStats  `json:"devices"`           // Device statistics
	Summary          struct {
		TotalUV         int     `json:"total_uv"`          // Total unique visitors
		TotalPV         int     `json:"total_pv"`          // Total page views
		TotalTraffic    int64   `json:"total_traffic"`     // Total bytes sent
		AvgDailyUV      float64 `json:"avg_daily_uv"`      // Average daily UV
		AvgDailyPV      float64 `json:"avg_daily_pv"`      // Average daily PV
		PeakHour        int     `json:"peak_hour"`         // Peak traffic hour (0-23)
		PeakHourTraffic int     `json:"peak_hour_traffic"` // Peak hour PV count
		AvgQPS          float64 `json:"avg_qps"`           // Requests per second across the range
		PeakQPS         float64 `json:"peak_qps"`          // Busiest minute expressed per second
	} `json:"summary"`
}

// GetDashboardAnalytics provides comprehensive dashboard analytics from modern analytics service
func GetDashboardAnalytics(c *gin.Context) {
	// Parse JSON body for POST request
	// The previous direct binding path was flagged by security scans as
	// "Uncontrolled data used in path expression".
	// Use bindAndValid for consistent bind+validation handling before
	// path-related fields are normalized and whitelist-validated.
	req, ok := bindDashboardRequest(c)
	if !ok {
		return
	}

	logger.Debugf("Dashboard API received log_path: '%s', start_date: '%s', end_date: '%s'", req.LogPath, req.StartDate, req.EndDate)

	analyticsService := service.GetAnalytics()
	if analyticsService == nil {
		writeError(c, service.ErrModernAnalyticsNotAvailable)
		return
	}

	rawLogPath := req.LogPath
	safeLogPath := ""

	if rawLogPath != "" {
		decodedPath, err := decodeAndValidateLogPath(rawLogPath)
		if err != nil {
			writeError(c, err)
			return
		}
		safeLogPath = decodedPath
	}
	if rejectErrorLog(c, safeLogPath) {
		return
	}

	// Use default access log path if LogPath is empty
	if safeLogPath == "" {
		defaultLogPath := utils.DefaultAccessLogPath()
		if defaultLogPath != "" {
			safeLogPath = defaultLogPath
			logger.Debugf("Using default access log path: %s", safeLogPath)
		}
	}

	// Validate log path if provided
	if safeLogPath != "" {
		if err := analyticsService.ValidateLogPath(safeLogPath); err != nil {
			writeError(c, err)
			return
		}
	}

	// Parse and validate date strings
	var startTime, endTime time.Time
	var err error

	// The dates are days in the local zone of the server, like the daily and
	// hourly buckets of the dashboard.
	if req.StartDate != "" {
		startTime, err = localDay(req.StartDate)
		if err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: "Invalid start_date format, expected YYYY-MM-DD: " + err.Error()})
			return
		}
	}

	if req.EndDate != "" {
		endTime, err = localDay(req.EndDate)
		if err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: "Invalid end_date format, expected YYYY-MM-DD: " + err.Error()})
			return
		}
		// The end date is included: the window ends at the next local midnight
		endTime = endTime.AddDate(0, 0, 1)
	}

	// Set default time range if not provided (last 30 days)
	if startTime.IsZero() || endTime.IsZero() {
		endTime = time.Now()
		startTime = endTime.AddDate(0, 0, -30) // 30 days ago
	}

	// Get dashboard analytics with timeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	logger.Debugf("Dashboard request for log_path: %s, parsed start_time: %v, end_time: %v", safeLogPath, startTime, endTime)

	// Use main_log_path field for efficient log group queries instead of expanding file paths
	// This provides much better performance by using indexed field filtering
	logger.Debugf("Dashboard querying log group with main_log_path: %s", safeLogPath)

	// Build dashboard query request
	// lgtm[go/path-injection]
	dashboardReq := &analytics.DashboardQueryRequest{
		LogPath:   safeLogPath,
		LogPaths:  []string{safeLogPath}, // Use single main log path
		StartTime: startTime.Unix(),
		EndTime:   endTime.Unix(),
	}
	logger.Debugf("Query parameters - LogPath='%s', StartTime=%v, EndTime=%v",
		dashboardReq.LogPath, dashboardReq.StartTime, dashboardReq.EndTime)

	// Get analytics from modern analytics service
	result, err := analyticsService.GetDashboardAnalytics(ctx, dashboardReq)

	if err != nil {
		writeError(c, err)
		return
	}

	logger.Debugf("Successfully retrieved dashboard analytics")

	// Debug: Log summary of results
	if result != nil {
		logger.Debugf("Results summary - TotalUV=%d, TotalPV=%d, HourlyStats=%d, DailyStats=%d, TopURLs=%d",
			result.Summary.TotalUV, result.Summary.TotalPV,
			len(result.HourlyStats), len(result.DailyStats), len(result.TopURLs))
	} else {
		logger.Debugf("Analytics result is nil")
	}

	c.JSON(http.StatusOK, result)
}

// GetWorldMapData provides geographic data for world map visualization
func GetWorldMapData(c *gin.Context) {
	var req AnalyticsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, err)
		return
	}

	logger.Debugf("=== DEBUG GetWorldMapData START ===")
	logger.Debugf("WorldMapData request - Path: '%s', StartTime: %d, EndTime: %d, Limit: %d",
		req.Path, req.StartTime, req.EndTime, req.Limit)

	analyticsService := service.GetAnalytics()
	if analyticsService == nil {
		writeError(c, service.ErrModernAnalyticsNotAvailable)
		return
	}

	rawLogPath := req.Path
	safeLogPath := ""

	if rawLogPath != "" {
		decodedPath, err := decodeAndValidateLogPath(rawLogPath)
		if err != nil {
			writeError(c, err)
			return
		}
		safeLogPath = decodedPath
	}
	if rejectErrorLog(c, safeLogPath) {
		return
	}

	// Use default access log path if Path is empty
	if safeLogPath == "" {
		defaultLogPath := utils.DefaultAccessLogPath()
		if defaultLogPath != "" {
			safeLogPath = defaultLogPath
			logger.Debugf("Using default access log path for world map: %s", safeLogPath)
		}
	}

	// Validate log path if provided
	if safeLogPath != "" {
		if err := analyticsService.ValidateLogPath(safeLogPath); err != nil {
			writeError(c, err)
			return
		}
	}

	// Use main_log_path field for efficient log group queries instead of expanding file paths
	logger.Debugf("WorldMapData - Using main_log_path field for log group: %s", safeLogPath)

	// Get world map data with timeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	geoReq := &analytics.GeoQueryRequest{
		StartTime:      req.StartTime,
		EndTime:        endAfter(req.EndTime),
		LogPath:        safeLogPath,
		LogPaths:       []string{safeLogPath}, // Use single main log path
		UseMainLogPath: true,                  // Use main_log_path field for efficient queries
		Limit:          req.Limit,
	}
	logger.Debugf("WorldMapData - GeoQueryRequest: %+v", geoReq)

	data, err := analyticsService.GetGeoDistribution(ctx, geoReq)
	if err != nil {
		writeError(c, err)
		return
	}

	logger.Debugf("WorldMapData - GetGeoDistribution returned data with %d countries", len(data.Countries))
	for code, count := range data.Countries {
		if code == "CN" {
			logger.Debugf("WorldMapData - CN country count: %d", count)
		}
		logger.Debugf("WorldMapData - Country: '%s', Count: %d", code, count)
	}

	// Transform map to slice for frontend chart compatibility, calculate percentages, and sort.
	chartData := make([]GeoRegionItem, 0, len(data.Countries))
	totalValue := 0
	for _, value := range data.Countries {
		totalValue += value
	}
	logger.Debugf("WorldMapData - Total value calculated: %d", totalValue)

	for code, value := range data.Countries {
		percent := 0.0
		if totalValue > 0 {
			percent = (float64(value) / float64(totalValue)) * 100
		}
		chartData = append(chartData, GeoRegionItem{Code: code, Value: value, Percent: percent})
	}

	// Sort by value descending
	sort.Slice(chartData, func(i, j int) bool {
		return chartData[i].Value > chartData[j].Value
	})

	logger.Debugf("WorldMapData - Final response data contains %d items with total value %d", len(chartData), totalValue)
	for i, item := range chartData {
		if item.Code == "CN" {
			logger.Debugf("WorldMapData - FOUND CN - [%d] Code: '%s', Value: %d, Percent: %.2f%%", i, item.Code, item.Value, item.Percent)
		}
		logger.Debugf("WorldMapData - [%d] Code: '%s', Value: %d, Percent: %.2f%%", i, item.Code, item.Value, item.Percent)
	}
	logger.Debugf("=== DEBUG GetWorldMapData END ===")

	c.JSON(http.StatusOK, GeoRegionResponse{
		Data: chartData,
	})
}

// GetGeoStats provides geographic statistics
func GetGeoStats(c *gin.Context) {
	var req AnalyticsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "Invalid JSON request body: " + err.Error()})
		return
	}

	analyticsService := service.GetAnalytics()
	if analyticsService == nil {
		writeError(c, service.ErrModernAnalyticsNotAvailable)
		return
	}

	rawLogPath := req.Path
	safeLogPath := ""

	if rawLogPath != "" {
		decodedPath, err := decodeAndValidateLogPath(rawLogPath)
		if err != nil {
			writeError(c, err)
			return
		}
		safeLogPath = decodedPath
	}
	if rejectErrorLog(c, safeLogPath) {
		return
	}

	// Use default access log path if Path is empty
	if safeLogPath == "" {
		defaultLogPath := utils.DefaultAccessLogPath()
		if defaultLogPath != "" {
			safeLogPath = defaultLogPath
			logger.Debugf("Using default access log path for geo stats: %s", safeLogPath)
		}
	}

	// Validate log path if provided
	if safeLogPath != "" {
		if err := analyticsService.ValidateLogPath(safeLogPath); err != nil {
			writeError(c, err)
			return
		}
	}

	// Use main_log_path field for efficient log group queries instead of expanding file paths
	logger.Debugf("GeoStats - Using main_log_path field for log group: %s", safeLogPath)

	// Set default limit if not provided
	if req.Limit == 0 {
		req.Limit = 20
	}

	// Get geographic statistics with timeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	geoReq := &analytics.GeoQueryRequest{
		StartTime:      req.StartTime,
		EndTime:        endAfter(req.EndTime),
		LogPath:        safeLogPath,
		LogPaths:       []string{safeLogPath}, // Use single main log path
		UseMainLogPath: true,                  // Use main_log_path field for efficient queries
		Limit:          req.Limit,
	}

	stats, err := analyticsService.GetTopCountries(ctx, geoReq)
	if err != nil {
		writeError(c, err)
		return
	}

	// Convert to []interface{} for JSON serialization
	statsInterface := make([]interface{}, len(stats))
	for i, stat := range stats {
		statsInterface[i] = stat
	}

	c.JSON(http.StatusOK, GeoStatsResponse{
		Stats: statsInterface,
	})
}
