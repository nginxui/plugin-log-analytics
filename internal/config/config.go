// Package config holds what the rest of the plugin reads from its
// environment: the data directory layout and the plugin settings the host
// pushes on start and whenever a person saves them.
package config

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Setting keys, as declared in plugin.json.
const (
	KeyIncrementalIndexInterval = "incremental_index_interval"
	KeyMaxConcurrentIndexTasks  = "max_concurrent_index_tasks"
	KeyIndexCustomMMDB          = "index_custom_mmdb"
	KeyGeoMapPath               = "geo_map_path"
)

// DefaultIncrementalIndexInterval is used when the setting is missing or not
// positive.
const DefaultIncrementalIndexInterval = 15 * time.Minute

// DefaultGeoMapDir is the directory of map boundary files below the data
// directory when geo_map_path is empty.
const DefaultGeoMapDir = "maps"

// Settings are the plugin settings.
type Settings struct {
	// IncrementalIndexInterval is how often the incremental indexing round
	// runs, in minutes. Zero or negative selects the default.
	IncrementalIndexInterval int
	// MaxConcurrentIndexTasks caps how many log groups are indexed at the same
	// time. Zero or negative derives the value from the CPU budget.
	MaxConcurrentIndexTasks int
	// IndexCustomMMDB names a custom GeoIP database. A relative path is
	// resolved against the geolite directory.
	IndexCustomMMDB string
	// GeoMapPath points to the directory of map boundary files. A relative
	// path is resolved against the data directory.
	GeoMapPath string
}

// IncrementalInterval returns the effective incremental indexing interval.
func (s Settings) IncrementalInterval() time.Duration {
	if s.IncrementalIndexInterval <= 0 {
		return DefaultIncrementalIndexInterval
	}
	return time.Duration(s.IncrementalIndexInterval) * time.Minute
}

// ParseSettings reads the settings map the host delivers. Numbers may arrive
// as JSON numbers or as strings, unknown keys are ignored.
func ParseSettings(raw map[string]any) Settings {
	return Settings{
		IncrementalIndexInterval: intValue(raw[KeyIncrementalIndexInterval]),
		MaxConcurrentIndexTasks:  intValue(raw[KeyMaxConcurrentIndexTasks]),
		IndexCustomMMDB:          strings.TrimSpace(stringValue(raw[KeyIndexCustomMMDB])),
		GeoMapPath:               strings.TrimSpace(stringValue(raw[KeyGeoMapPath])),
	}
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

var (
	mu          sync.RWMutex
	dataDir     string
	indexPath   string
	settings    Settings
	subscribers []func(prev, next Settings)
)

// SetDataDir sets the plugin data directory, the only place the plugin writes.
func SetDataDir(dir string) {
	mu.Lock()
	defer mu.Unlock()
	dataDir = dir
}

// DataDir returns the plugin data directory.
func DataDir() string {
	mu.RLock()
	defer mu.RUnlock()
	return dataDir
}

// SetIndexPath overrides the index directory. It is used when an index that
// could not be moved into the data directory is used in place.
func SetIndexPath(path string) {
	mu.Lock()
	defer mu.Unlock()
	indexPath = path
}

// IndexPath returns the directory of the Bleve shards.
func IndexPath() string {
	mu.RLock()
	defer mu.RUnlock()
	if indexPath != "" {
		return indexPath
	}
	return filepath.Join(dataDir, "index")
}

// GeoLiteDir returns the directory of the GeoIP databases.
func GeoLiteDir() string {
	mu.RLock()
	defer mu.RUnlock()
	return filepath.Join(dataDir, "geolite")
}

// GeoMapDir returns the directory of the map boundary files.
func GeoMapDir() string {
	mu.RLock()
	defer mu.RUnlock()

	base := settings.GeoMapPath
	if base == "" {
		base = DefaultGeoMapDir
	}
	if filepath.IsAbs(base) {
		return filepath.Clean(base)
	}
	return filepath.Clean(filepath.Join(dataDir, base))
}

// Get returns the current settings.
func Get() Settings {
	mu.RLock()
	defer mu.RUnlock()
	return settings
}

// Set replaces the settings and tells the subscribers when they changed.
func Set(next Settings) {
	mu.Lock()
	prev := settings
	settings = next
	subs := append([]func(prev, next Settings){}, subscribers...)
	mu.Unlock()

	if prev == next {
		return
	}
	for _, fn := range subs {
		fn(prev, next)
	}
}

// Subscribe registers a callback for setting changes.
func Subscribe(fn func(prev, next Settings)) {
	mu.Lock()
	defer mu.Unlock()
	subscribers = append(subscribers, fn)
}

// Reset clears every value and subscriber, for tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	dataDir, indexPath, settings, subscribers = "", "", Settings{}, nil
}
