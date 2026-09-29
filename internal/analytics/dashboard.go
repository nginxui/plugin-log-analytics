package analytics

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/searcher"
)

// GetDashboardAnalytics generates comprehensive dashboard analytics
func (s *service) GetDashboardAnalytics(ctx context.Context, req *DashboardQueryRequest) (*DashboardAnalytics, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}

	if err := s.ValidateTimeRange(req.StartTime, req.EndTime); err != nil {
		return nil, fmt.Errorf("invalid time range: %w", err)
	}

	// One pass over the documents of the range fills every figure of the
	// dashboard. The scan covers the window plus the timezone buffer of the
	// hourly buckets, and the scanners keep the window's own figures apart.
	layout := newDashboardLayout(req.StartTime, req.EndTime)
	scanStart, scanEnd := layout.scanRange()
	searchReq := &searcher.SearchRequest{
		StartTime:      &scanStart,
		EndTime:        &scanEnd,
		LogPaths:       req.LogPaths,
		UseMainLogPath: true, // Use main_log_path field for efficient log group queries
		Limit:          -1,   // No documents needed, only the scan
		Scan: &searcher.ScanSpec{
			Fields: dashboardFields,
			New:    func() searcher.DocScanner { return newDashboardScanner(layout) },
		},
	}

	result, err := s.searcher.Search(ctx, searchReq)
	if err != nil {
		return nil, fmt.Errorf("failed to search logs for dashboard: %w", err)
	}
	figures := mergeDashboardScanners(layout, result.Scanned)

	// Initialize analytics with empty slices
	analytics := &DashboardAnalytics{}
	aggregates := &scanAggregates{
		TotalBytes:   figures.windowBytes,
		PeakMinutePV: figures.peakMinute,
	}

	// The lists describe the requested window, and so do the facet style
	// results the list builders read.
	windowResult := &searcher.SearchResult{
		TotalHits: uint64(figures.windowPV),
		Facets: map[string]*searcher.Facet{
			facetBrowser: {Field: facetBrowser, Terms: figures.groups[0]},
			facetOS:      {Field: facetOS, Terms: figures.groups[1]},
			facetDevice:  {Field: facetDevice, Terms: figures.groups[2]},
			facetURL:     {Field: facetURL, Terms: figures.groups[3]},
		},
	}

	if windowResult.TotalHits > 0 {
		analytics.HourlyStats = figures.hourly
		analytics.DailyStats = figures.daily
		analytics.TopURLs = s.calculateTopURLs(windowResult)
		analytics.Browsers = s.calculateBrowserStats(windowResult)
		analytics.OperatingSystems = s.calculateOSStats(windowResult)
		analytics.Devices = s.calculateDeviceStats(windowResult)
	} else {
		// Ensure slices are initialized even if there are no hits
		analytics.HourlyStats = make([]HourlyAccessStats, 0)
		analytics.DailyStats = make([]DailyAccessStats, 0)
		analytics.TopURLs = make([]URLAccessStats, 0)
		analytics.Browsers = make([]BrowserAccessStats, 0)
		analytics.OperatingSystems = make([]OSAccessStats, 0)
		analytics.Devices = make([]DeviceAccessStats, 0)
	}

	analytics.Summary = s.calculateDashboardSummary(analytics, windowResult, aggregates, req)
	analytics.Summary.TotalUV = figures.windowUV
	if len(analytics.DailyStats) > 0 {
		analytics.Summary.AvgDailyUV = float64(figures.windowUV) / float64(len(analytics.DailyStats))
	}

	return analytics, nil
}

