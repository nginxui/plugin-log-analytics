package analytics

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/searcher"
)

// legacyDashboard is the dashboard as it was computed before the single pass
// scan: paged searches for the time buckets, a facet search for the lists and
// the cardinality counter for the visitors. It stays here as the reference the
// scan has to agree with.
type legacyDashboard struct {
	*service
	counter *searcher.Counter
}

// GetDashboardAnalytics generates comprehensive dashboard analytics
func (s *legacyDashboard) analytics(ctx context.Context, req *DashboardQueryRequest) (*DashboardAnalytics, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}

	if err := s.ValidateTimeRange(req.StartTime, req.EndTime); err != nil {
		return nil, fmt.Errorf("invalid time range: %w", err)
	}

	searchReq := &searcher.SearchRequest{
		StartTime:      &req.StartTime,
		EndTime:        &req.EndTime,
		LogPaths:       req.LogPaths,
		UseMainLogPath: true, // Use main_log_path field for efficient log group queries
		IncludeFacets:  true,
		FacetFields:    []string{"browser", "os", "device_type"}, // Removed 'ip' to reduce facet computation
		FacetSize:      50,                                       // Significantly reduced for faster facet computation
		UseCache:       true,
		SortBy:         "timestamp",
		SortOrder:      "desc",
		Limit:          -1, // Facet/aggregation-only query, no documents needed
	}

	// Execute search
	result, err := s.searcher.Search(ctx, searchReq)
	if err != nil {
		return nil, fmt.Errorf("failed to search logs for dashboard: %w", err)
	}

	// Initialize analytics with empty slices
	analytics := &DashboardAnalytics{}
	aggregates := &scanAggregates{}

	// Calculate analytics if we have results
	if result.TotalHits > 0 {
		// For now, use batch queries to get complete data
		analytics.HourlyStats, analytics.DailyStats, aggregates = s.calculateTimeBucketStats(ctx, req)

		// Use cardinality counter for efficient unique URLs counting
		analytics.TopURLs = s.calculateTopURLsWithCardinality(ctx, req)

		analytics.Browsers = s.calculateBrowserStats(result)
		analytics.OperatingSystems = s.calculateOSStats(result)
		analytics.Devices = s.calculateDeviceStats(result)
	} else {
		// Ensure slices are initialized even if there are no hits
		analytics.HourlyStats = make([]HourlyAccessStats, 0)
		analytics.DailyStats = make([]DailyAccessStats, 0)
		analytics.TopURLs = make([]URLAccessStats, 0)
		analytics.Browsers = make([]BrowserAccessStats, 0)
		analytics.OperatingSystems = make([]OSAccessStats, 0)
		analytics.Devices = make([]DeviceAccessStats, 0)
	}

	// Calculate summary with cardinality counting for accurate unique pages
	analytics.Summary = s.calculateDashboardSummaryWithCardinality(ctx, analytics, result, req, aggregates)

	return analytics, nil
}

// calculateTopURLsWithCardinality calculates top URL statistics using facet-based approach
// Always returns actual top URLs with their visit counts instead of just a summary
func (s *legacyDashboard) calculateTopURLsWithCardinality(ctx context.Context, req *DashboardQueryRequest) []URLAccessStats {
	// Always use facet-based calculation to get actual top URLs with visit counts
	searchReq := &searcher.SearchRequest{
		StartTime:      &req.StartTime,
		EndTime:        &req.EndTime,
		LogPaths:       req.LogPaths,
		UseMainLogPath: true, // Use main_log_path for efficient log group queries
		IncludeFacets:  true,
		FacetFields:    []string{"path_exact"},
		FacetSize:      100, // Reasonable facet size to get top URLs
		UseCache:       true,
		Limit:          -1, // Facet-only query, no documents needed
	}

	result, err := s.searcher.Search(ctx, searchReq)
	if err != nil {
		logger.Errorf("Failed to search for URL facets: %v", err)
		return []URLAccessStats{}
	}

	// Get actual top URLs with visit counts
	return s.calculateTopURLs(result)
}

