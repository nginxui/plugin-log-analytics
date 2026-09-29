package searcher

import (
	"fmt"
	"hash/maphash"
	"math"
	"sync"

	"github.com/blevesearch/bleve/v2/numeric"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/collector"
)

// DocScanner receives the doc values of every document a search matches. Each
// shard search gets its own scanner, so a scanner needs no locking. The doc
// values of one match arrive as Visit calls, followed by one EndDoc call.
type DocScanner interface {
	Visit(field string, term []byte)
	EndDoc()
}

// ScanSpec attaches scanners to a search. The scan runs in the same pass that
// finds the hits, so aggregates over the whole match set cost one walk of the
// index instead of one per statistic. Only fields mapped with doc values can be
// read this way.
type ScanSpec struct {
	// Fields are the doc value fields the scanner wants to see.
	Fields []string
	// New returns the scanner for one shard search.
	New func() DocScanner
}

// SummarySpec asks for aggregates over the whole match set.
type SummarySpec struct {
	// DistinctFields lists doc value text fields to count distinct values of.
	DistinctFields []string `json:"distinct_fields,omitempty"`
	// Bytes adds the total, minimum and maximum of bytes_sent.
	Bytes bool `json:"bytes,omitempty"`
}

// ScanSummary holds the aggregates a SummarySpec asked for.
type ScanSummary struct {
	// Docs is the size of the match set.
	Docs uint64 `json:"docs"`
	// Distinct maps a field to its number of distinct values.
	Distinct map[string]int `json:"distinct,omitempty"`

	TotalBytes int64 `json:"total_bytes"`
	MinBytes   int64 `json:"min_bytes"`
	MaxBytes   int64 `json:"max_bytes"`
}

// hashSeed keys the value hashes. Distinct values are counted by their 64 bit
// hash, which keeps the sets small; at a million distinct values the chance of
// one collision is around 3e-8.
var hashSeed = maphash.MakeSeed()

// HashTerm returns the 64 bit hash used to count distinct values.
func HashTerm(term []byte) uint64 {
	return maphash.Bytes(hashSeed, term)
}

// NumericTermValue decodes a doc value term of a numeric field. Numeric fields
// carry one term per precision step; only the full precision one is a value.
func NumericTermValue(term []byte) (float64, bool) {
	valid, shift := numeric.ValidPrefixCodedTermBytes(term)
	if !valid || shift != 0 {
		return 0, false
	}
	bits, err := numeric.PrefixCoded(term).Int64()
	if err != nil {
		return 0, false
	}
	return numeric.Int64ToFloat64(bits), true
}

// scanSink collects the scanners created for the shard searches of one Search
// call.
type scanSink struct {
	fields  []string
	factory []func() DocScanner
	scanned [][]DocScanner

	mu sync.Mutex
}

func newScanSink(req *SearchRequest) *scanSink {
	sink := &scanSink{}
	seen := make(map[string]struct{})
	addFields := func(fields ...string) {
		for _, field := range fields {
			if _, ok := seen[field]; ok {
				continue
			}
			seen[field] = struct{}{}
			sink.fields = append(sink.fields, field)
		}
	}

	if req.Summary != nil {
		spec := req.Summary
		addFields(spec.DistinctFields...)
		if spec.Bytes {
			addFields("bytes_sent")
		}
		sink.factory = append(sink.factory, func() DocScanner { return newSummaryScanner(spec) })
	}
	if req.Scan != nil {
		addFields(req.Scan.Fields...)
		sink.factory = append(sink.factory, req.Scan.New)
	}
	return sink
}