// calculateHourlyStats calculates hourly access statistics.
// Returns 48 hours of data centered around the end_date to support all timezones.
func (s *service) calculateHourlyStats(result *searcher.SearchResult, startTime, endTime int64) []HourlyAccessStats {
	// Use a map with timestamp as key for easier processing
	hourlyMap := make(map[int64]*HourlyAccessStats)
	uniqueIPsPerHour := make(map[int64]map[string]bool)

	// Calculate 48 hours range: from UTC end_date minus 12 hours to plus 36 hours
	// This covers UTC-12 to UTC+14 timezones
	endDate := time.Unix(endTime, 0).UTC()
	endDateStart := time.Date(endDate.Year(), endDate.Month(), endDate.Day(), 0, 0, 0, 0, time.UTC)

	// Create hourly buckets for 48 hours (12 hours before to 36 hours after the UTC date boundary)
	rangeStart := endDateStart.Add(-12 * time.Hour)
	rangeEnd := endDateStart.Add(36 * time.Hour)

	// Initialize hourly buckets
	for t := rangeStart; t.Before(rangeEnd); t = t.Add(time.Hour) {
		timestamp := t.Unix()
		hourlyMap[timestamp] = &HourlyAccessStats{
			Hour:      t.Hour(),
			UV:        0,
			PV:        0,
			Timestamp: timestamp,
		}
		uniqueIPsPerHour[timestamp] = make(map[string]bool)
	}

	// Process search results - count hits within the 48-hour window
	for _, hit := range result.Hits {
		if timestampField, ok := hit.Fields["timestamp"]; ok {
			if timestampFloat, ok := timestampField.(float64); ok {
				timestamp := int64(timestampFloat)

				// Check if this hit falls within our 48-hour window
				if timestamp >= rangeStart.Unix() && timestamp < rangeEnd.Unix() {
					// Round down to the hour
					t := time.Unix(timestamp, 0).UTC()
					hourTimestamp := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Unix()

					if stats, exists := hourlyMap[hourTimestamp]; exists {
						stats.PV++
						if ipField, ok := hit.Fields["ip"]; ok {
							if ip, ok := ipField.(string); ok && ip != "" {
								if !uniqueIPsPerHour[hourTimestamp][ip] {
									uniqueIPsPerHour[hourTimestamp][ip] = true
									stats.UV++
								}
							}
						}
					}
				}
			}
		}
	}

	// Convert to slice and sort by timestamp
	var stats []HourlyAccessStats
	for _, stat := range hourlyMap {
		stats = append(stats, *stat)
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Timestamp < stats[j].Timestamp
	})

	return stats
}

// calculateDailyStats calculates daily access statistics
func (s *service) calculateDailyStats(result *searcher.SearchResult, startTime, endTime int64) []DailyAccessStats {
	dailyMap := make(map[string]*DailyAccessStats)
	uniqueIPsPerDay := make(map[string]map[string]bool)

	// Initialize daily buckets for the entire time range
	start := time.Unix(startTime, 0)
	end := time.Unix(endTime, 0)
	for t := start; t.Before(end) || t.Equal(end); t = t.AddDate(0, 0, 1) {
		dateStr := t.Format("2006-01-02")
		if _, exists := dailyMap[dateStr]; !exists {
			dailyMap[dateStr] = &DailyAccessStats{
				Date:      dateStr,
				UV:        0,
				PV:        0,
				Timestamp: t.Unix(),
			}
			uniqueIPsPerDay[dateStr] = make(map[string]bool)
		}
	}

	// Process search results
	for _, hit := range result.Hits {
		if timestampField, ok := hit.Fields["timestamp"]; ok {
			if timestampFloat, ok := timestampField.(float64); ok {
				timestamp := int64(timestampFloat)
				t := time.Unix(timestamp, 0)
				dateStr := t.Format("2006-01-02")

				if stats, exists := dailyMap[dateStr]; exists {
					stats.PV++
					if ipField, ok := hit.Fields["ip"]; ok {
						if ip, ok := ipField.(string); ok && ip != "" {
							if !uniqueIPsPerDay[dateStr][ip] {
								uniqueIPsPerDay[dateStr][ip] = true
								stats.UV++
							}
						}
					}
				}
			}
		}
	}

	// Convert to slice and sort
	var stats []DailyAccessStats
	for _, stat := range dailyMap {
		stats = append(stats, *stat)
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Timestamp < stats[j].Timestamp
	})

	return stats
}

// calculateTopURLs calculates top URL statistics from facets (legacy method)
func (s *service) calculateTopURLs(result *searcher.SearchResult) []URLAccessStats {
	if facet, ok := result.Facets["path_exact"]; ok {
		logger.Infof("📊 Facet-based URL calculation: facet.Total=%d, TotalHits=%d",
			facet.Total, result.TotalHits)

		urlStats := calculateTopFieldStats(facet, int(result.TotalHits), func(term string, count int, percent float64) URLAccessStats {
			return URLAccessStats{URL: term, Visits: count, Percent: percent}
		})

		logger.Infof("📈 Calculated %d URL stats from facet", len(urlStats))
		return urlStats
	} else {
		logger.Errorf("❌ path_exact facet not found in search results")
		return []URLAccessStats{}
	}
}

