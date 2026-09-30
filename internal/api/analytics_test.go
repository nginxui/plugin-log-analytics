package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestSplitCommaSeparated verifies that comma-joined filter values produced by
// the frontend multi-select inputs (browser/os/device) are split back into
// individual values so each can be matched independently.
func TestSplitCommaSeparated(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single value", "Chrome", []string{"Chrome"}},
		{"multiple values", "Chrome,Firefox", []string{"Chrome", "Firefox"}},
		{"values containing spaces", "Internet Explorer,Samsung Browser", []string{"Internet Explorer", "Samsung Browser"}},
		{"trims surrounding whitespace", " Chrome , Firefox ", []string{"Chrome", "Firefox"}},
		{"drops empty segments", "Chrome,,Firefox,", []string{"Chrome", "Firefox"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, splitCommaSeparated(tt.input))
		})
	}
}

func TestEndAfterIncludesTheLastSecond(t *testing.T) {
	assert.Equal(t, int64(10), endAfter(9))
	assert.Equal(t, int64(0), endAfter(0), "zero leaves the range open")
}

func TestEnrichErrorEntryReadsTheLine(t *testing.T) {
	entry := enrichErrorEntry(map[string]interface{}{
		"level": "error",
		"raw":   `2026/09/07 15:32:29 [error] 12#12: *7 connect() failed, client: 203.0.113.9, server: example.com, request: "GET /api HTTP/1.1", upstream: "http://127.0.0.1:9000/api", host: "example.com"`,
	})
	assert.Equal(t, "connect() failed", entry["message"])
	assert.Equal(t, "example.com", entry["server"])
	assert.Equal(t, "GET /api HTTP/1.1", entry["request"])
	assert.Equal(t, "http://127.0.0.1:9000/api", entry["upstream"])
	assert.Equal(t, int64(12), entry["pid"])
	assert.Equal(t, int64(7), entry["connection"])

	access := map[string]interface{}{"raw": "1.2.3.4 - - ...", "status": 200}
	assert.Equal(t, map[string]interface{}{"raw": "1.2.3.4 - - ...", "status": 200}, enrichErrorEntry(access))
}

func TestLocalDayStartsAtTheLocalMidnight(t *testing.T) {
	day, err := localDay("2026-10-01")
	assert.NoError(t, err)
	assert.Equal(t, time.Local, day.Location())
	assert.Equal(t, "2026-10-01 00:00:00", day.Format("2006-01-02 15:04:05"))
	assert.Equal(t, "2026-10-02 00:00:00", day.AddDate(0, 0, 1).Format("2006-01-02 15:04:05"))

	_, err = localDay("2026/10/01")
	assert.Error(t, err)
}
