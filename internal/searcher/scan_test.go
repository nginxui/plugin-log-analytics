package searcher

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/indexer"
)

const scanTestDocs = 300

// scanTestDoc is what the test index holds, kept to work out the expected
// aggregates without asking the index.
type scanTestDoc struct {
	ip     string
	path   string
	status int
	bytes  int
	ts     int64
}

// newScanTestSearcher builds a two shard searcher over the production mapping.
// The documents alternate between the shards.
func newScanTestSearcher(t *testing.T, sameTimestamp bool) (*Searcher, []scanTestDoc) {
	t.Helper()

	shards := make([]bleve.Index, 2)
	batches := make([]*bleve.Batch, 2)
	for i := range shards {
		shard, err := bleve.NewMemOnly(indexer.CreateLogIndexMapping())
		require.NoError(t, err)
		t.Cleanup(func() { _ = shard.Close() })
		shards[i] = shard
		batches[i] = shard.NewBatch()
	}

	docs := make([]scanTestDoc, 0, scanTestDocs)
	for i := 0; i < scanTestDocs; i++ {
		doc := scanTestDoc{
			ip:     fmt.Sprintf("10.0.0.%d", i%37),
			path:   fmt.Sprintf("/page/%d", i%11),
			status: []int{200, 404, 500}[i%3],
			bytes:  100 + i*7,
			ts:     1700000000 + int64(i),
		}
		if sameTimestamp {
			doc.ts = 1700000000
		}
		docs = append(docs, doc)
		require.NoError(t, batches[i%2].Index(fmt.Sprintf("doc-%03d", i), map[string]any{
			"timestamp":     float64(doc.ts),
			"ip":            doc.ip,
			"path_exact":    doc.path,
			"status":        float64(doc.status),
			"bytes_sent":    float64(doc.bytes),
			"file_path":     "/var/log/nginx/access.log",
			"main_log_path": "/var/log/nginx/access.log",
		}))
	}
	for i, batch := range batches {
		require.NoError(t, shards[i].Batch(batch))
	}

	searcher := NewSearcher(DefaultSearcherConfig(), shards)
	t.Cleanup(func() { _ = searcher.Stop() })
	return searcher, docs
}

func summarize(docs []scanTestDoc, keep func(scanTestDoc) bool) *ScanSummary {
	summary := &ScanSummary{Distinct: map[string]int{}}
	ips, paths := map[string]struct{}{}, map[string]struct{}{}
	for _, doc := range docs {
		if !keep(doc) {
			continue
		}
		summary.Docs++
		ips[doc.ip] = struct{}{}
		paths[doc.path] = struct{}{}
		summary.TotalBytes += int64(doc.bytes)
		if summary.MinBytes == 0 || int64(doc.bytes) < summary.MinBytes {
			summary.MinBytes = int64(doc.bytes)
		}
		summary.MaxBytes = max(summary.MaxBytes, int64(doc.bytes))
	}
	summary.Distinct["ip"] = len(ips)
	summary.Distinct["path_exact"] = len(paths)
	return summary
}