// calculateDashboardSummaryWithCardinality calculates enhanced summary statistics using cardinality counters
func (s *legacyDashboard) calculateDashboardSummaryWithCardinality(ctx context.Context, analytics *DashboardAnalytics, result *searcher.SearchResult, req *DashboardQueryRequest, aggregates *scanAggregates) DashboardSummary {
	// Start with the basic summary but we'll override the UV calculation
	summary := s.calculateDashboardSummary(analytics, result, aggregates, req)

	// Use cardinality counter for accurate unique visitor (UV) counting if available
	cardinalityCounter := s.counter
	if cardinalityCounter != nil {
		// Count unique IPs (visitors) using cardinality counter instead of limited facet
		uvCardReq := &searcher.CardinalityRequest{
			Field:          "ip",
			StartTime:      &req.StartTime,
			EndTime:        &req.EndTime,
			LogPaths:       req.LogPaths,
			UseMainLogPath: true, // Use main_log_path for efficient log group queries
		}

		if uvResult, err := cardinalityCounter.Count(ctx, uvCardReq); err == nil {
			// Override the facet-limited UV count with accurate cardinality count
			summary.TotalUV = int(uvResult.Cardinality)

			// Recalculate average daily UV with accurate count
			if len(analytics.DailyStats) > 0 {
				summary.AvgDailyUV = float64(summary.TotalUV) / float64(len(analytics.DailyStats))
			}

		} else {
			logger.Errorf("Failed to count unique visitors with cardinality counter: %v", err)
		}
	} else {
		logger.Warnf("Counter not available, UV count limited by facet size to %d", summary.TotalUV)
	}

	return summary
}

// calculateTimeBucketStats computes hourly and daily UV/PV statistics, total
// traffic and the peak-minute request count in a single pass over the matching
// documents. Pagination uses a SearchAfter cursor on the (timestamp, _id) sort
// key: each page costs O(page) instead of the O(offset+page) of offset
// pagination, so a full scan stays linear in the number of documents.
func (s *legacyDashboard) calculateTimeBucketStats(ctx context.Context, req *DashboardQueryRequest) ([]HourlyAccessStats, []DailyAccessStats, *scanAggregates) {
	// Daily buckets cover the requested range (dates in server-local time,
	// matching the rest of the dashboard).
	dailyMap := make(map[string]*DailyAccessStats)
	uniqueIPsPerDay := make(map[string]map[string]bool)

	start := time.Unix(req.StartTime, 0)
	end := time.Unix(req.EndTime, 0)
	for t := start; t.Before(end) || t.Equal(end); t = t.AddDate(0, 0, 1) {
		dateStr := t.Format("2006-01-02")
		if _, exists := dailyMap[dateStr]; !exists {
			dailyMap[dateStr] = &DailyAccessStats{
				Date:      dateStr,
				Timestamp: t.Unix(),
			}
			uniqueIPsPerDay[dateStr] = make(map[string]bool)
		}
	}

	// Hourly buckets cover the requested range plus a timezone buffer
	// (12 hours on each side, covering UTC-12 to UTC+12).
	hourlyMap := make(map[int64]*HourlyAccessStats)
	uniqueIPsPerHour := make(map[int64]map[string]bool)

	rangeStart := time.Unix(req.StartTime, 0).UTC().Add(-12 * time.Hour)
	rangeEnd := time.Unix(req.EndTime, 0).UTC().Add(12 * time.Hour)
	for t := rangeStart; t.Before(rangeEnd); t = t.Add(time.Hour) {
		timestamp := t.Unix()
		hourlyMap[timestamp] = &HourlyAccessStats{
			Hour:      t.Hour(),
			Timestamp: timestamp,
		}
		uniqueIPsPerHour[timestamp] = make(map[string]bool)
	}

	// One scan over the wider (hourly) range feeds both bucket sets; documents
	// outside the daily range simply miss the daily map and are skipped there.
	scanStart := rangeStart.Unix()
	scanEnd := rangeEnd.Unix()
	const batchSize = 10000

	// Traffic and peak-rate figures describe the requested range only, so they
	// ignore the timezone buffer the hourly buckets need.
	aggregates := &scanAggregates{}
	perMinutePV := make(map[int64]int)

	var searchAfter []string
	totalProcessed := 0

	for {
		searchReq := &searcher.SearchRequest{
			StartTime:      &scanStart,
			EndTime:        &scanEnd,
			LogPaths:       req.LogPaths,
			UseMainLogPath: true, // Use main_log_path for efficient log group queries
			Limit:          batchSize,
			SearchAfter:    searchAfter,
			SortBy:         "timestamp",
			SortOrder:      "asc",
			Fields:         []string{"timestamp", "ip", "bytes_sent"},
			UseCache:       false, // Don't cache intermediate scan pages
		}

		result, err := s.searcher.Search(ctx, searchReq)
		if err != nil {
			logger.Errorf("Failed to fetch time-bucket batch (processed %d): %v", totalProcessed, err)
			break
		}

		for _, hit := range result.Hits {
			timestampField, ok := hit.Fields["timestamp"]
			if !ok {
				continue
			}
			timestampFloat, ok := timestampField.(float64)
			if !ok {
				continue
			}
			timestamp := int64(timestampFloat)

			var ip string
			if ipField, ok := hit.Fields["ip"]; ok {
				ip, _ = ipField.(string)
			}

			// Range-wide aggregates, restricted to the requested window
			// Bleve's timestamp range is half-open: include StartTime and
			// exclude EndTime. Keep aggregates on the same document set as
			// TotalPV even though this scan uses a wider timezone buffer.
			if timestamp >= req.StartTime && timestamp < req.EndTime {
				if bytesField, ok := hit.Fields["bytes_sent"]; ok {
					if bytesSent, ok := bytesField.(float64); ok {
						aggregates.TotalBytes += int64(bytesSent)
					}
				}
				perMinutePV[timestamp-timestamp%60]++
			}

			// Daily bucket (server-local date)
			dateStr := time.Unix(timestamp, 0).Format("2006-01-02")
			if stats, exists := dailyMap[dateStr]; exists {
				stats.PV++
				if ip != "" && !uniqueIPsPerDay[dateStr][ip] {
					uniqueIPsPerDay[dateStr][ip] = true
					stats.UV++
				}
			}

			// Hourly bucket (UTC hour)
			t := time.Unix(timestamp, 0).UTC()
			hourTimestamp := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Unix()
			if stats, exists := hourlyMap[hourTimestamp]; exists {
				stats.PV++
				if ip != "" && !uniqueIPsPerHour[hourTimestamp][ip] {
					uniqueIPsPerHour[hourTimestamp][ip] = true
					stats.UV++
				}
			}
		}

		totalProcessed += len(result.Hits)

		if len(result.Hits) < batchSize {
			break
		}

		lastHit := result.Hits[len(result.Hits)-1]
		if len(lastHit.Sort) == 0 {
			logger.Warnf("Time-bucket scan: last hit carries no sort values, cannot continue pagination (processed %d)", totalProcessed)
			break
		}
		searchAfter = lastHit.Sort
	}

	for _, pv := range perMinutePV {
		if pv > aggregates.PeakMinutePV {
			aggregates.PeakMinutePV = pv
		}
	}

	logger.Debugf("Time-bucket stats completed: %d records into %d hourly / %d daily buckets, %d bytes",
		totalProcessed, len(hourlyMap), len(dailyMap), aggregates.TotalBytes)

	// Convert to sorted slices
	hourlyStats := make([]HourlyAccessStats, 0, len(hourlyMap))
	for _, stat := range hourlyMap {
		hourlyStats = append(hourlyStats, *stat)
	}
	sort.Slice(hourlyStats, func(i, j int) bool {
		return hourlyStats[i].Timestamp < hourlyStats[j].Timestamp
	})

	dailyStats := make([]DailyAccessStats, 0, len(dailyMap))
	for _, stat := range dailyMap {
		dailyStats = append(dailyStats, *stat)
	}
	sort.Slice(dailyStats, func(i, j int) bool {
		return dailyStats[i].Timestamp < dailyStats[j].Timestamp
	})

	return hourlyStats, dailyStats, aggregates
}