// handlerMaker returns the hook Bleve calls once per shard search. The hook
// wraps the default top-N handler, so hits are ranked as usual, and shows each
// match to the scanners before handing it on.
func (s *scanSink) handlerMaker() search.MakeDocumentMatchHandler {
	return func(sc *search.SearchContext) (search.DocumentMatchHandler, bool, error) {
		inner, loadID, err := collector.MakeTopNDocumentMatchHandler(sc)
		if err != nil {
			return nil, false, err
		}
		if inner == nil {
			return nil, false, fmt.Errorf("scan needs the top-n collector")
		}

		reader, err := sc.IndexReader.DocValueReader(s.fields)
		if err != nil {
			return nil, false, err
		}

		scanners := make([]DocScanner, len(s.factory))
		for i, newScanner := range s.factory {
			scanners[i] = newScanner()
		}
		s.mu.Lock()
		s.scanned = append(s.scanned, scanners)
		s.mu.Unlock()

		visit := func(field string, term []byte) {
			for _, scanner := range scanners {
				scanner.Visit(field, term)
			}
		}

		return func(d *search.DocumentMatch) error {
			if d != nil {
				if err := reader.VisitDocValues(d.IndexInternalID, visit); err != nil {
					return err
				}
				for _, scanner := range scanners {
					scanner.EndDoc()
				}
			}
			return inner(d)
		}, loadID, nil
	}
}

// results returns, for every scanner slot, the scanners of all shard searches.
func (s *scanSink) results() [][]DocScanner {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([][]DocScanner, len(s.factory))
	for _, scanners := range s.scanned {
		for i, scanner := range scanners {
			out[i] = append(out[i], scanner)
		}
	}
	return out
}

// summaryScanner computes a ScanSummary for one shard.
type summaryScanner struct {
	slots    map[string]int
	distinct []map[uint64]struct{}

	docs       uint64
	withBytes  bool
	totalBytes int64
	minBytes   int64
	maxBytes   int64
}

func newSummaryScanner(spec *SummarySpec) *summaryScanner {
	s := &summaryScanner{
		slots:     make(map[string]int, len(spec.DistinctFields)),
		distinct:  make([]map[uint64]struct{}, len(spec.DistinctFields)),
		withBytes: spec.Bytes,
		minBytes:  math.MaxInt64,
	}
	for i, field := range spec.DistinctFields {
		s.slots[field] = i
		s.distinct[i] = make(map[uint64]struct{})
	}
	return s
}

func (s *summaryScanner) Visit(field string, term []byte) {
	if slot, ok := s.slots[field]; ok {
		s.distinct[slot][HashTerm(term)] = struct{}{}
		return
	}
	if s.withBytes && field == "bytes_sent" {
		if value, ok := NumericTermValue(term); ok {
			b := int64(value)
			s.totalBytes += b
			if b < s.minBytes {
				s.minBytes = b
			}
			if b > s.maxBytes {
				s.maxBytes = b
			}
		}
	}
}

func (s *summaryScanner) EndDoc() { s.docs++ }

// mergeSummary combines the shard scanners into one summary.
func mergeSummary(spec *SummarySpec, scanners []DocScanner) *ScanSummary {
	summary := &ScanSummary{Distinct: make(map[string]int, len(spec.DistinctFields))}
	union := make([]map[uint64]struct{}, len(spec.DistinctFields))
	minBytes := int64(math.MaxInt64)

	for _, scanner := range scanners {
		shard, ok := scanner.(*summaryScanner)
		if !ok {
			continue
		}
		summary.Docs += shard.docs
		summary.TotalBytes += shard.totalBytes
		if shard.minBytes < minBytes {
			minBytes = shard.minBytes
		}
		if shard.maxBytes > summary.MaxBytes {
			summary.MaxBytes = shard.maxBytes
		}
		for i := range union {
			switch {
			case union[i] == nil:
				union[i] = shard.distinct[i]
			default:
				for h := range shard.distinct[i] {
					union[i][h] = struct{}{}
				}
			}
		}
	}

	if minBytes != math.MaxInt64 {
		summary.MinBytes = minBytes
	}
	for i, field := range spec.DistinctFields {
		summary.Distinct[field] = len(union[i])
	}
	return summary
}