func TestSearchSummaryCoversTheWholeMatchSet(t *testing.T) {
	searcher, docs := newScanTestSearcher(t, false)
	spec := &SummarySpec{DistinctFields: []string{"ip", "path_exact"}, Bytes: true}

	cases := map[string]struct {
		req  *SearchRequest
		keep func(scanTestDoc) bool
	}{
		"everything": {
			req:  &SearchRequest{Limit: 5},
			keep: func(scanTestDoc) bool { return true },
		},
		"status filter": {
			req:  &SearchRequest{Limit: 5, StatusCodes: []int{500}},
			keep: func(d scanTestDoc) bool { return d.status == 500 },
		},
		"time range": {
			req: &SearchRequest{Limit: 5, StartTime: ptr(int64(1700000100)), EndTime: ptr(int64(1700000200))},
			keep: func(d scanTestDoc) bool {
				return d.ts >= 1700000100 && d.ts < 1700000200
			},
		},
		"no hits requested": {
			req:  &SearchRequest{Limit: -1, IPAddresses: []string{"10.0.0.3"}},
			keep: func(d scanTestDoc) bool { return d.ip == "10.0.0.3" },
		},
		"no match": {
			req:  &SearchRequest{Limit: 5, IPAddresses: []string{"192.0.2.1"}},
			keep: func(scanTestDoc) bool { return false },
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.req.Summary = spec
			result, err := searcher.Search(context.Background(), tc.req)
			require.NoError(t, err)

			want := summarize(docs, tc.keep)
			require.NotNil(t, result.Summary)
			assert.Equal(t, want.Docs, result.TotalHits)
			assert.Equal(t, want.Docs, result.Summary.Docs)
			assert.Equal(t, want.Distinct, result.Summary.Distinct)
			assert.Equal(t, want.TotalBytes, result.Summary.TotalBytes)
			assert.Equal(t, want.MinBytes, result.Summary.MinBytes)
			assert.Equal(t, want.MaxBytes, result.Summary.MaxBytes)
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestSearchSummaryIsCachedButScansAreNot(t *testing.T) {
	searcher, _ := newScanTestSearcher(t, false)
	spec := &SummarySpec{DistinctFields: []string{"ip"}}

	first, err := searcher.Search(context.Background(), &SearchRequest{Limit: 5, UseCache: true, Summary: spec})
	require.NoError(t, err)
	second, err := searcher.Search(context.Background(), &SearchRequest{Limit: 5, UseCache: true, Summary: spec})
	require.NoError(t, err)
	assert.True(t, second.CacheHit)
	assert.Equal(t, first.Summary, second.Summary)

	// A request without the summary must not be answered by the entry above.
	plain, err := searcher.Search(context.Background(), &SearchRequest{Limit: 5, UseCache: true})
	require.NoError(t, err)
	assert.Nil(t, plain.Summary)

	var seen atomic.Int64
	scan := func() *ScanSpec {
		return &ScanSpec{
			Fields: []string{"ip"},
			New:    func() DocScanner { return &countingScanner{seen: &seen} },
		}
	}
	for i := 0; i < 2; i++ {
		seen.Store(0)
		result, err := searcher.Search(context.Background(), &SearchRequest{Limit: -1, UseCache: true, Scan: scan()})
		require.NoError(t, err)
		assert.False(t, result.CacheHit)
		assert.Len(t, result.Scanned, 2, "one scanner per shard")
		assert.EqualValues(t, scanTestDocs, seen.Load())
	}
}

type countingScanner struct {
	seen *atomic.Int64
}

func (c *countingScanner) Visit(string, []byte) {}
func (c *countingScanner) EndDoc()              { c.seen.Add(1) }

func TestSearchPagesStayDisjointWhenTimestampsTie(t *testing.T) {
	searcher, _ := newScanTestSearcher(t, true)

	// Every document shares one timestamp, so the order between them comes from
	// the index alone. Paging through it must still visit each document once.
	seen := map[string]int{}
	var firstRun []string
	for run := 0; run < 2; run++ {
		var ids []string
		for offset := 0; offset < scanTestDocs; offset += 25 {
			result, err := searcher.Search(context.Background(), &SearchRequest{
				Limit: 25, Offset: offset, SortBy: "timestamp", SortOrder: SortOrderDesc, Fields: []string{"ip"},
			})
			require.NoError(t, err)
			for _, hit := range result.Hits {
				ids = append(ids, hit.ID)
				if run == 0 {
					seen[hit.ID]++
				}
			}
		}
		if run == 0 {
			firstRun = ids
		} else {
			assert.Equal(t, firstRun, ids, "the order is stable between runs")
		}
	}

	assert.Len(t, seen, scanTestDocs)
	for id, count := range seen {
		assert.Equal(t, 1, count, id)
	}
}
