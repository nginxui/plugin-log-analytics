package analytics

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/searcher"
)

func TestRegionSharesCountBothLevelsAndCityPointsCarryCoordinates(t *testing.T) {
	shard, err := bleve.New(filepath.Join(t.TempDir(), "shard.bleve"), indexer.CreateLogIndexMapping())
	require.NoError(t, err)
	t.Cleanup(func() { _ = shard.Close() })

	ts := time.Date(2022, time.January, 1, 12, 0, 0, 0, time.UTC).Unix()
	docs := []map[string]any{
		{"region_code": "FR", "sub1": "FR-IDF", "sub2": "FR-75", "city_point": "FR|Paris|48.86|2.35"},
		{"region_code": "FR", "sub1": "FR-IDF", "sub2": "FR-75", "city_point": "FR|Paris|48.86|2.35"},
		{"region_code": "FR", "sub1": "FR-ARA", "sub2": "FR-69", "city_point": "FR|Lyon|45.76|4.83"},
		{"region_code": "US", "sub1": "US-CA", "city_point": "US|San Jose|37.34|-121.89"},
	}
	batch := shard.NewBatch()
	for i, doc := range docs {
		doc["timestamp"] = float64(ts)
		doc["main_log_path"] = scanTestLogPath
		require.NoError(t, batch.Index(fmt.Sprintf("doc-%d", i), doc))
	}
	require.NoError(t, shard.Batch(batch))

	s := searcher.NewSearcher(searcher.DefaultSearcherConfig(), []bleve.Index{shard})
	t.Cleanup(func() { _ = s.Stop() })
	svc := NewService(s)
	req := &GeoQueryRequest{StartTime: ts - 60, EndTime: ts + 60, LogPaths: []string{scanTestLogPath}, UseMainLogPath: true, Limit: 10}

	shares, err := svc.GetRegionShares(context.Background(), req, "FR")
	require.NoError(t, err)
	byCode := map[string]RegionShare{}
	for _, share := range shares {
		byCode[share.Code] = share
	}
	assert.Equal(t, 2, byCode["FR-IDF"].Value)
	assert.InDelta(t, 66.67, byCode["FR-75"].Percent, 0.01)
	assert.Equal(t, 1, byCode["FR-69"].Value)
	assert.NotContains(t, byCode, "US-CA", "another country is left out")

	points, err := svc.GetCityPoints(context.Background(), req, "")
	require.NoError(t, err)
	require.Len(t, points, 3)
	assert.Equal(t, CityPoint{Country: "FR", City: "Paris", Lat: 48.86, Lon: 2.35, Value: 2, Percent: 50}, points[0])

	us, err := svc.GetCityPoints(context.Background(), req, "US")
	require.NoError(t, err)
	require.Len(t, us, 1)
	assert.InDelta(t, -121.89, us[0].Lon, 1e-9)
	assert.InDelta(t, 100.0, us[0].Percent, 1e-9)
}
