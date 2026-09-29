package geolite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"

	"github.com/nginxui/plugin-log-analytics/internal/apierr"
	"github.com/nginxui/plugin-log-analytics/internal/config"
)

func useDataDir(t *testing.T) string {
	t.Helper()

	config.Reset()
	dir := t.TempDir()
	config.SetDataDir(dir)
	t.Cleanup(func() {
		CloseService()
		config.Reset()
	})
	return dir
}

func TestCountryCodeComesFromTheEmbeddedDatabase(t *testing.T) {
	useDataDir(t)

	assert.Equal(t, "US", CountryCode("8.8.8.8"))
	assert.Equal(t, "CN", CountryCode("223.5.5.5"))
	assert.Equal(t, "Unknown", CountryCode("not an address"))
	assert.Empty(t, CountryCode("192.0.2.1"), "a documentation address has no country")
}

func TestCountryDatabaseIsReleasedAndOpensAgain(t *testing.T) {
	useDataDir(t)

	require.Equal(t, "US", CountryCode("8.8.8.8"))
	CloseService()
	CloseService()
	assert.Equal(t, "US", CountryCode("8.8.8.8"), "the next lookup opens the database again")
}

func TestServiceWithoutADatabaseFailsAndRetries(t *testing.T) {
	dir := useDataDir(t)

	_, err := GetService()
	require.Error(t, err)
	assert.False(t, DBExists())

	// A failure is not remembered: once a database is there the next call works.
	// The embedded country database is a valid MaxMind file, it stands in here.
	raw := decompressEmbeddedCountryDB(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "geolite"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "geolite", "GeoLite2-City.mmdb"), raw, 0o644))

	service, err := GetService()
	require.NoError(t, err)
	assert.NotNil(t, service)
	again, err := GetService()
	require.NoError(t, err)
	assert.Same(t, service, again)

	location, err := service.Search("8.8.8.8")
	if err == nil {
		assert.Equal(t, "US", location.RegionCode)
	}

	CloseService()
	_, err = service.Search("8.8.8.8")
	assert.Error(t, err, "a closed service answers with an error, not with a crash")
}

func TestPathsFollowTheDataDirectoryAndTheCustomSetting(t *testing.T) {
	dir := useDataDir(t)
	defaultPath := filepath.Join(dir, "geolite", "GeoLite2-City.mmdb")

	assert.Equal(t, defaultPath, GetDBPath())
	assert.Equal(t, filepath.Join(dir, "geolite", "GeoLite2-City.mmdb.xz"), GetDBXZPath())

	config.Set(config.Settings{IndexCustomMMDB: "custom.mmdb"})
	assert.Equal(t, filepath.Join(dir, "geolite", "custom.mmdb"), GetDBPath(), "a relative custom path sits in the geolite directory")

	config.Set(config.Settings{IndexCustomMMDB: "/srv/geo/custom.mmdb"})
	assert.Equal(t, "/srv/geo/custom.mmdb", GetDBPath())

	// The downloaded database wins when it exists.
	require.NoError(t, os.MkdirAll(filepath.Dir(defaultPath), 0o755))
	require.NoError(t, os.WriteFile(defaultPath, []byte("x"), 0o644))
	assert.Equal(t, defaultPath, GetDBPath())
}

func TestDecompressWritesTheDefaultDatabaseNeverTheCustomOne(t *testing.T) {
	dir := useDataDir(t)
	geoDir := filepath.Join(dir, "geolite")
	require.NoError(t, os.MkdirAll(geoDir, 0o755))

	custom := filepath.Join(t.TempDir(), "custom.mmdb")
	require.NoError(t, os.WriteFile(custom, []byte("mine"), 0o644))
	config.Set(config.Settings{IndexCustomMMDB: custom})

	payload := bytes.Repeat([]byte("maxmind"), 5000)
	var packed bytes.Buffer
	writer, err := xz.NewWriter(&packed)
	require.NoError(t, err)
	_, err = writer.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, os.WriteFile(GetDBXZPath(), packed.Bytes(), 0o644))

	progress := make(chan float64, 100)
	require.NoError(t, DecompressGeoLiteDB(progress))

	got, err := os.ReadFile(filepath.Join(geoDir, "GeoLite2-City.mmdb"))
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.NoFileExists(t, filepath.Join(geoDir, "GeoLite2-City.mmdb.tmp"))
	assert.NoFileExists(t, GetDBXZPath(), "the archive is removed once unpacked")

	mine, err := os.ReadFile(custom)
	require.NoError(t, err)
	assert.Equal(t, "mine", string(mine), "the custom database is never overwritten")

	assert.Equal(t, float64(100), lastProgress(progress))
}

func TestDecompressOfABrokenArchiveLeavesNothingBehind(t *testing.T) {
	dir := useDataDir(t)
	geoDir := filepath.Join(dir, "geolite")
	require.NoError(t, os.MkdirAll(geoDir, 0o755))
	require.NoError(t, os.WriteFile(GetDBXZPath(), []byte("not an xz archive"), 0o644))

	err := DecompressGeoLiteDB(make(chan float64, 10))
	require.Error(t, err)

	var coded *apierr.Error
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, int32(60007), coded.Code)
	assert.NoFileExists(t, filepath.Join(geoDir, "GeoLite2-City.mmdb"))
}

func TestAvailabilityProviderOverridesTheFilesystem(t *testing.T) {
	useDataDir(t)
	t.Cleanup(func() { SetAvailabilityProvider(nil) })

	assert.False(t, CurrentAvailability().Exists)

	SetAvailabilityProvider(fixedAvailability{Availability{Exists: true, Path: "demo", Size: 5}})
	got := CurrentAvailability()
	assert.True(t, got.Exists)
	assert.Equal(t, "demo", got.Path)
}

type fixedAvailability struct{ value Availability }

func (f fixedAvailability) Availability() Availability { return f.value }

func decompressEmbeddedCountryDB(t *testing.T) []byte {
	t.Helper()

	reader, err := xz.NewReader(bytes.NewReader(countryDBXZ))
	require.NoError(t, err)
	var out bytes.Buffer
	_, err = out.ReadFrom(reader)
	require.NoError(t, err)
	return out.Bytes()
}

func lastProgress(progress chan float64) float64 {
	var last float64
	for {
		select {
		case value := <-progress:
			last = value
		default:
			return last
		}
	}
}