const scanTestLogPath = "/var/log/nginx/access.log"

// scanFixture is a two shard index of generated requests around a window of
// two days that starts at a UTC midnight. Some requests fall before and after
// the window, inside the timezone buffer and beyond it.
type scanFixture struct {
	searcher *searcher.Searcher
	shards   []bleve.Index
	start    int64
	end      int64
	docs     []map[string]any
}

func newScanFixture(t *testing.T, count int) *scanFixture {
	t.Helper()

	start := time.Date(2022, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	fixture := &scanFixture{start: start, end: start + 2*86400}

	mapping := indexer.CreateLogIndexMapping()
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		shard, err := bleve.New(filepath.Join(dir, fmt.Sprintf("shard%d.bleve", i)), mapping)
		require.NoError(t, err)
		t.Cleanup(func() { _ = shard.Close() })
		fixture.shards = append(fixture.shards, shard)
	}

	browsers := []string{"Chrome", "Firefox", "Safari", "Edge"}
	systems := []string{"Windows", "macOS", "Linux", "Android"}
	devices := []string{"Desktop", "Mobile"}
	rng := rand.New(rand.NewSource(7))
	batches := []*bleve.Batch{fixture.shards[0].NewBatch(), fixture.shards[1].NewBatch()}
	for i := 0; i < count; i++ {
		// Ten hours before the window to ten hours after it.
		ts := start - 10*3600 + rng.Int63n(2*86400+20*3600)
		doc := map[string]any{
			"timestamp":     float64(ts),
			"ip":            fmt.Sprintf("10.0.%d.%d", rng.Intn(4), rng.Intn(30)),
			"path_exact":    fmt.Sprintf("/page/%d", rng.Intn(25)),
			"browser":       browsers[rng.Intn(len(browsers))],
			"os":            systems[rng.Intn(len(systems))],
			"device_type":   devices[rng.Intn(len(devices))],
			"bytes_sent":    float64(rng.Intn(5000)),
			"main_log_path": scanTestLogPath,
			"file_path":     scanTestLogPath,
			"status":        float64(200 + 100*rng.Intn(3)),
		}
		fixture.docs = append(fixture.docs, doc)
		require.NoError(t, batches[i%2].Index(fmt.Sprintf("doc-%d", i), doc))
	}
	for i, batch := range batches {
		require.NoError(t, fixture.shards[i].Batch(batch))
	}

	fixture.searcher = searcher.NewSearcher(searcher.DefaultSearcherConfig(), fixture.shards)
	t.Cleanup(func() { _ = fixture.searcher.Stop() })
	return fixture
}

