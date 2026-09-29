package analytics

import (
	"sort"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/searcher"
)

const (
	facetBrowser = "browser"
	facetOS      = "os"
	facetDevice  = "device_type"
	facetURL     = "path_exact"

	// The dashboard lists the top 50 browsers, systems and device types and the
	// top 100 URLs.
	topGroupSize = 50
	topURLSize   = 100
)

// dashboardFields are the doc value fields the dashboard scan reads.
var dashboardFields = []string{"timestamp", "ip", "bytes_sent", facetBrowser, facetOS, facetDevice, facetURL}

// dashboardLayout describes the buckets a dashboard scan fills. It is shared,
// read only, by the scanners of all shards.
type dashboardLayout struct {
	// window is the requested range [start, end). The traffic totals, the
	// visitor count, the peak rate and the top lists describe it.
	start, end int64

	// The hourly buckets start at hourStart and hold one UTC hour each. The
	// range reaches past the window by the timezone buffer.
	hourStart int64
	hourCount int

	// dailyIndex maps a server-local date to its bucket. The dates are those of
	// the window, one per day.
	dailyIndex map[int32]int
	dailyKeys  []int32
	dailyStamp []int64
	dailyDate  []string
}

// newDashboardLayout prepares the buckets for the requested range. The buckets
// are the ones the dashboard has always had: daily ones in server-local time,
// hourly ones with a timezone buffer of 12 hours on each side.
func newDashboardLayout(start, end int64) *dashboardLayout {
	layout := &dashboardLayout{
		start:      start,
		end:        end,
		dailyIndex: make(map[int32]int),
	}

	first := time.Unix(start, 0)
	last := time.Unix(end, 0)
	for t := first; !t.After(last); t = t.AddDate(0, 0, 1) {
		key := dateKey(t)
		if _, exists := layout.dailyIndex[key]; exists {
			continue
		}
		layout.dailyIndex[key] = len(layout.dailyKeys)
		layout.dailyKeys = append(layout.dailyKeys, key)
		layout.dailyStamp = append(layout.dailyStamp, t.Unix())
		layout.dailyDate = append(layout.dailyDate, t.Format("2006-01-02"))
	}

	layout.hourStart = time.Unix(start, 0).UTC().Add(-12 * time.Hour).Unix()
	hourEnd := time.Unix(end, 0).UTC().Add(12 * time.Hour).Unix()
	if hourEnd > layout.hourStart {
		layout.hourCount = int((hourEnd - layout.hourStart + 3599) / 3600)
	}
	return layout
}

// scanRange is the time range one scan has to cover: the window plus the
// timezone buffer.
func (l *dashboardLayout) scanRange() (int64, int64) {
	return l.hourStart, time.Unix(l.end, 0).UTC().Add(12 * time.Hour).Unix()
}

func dateKey(t time.Time) int32 {
	year, month, day := t.Date()
	return int32(year*10000 + int(month)*100 + day)
}

// spanAccumulator counts requests and distinct visitors of one bucket.
type spanAccumulator struct {
	pv  int
	ips map[uint64]struct{}
}

func (a *spanAccumulator) add(ip uint64, hasIP bool) {
	a.pv++
	if !hasIP {
		return
	}
	if a.ips == nil {
		a.ips = make(map[uint64]struct{})
	}
	a.ips[ip] = struct{}{}
}

// dashboardScanner accumulates the dashboard figures of one shard.
type dashboardScanner struct {
	layout *dashboardLayout

	hourly []spanAccumulator
	daily  []spanAccumulator

	windowPV    int
	windowBytes int64
	windowIPs   map[uint64]struct{}
	perMinute   map[int64]int
	groups      [4]map[string]*int
	current     docValues
}

// docValues holds the values of the match being visited. Bleve does not say in
// which order the fields arrive, so they are kept until the match ends.
type docValues struct {
	timestamp int64
	hasTime   bool
	ip        uint64
	hasIP     bool
	bytes     int64
	text      [4][]byte
	hasText   [4]bool
}

func newDashboardScanner(layout *dashboardLayout) *dashboardScanner {
	scanner := &dashboardScanner{
		layout:    layout,
		hourly:    make([]spanAccumulator, layout.hourCount),
		daily:     make([]spanAccumulator, len(layout.dailyKeys)),
		windowIPs: make(map[uint64]struct{}),
		perMinute: make(map[int64]int),
	}
	for i := range scanner.groups {
		scanner.groups[i] = make(map[string]*int)
	}
	return scanner
}

func groupSlot(field string) int {
	switch field {
	case facetBrowser:
		return 0
	case facetOS:
		return 1
	case facetDevice:
		return 2
	case facetURL:
		return 3
	}
	return -1
}

func (s *dashboardScanner) Visit(field string, term []byte) {
	switch field {
	case "timestamp":
		if value, ok := searcher.NumericTermValue(term); ok {
			s.current.timestamp = int64(value)
			s.current.hasTime = true
		}
	case "bytes_sent":
		if value, ok := searcher.NumericTermValue(term); ok {
			s.current.bytes = int64(value)
		}
	case "ip":
		s.current.ip = searcher.HashTerm(term)
		s.current.hasIP = true
	default:
		if slot := groupSlot(field); slot >= 0 {
			s.current.text[slot] = append(s.current.text[slot][:0], term...)
			s.current.hasText[slot] = true
		}
	}
}