// calculateBrowserStats calculates browser statistics from facets
func (s *service) calculateBrowserStats(result *searcher.SearchResult) []BrowserAccessStats {
	return calculateTopFieldStats(result.Facets["browser"], int(result.TotalHits), func(term string, count int, percent float64) BrowserAccessStats {
		return BrowserAccessStats{Browser: term, Count: count, Percent: percent}
	})
}

// calculateOSStats calculates operating system statistics from facets
func (s *service) calculateOSStats(result *searcher.SearchResult) []OSAccessStats {
	return calculateTopFieldStats(result.Facets["os"], int(result.TotalHits), func(term string, count int, percent float64) OSAccessStats {
		return OSAccessStats{OS: term, Count: count, Percent: percent}
	})
}

// calculateDeviceStats calculates device statistics from facets
func (s *service) calculateDeviceStats(result *searcher.SearchResult) []DeviceAccessStats {
	return calculateTopFieldStats(result.Facets["device_type"], int(result.TotalHits), func(term string, count int, percent float64) DeviceAccessStats {
		return DeviceAccessStats{Device: term, Count: count, Percent: percent}
	})
}

// calculateTopFieldStats is a generic function to calculate top N items from a facet result.
func calculateTopFieldStats[T any](
	facet *searcher.Facet,
	totalHits int,
	creator func(term string, count int, percent float64) T,
) []T {
	if facet == nil || totalHits == 0 {
		return []T{}
	}

	var items []T
	for _, term := range facet.Terms {
		percent := float64(term.Count) / float64(totalHits) * 100
		items = append(items, creator(term.Term, term.Count, percent))
	}
	return items
}

// calculateDashboardSummary calculates summary statistics
func (s *service) calculateDashboardSummary(analytics *DashboardAnalytics, result *searcher.SearchResult, aggregates *scanAggregates, req *DashboardQueryRequest) DashboardSummary {
	// Calculate total UV from IP facet, which is now reliable.
	totalUV := 0
	if result.Facets != nil {
		if ipFacet, ok := result.Facets["ip"]; ok {
			// The total number of unique terms in the facet is the UV count.
			totalUV = ipFacet.Total
		}
	}

	totalPV := int(result.TotalHits)

	// Calculate average daily UV and PV
	var avgDailyUV, avgDailyPV float64
	if len(analytics.DailyStats) > 0 {
		var sumPV int
		for _, daily := range analytics.DailyStats {
			sumPV += daily.PV
		}
		// Use total unique visitors divided by number of days for accurate daily UV average
		// The totalUV represents unique visitors across the entire period, not sum of daily UVs
		avgDailyUV = float64(totalUV) / float64(len(analytics.DailyStats))
		avgDailyPV = float64(sumPV) / float64(len(analytics.DailyStats))
	}

	// Find peak hour
	var peakHour, peakHourTraffic int
	for _, hourly := range analytics.HourlyStats {
		if hourly.PV > peakHourTraffic {
			peakHour = hourly.Hour
			peakHourTraffic = hourly.PV
		}
	}

	// Average QPS spreads the request total over the whole queried range, so a
	// wide range dilutes it; peak QPS reports the busiest minute instead, which
	// is what a capacity question is usually about.
	var avgQPS, peakQPS float64
	if rangeSeconds := req.EndTime - req.StartTime; rangeSeconds > 0 {
		avgQPS = float64(totalPV) / float64(rangeSeconds)
	}
	if aggregates != nil {
		peakQPS = float64(aggregates.PeakMinutePV) / 60
	}

	summary := DashboardSummary{
		TotalUV:         totalUV,
		TotalPV:         totalPV,
		AvgDailyUV:      avgDailyUV,
		AvgDailyPV:      avgDailyPV,
		PeakHour:        peakHour,
		PeakHourTraffic: peakHourTraffic,
		AvgQPS:          avgQPS,
		PeakQPS:         peakQPS,
	}
	if aggregates != nil {
		summary.TotalTraffic = aggregates.TotalBytes
	}

	return summary
}

// scanAggregates holds the range-wide metrics collected by the dashboard scan.
type scanAggregates struct {
	// TotalBytes is the sum of bytes_sent over documents inside the requested
	// range, matching the basis of the summary's TotalPV.
	TotalBytes int64
	// PeakMinutePV is the request count of the busiest minute in the range.
	PeakMinutePV int
}