func TestDashboardScanMatchesThePagedComputation(t *testing.T) {
	fixture := newScanFixture(t, 1500)
	ctx := context.Background()

	req := &DashboardQueryRequest{
		StartTime: fixture.start,
		EndTime:   fixture.end,
		LogPaths:  []string{scanTestLogPath},
	}

	got, err := NewService(fixture.searcher).GetDashboardAnalytics(ctx, req)
	require.NoError(t, err)

	counter := searcher.NewCounter(fixture.shards)
	t.Cleanup(func() { _ = counter.Stop() })
	legacy := &legacyDashboard{service: &service{searcher: fixture.searcher}, counter: counter}
	want, err := legacy.analytics(ctx, req)
	require.NoError(t, err)

	require.Positive(t, want.Summary.TotalPV)
	assert.Equal(t, want.Summary, got.Summary)
	assert.Equal(t, want.HourlyStats, got.HourlyStats)
	assert.Equal(t, want.DailyStats, got.DailyStats)
	assert.Equal(t, want.TopURLs, got.TopURLs)
	assert.Equal(t, want.Browsers, got.Browsers)
	assert.Equal(t, want.OperatingSystems, got.OperatingSystems)
	assert.Equal(t, want.Devices, got.Devices)
}

func TestDashboardScanFigures(t *testing.T) {
	fixture := newScanFixture(t, 800)

	got, err := NewService(fixture.searcher).GetDashboardAnalytics(context.Background(), &DashboardQueryRequest{
		StartTime: fixture.start,
		EndTime:   fixture.end,
		LogPaths:  []string{scanTestLogPath},
	})
	require.NoError(t, err)

	// The same figures worked out straight from the documents.
	var pv int
	var traffic int64
	visitors := map[string]struct{}{}
	pages := map[string]int{}
	minutes := map[int64]int{}
	for _, doc := range fixture.docs {
		ts := int64(doc["timestamp"].(float64))
		if ts < fixture.start || ts >= fixture.end {
			continue
		}
		pv++
		traffic += int64(doc["bytes_sent"].(float64))
		visitors[doc["ip"].(string)] = struct{}{}
		pages[doc["path_exact"].(string)]++
		minutes[ts-ts%60]++
	}
	peak := 0
	for _, count := range minutes {
		peak = max(peak, count)
	}

	assert.Equal(t, pv, got.Summary.TotalPV)
	assert.Equal(t, len(visitors), got.Summary.TotalUV)
	assert.Equal(t, traffic, got.Summary.TotalTraffic)
	assert.InDelta(t, float64(peak)/60, got.Summary.PeakQPS, 1e-9)

	var topCount int
	for _, count := range pages {
		topCount = max(topCount, count)
	}
	require.NotEmpty(t, got.TopURLs)
	assert.Equal(t, topCount, got.TopURLs[0].Visits)
	assert.True(t, sort.SliceIsSorted(got.TopURLs, func(i, j int) bool { return got.TopURLs[i].Visits > got.TopURLs[j].Visits }))

	// The hourly buckets reach twelve hours past each side of the window.
	require.Len(t, got.HourlyStats, 48+24)
	assert.Equal(t, fixture.start-12*3600, got.HourlyStats[0].Timestamp)
}

func TestDashboardScanEmptyWindow(t *testing.T) {
	fixture := newScanFixture(t, 50)

	// A window with no requests, though the buffer around it holds some.
	start := fixture.end + 3*86400
	got, err := NewService(fixture.searcher).GetDashboardAnalytics(context.Background(), &DashboardQueryRequest{
		StartTime: start,
		EndTime:   start + 86400,
		LogPaths:  []string{scanTestLogPath},
	})
	require.NoError(t, err)
	assert.Empty(t, got.HourlyStats)
	assert.Empty(t, got.TopURLs)
	assert.Zero(t, got.Summary.TotalPV)
	assert.Zero(t, got.Summary.TotalUV)
}
