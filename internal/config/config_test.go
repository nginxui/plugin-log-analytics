package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseSettings(t *testing.T) {
	got := ParseSettings(map[string]any{
		KeyIncrementalIndexInterval: float64(30),
		KeyMaxConcurrentIndexTasks:  "3",
		KeyIndexCustomMMDB:          "  custom.mmdb ",
		KeyGeoMapPath:               "/srv/maps",
		"unknown":                   true,
	})

	assert.Equal(t, Settings{
		IncrementalIndexInterval: 30,
		MaxConcurrentIndexTasks:  3,
		IndexCustomMMDB:          "custom.mmdb",
		GeoMapPath:               "/srv/maps",
	}, got)
	assert.Equal(t, 30*time.Minute, got.IncrementalInterval())
}

func TestIncrementalIntervalDefault(t *testing.T) {
	assert.Equal(t, DefaultIncrementalIndexInterval, Settings{}.IncrementalInterval())
	assert.Equal(t, DefaultIncrementalIndexInterval, Settings{IncrementalIndexInterval: -5}.IncrementalInterval())
}

func TestPaths(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	SetDataDir("/data")
	assert.Equal(t, filepath.Join("/data", "index"), IndexPath())
	assert.Equal(t, filepath.Join("/data", "geolite"), GeoLiteDir())
	assert.Equal(t, filepath.Join("/data", "maps"), GeoMapDir())

	SetIndexPath("/legacy/log-index")
	assert.Equal(t, "/legacy/log-index", IndexPath())

	Set(Settings{GeoMapPath: "boundaries"})
	assert.Equal(t, filepath.Join("/data", "boundaries"), GeoMapDir())
	Set(Settings{GeoMapPath: "/abs/maps"})
	assert.Equal(t, "/abs/maps", GeoMapDir())
}

func TestSubscribersSeeChangesOnly(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	var calls int
	var lastNext Settings
	Subscribe(func(_, next Settings) {
		calls++
		lastNext = next
	})

	Set(Settings{})
	assert.Equal(t, 0, calls)

	Set(Settings{IncrementalIndexInterval: 5})
	assert.Equal(t, 1, calls)
	assert.Equal(t, 5, lastNext.IncrementalIndexInterval)

	Set(Settings{IncrementalIndexInterval: 5})
	assert.Equal(t, 1, calls)
}