func (s *dashboardScanner) EndDoc() {
	cur := s.current
	s.current.hasTime, s.current.hasIP, s.current.bytes = false, false, 0
	s.current.hasText = [4]bool{}
	if !cur.hasTime {
		return
	}
	layout := s.layout
	ts := cur.timestamp

	if ts >= layout.start && ts < layout.end {
		s.windowPV++
		s.windowBytes += cur.bytes
		s.perMinute[ts-ts%60]++
		if cur.hasIP {
			s.windowIPs[cur.ip] = struct{}{}
		}
		for slot, ok := range cur.hasText {
			if !ok {
				continue
			}
			counts := s.groups[slot]
			if count, found := counts[string(cur.text[slot])]; found {
				*count++
				continue
			}
			one := 1
			counts[string(cur.text[slot])] = &one
		}
	}

	if index, ok := layout.dailyIndex[dateKey(time.Unix(ts, 0))]; ok {
		s.daily[index].add(cur.ip, cur.hasIP)
	}

	// An hourly bucket exists for the whole UTC hours that start on the grid of
	// the buckets, which are aligned to the requested start.
	hour := ts - ts%3600
	if offset := hour - layout.hourStart; offset >= 0 && offset%3600 == 0 {
		if index := int(offset / 3600); index < len(s.hourly) {
			s.hourly[index].add(cur.ip, cur.hasIP)
		}
	}
}

// dashboardFigures are the merged results of the shard scanners.
type dashboardFigures struct {
	windowPV    int
	windowBytes int64
	windowUV    int
	peakMinute  int
	hourly      []HourlyAccessStats
	daily       []DailyAccessStats
	groups      [4][]*searcher.FacetTerm
}

// mergeDashboardScanners combines the scanners of all shards.
func mergeDashboardScanners(layout *dashboardLayout, scanners []searcher.DocScanner) *dashboardFigures {
	figures := &dashboardFigures{}

	hourly := make([]spanAccumulator, layout.hourCount)
	daily := make([]spanAccumulator, len(layout.dailyKeys))
	windowIPs := make(map[uint64]struct{})
	perMinute := make(map[int64]int)
	var groups [4]map[string]int
	for i := range groups {
		groups[i] = make(map[string]int)
	}

	for _, scanner := range scanners {
		shard, ok := scanner.(*dashboardScanner)
		if !ok {
			continue
		}
		figures.windowPV += shard.windowPV
		figures.windowBytes += shard.windowBytes
		mergeSpans(hourly, shard.hourly)
		mergeSpans(daily, shard.daily)
		for ip := range shard.windowIPs {
			windowIPs[ip] = struct{}{}
		}
		for minute, pv := range shard.perMinute {
			perMinute[minute] += pv
		}
		for slot := range groups {
			for term, count := range shard.groups[slot] {
				groups[slot][term] += *count
			}
		}
	}

	figures.windowUV = len(windowIPs)
	for _, pv := range perMinute {
		if pv > figures.peakMinute {
			figures.peakMinute = pv
		}
	}

	figures.hourly = make([]HourlyAccessStats, layout.hourCount)
	for i := range hourly {
		stamp := layout.hourStart + int64(i)*3600
		figures.hourly[i] = HourlyAccessStats{
			Hour:      time.Unix(stamp, 0).UTC().Hour(),
			UV:        len(hourly[i].ips),
			PV:        hourly[i].pv,
			Timestamp: stamp,
		}
	}
	figures.daily = make([]DailyAccessStats, len(layout.dailyKeys))
	for i := range daily {
		figures.daily[i] = DailyAccessStats{
			Date:      layout.dailyDate[i],
			UV:        len(daily[i].ips),
			PV:        daily[i].pv,
			Timestamp: layout.dailyStamp[i],
		}
	}
	sort.Slice(figures.daily, func(i, j int) bool { return figures.daily[i].Timestamp < figures.daily[j].Timestamp })

	sizes := [4]int{topGroupSize, topGroupSize, topGroupSize, topURLSize}
	for slot := range groups {
		figures.groups[slot] = topTerms(groups[slot], sizes[slot])
	}
	return figures
}

// mergeSpans adds the buckets of src to dst.
func mergeSpans(dst, src []spanAccumulator) {
	for i := range src {
		dst[i].pv += src[i].pv
		if len(src[i].ips) == 0 {
			continue
		}
		if dst[i].ips == nil {
			dst[i].ips = src[i].ips
			continue
		}
		for ip := range src[i].ips {
			dst[i].ips[ip] = struct{}{}
		}
	}
}

// topTerms returns the size most frequent terms, ordered like a Bleve terms
// facet: by count, highest first, and by term for equal counts.
func topTerms(counts map[string]int, size int) []*searcher.FacetTerm {
	terms := make([]*searcher.FacetTerm, 0, len(counts))
	for term, count := range counts {
		terms = append(terms, &searcher.FacetTerm{Term: term, Count: count})
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].Count != terms[j].Count {
			return terms[i].Count > terms[j].Count
		}
		return terms[i].Term < terms[j].Term
	})
	if len(terms) > size {
		terms = terms[:size]
	}
	return terms
}
