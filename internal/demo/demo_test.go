package demo

import (
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/geolite"
	"github.com/nginxui/plugin-log-analytics/internal/indexer"
)

// withDemo sets the demo environment variable for one test and clears the
// provider slots afterwards.
func withDemo(t *testing.T, value string) {
	t.Helper()

	t.Setenv(EnvDemo, value)
	t.Cleanup(func() {
		indexer.SetGeoIPService(nil)
		geolite.SetAvailabilityProvider(nil)
	})
}

func TestEnabled(t *testing.T) {
	for value, want := range map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"FALSE": false,
		"1":     true,
		"true":  true,
		"yes":   true,
	} {
		t.Setenv(EnvDemo, value)
		assert.Equal(t, want, Enabled(), "NGINX_UI_DEMO=%q", value)
	}
}

func TestInstallIsInertWhenDemoDisabled(t *testing.T) {
	withDemo(t, "")

	assert.Nil(t, Install(), "a normal run must install no overrides at all")
	assert.Equal(t, geolite.CurrentAvailability().Exists, geolite.DBExists(), "availability must stay the real one")
}

func TestInstallReportsEveryOverride(t *testing.T) {
	withDemo(t, "1")

	applied := Install()

	require.Len(t, applied, 2)
	assert.Contains(t, applied, "geoip")
	assert.Contains(t, applied, "geolite-availability")
	assert.True(t, geolite.CurrentAvailability().Exists)
}

func TestGeoFabricatesOnlyForDocumentationAddresses(t *testing.T) {
	svc := &geoService{}

	// A documentation address gets a full fabricated location.
	got, err := svc.Search("203.0.113.7")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotEmpty(t, got.RegionCode)

	// A real routable address must never gain a fabricated province or city.
	// This is the property that keeps the fake harmless if it ever loads on a
	// real installation.
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "223.5.5.5"} {
		got, err := svc.Search(ip)
		require.NoError(t, err, ip)
		if got == nil {
			continue
		}
		assert.Empty(t, got.Province, "province fabricated for real IP %s", ip)
		assert.Empty(t, got.City, "city fabricated for real IP %s", ip)
	}
}

func TestGeoMirrorsChineseRegionCollapsing(t *testing.T) {
	svc := &geoService{}

	// The real service collapses CN/HK/MO/TW into "CN", so any fabricated
	// domestic location must do the same and name its region for the map.
	for _, ip := range documentationSample(t, 200) {
		got, err := svc.Search(ip)
		require.NoError(t, err)
		require.NotNil(t, got)

		if got.Province != "" {
			assert.Equal(t, "CN", got.RegionCode,
				"a fabricated province must always carry region code CN")
			assert.Contains(t, cnProvinces, got.Province)
			assert.Equal(t, cnProvinceCodes[got.Province], got.Sub1)
			assert.NotEmpty(t, got.Sub1)
		}
	}
}

func TestGeoIsDeterministic(t *testing.T) {
	svc := &geoService{}

	for _, ip := range documentationSample(t, 50) {
		first, err := svc.Search(ip)
		require.NoError(t, err)

		for range 20 {
			again, err := svc.Search(ip)
			require.NoError(t, err)
			assert.Equal(t, first, again, "geo lookup for %s is not stable", ip)
		}
	}
}

func TestGeoIsDeterministicUnderConcurrency(t *testing.T) {
	svc := &geoService{}
	ips := documentationSample(t, 32)

	expected := make([]string, len(ips))
	for i, ip := range ips {
		got, err := svc.Search(ip)
		require.NoError(t, err)
		expected[i] = got.Province + "|" + got.City + "|" + got.RegionCode
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, ip := range ips {
				got, err := svc.Search(ip)
				if err != nil || got == nil {
					t.Errorf("lookup failed for %s: %v", ip, err)
					return
				}
				if actual := got.Province + "|" + got.City + "|" + got.RegionCode; actual != expected[i] {
					t.Errorf("%s: got %q, want %q", ip, actual, expected[i])
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestGeoDeclinesUnparseableInput(t *testing.T) {
	svc := &geoService{}

	for _, input := range []string{"", "not-an-ip", "999.999.999.999", "192.0.2"} {
		got, err := svc.Search(input)
		assert.NoError(t, err, input)
		assert.Nil(t, got, input)
	}
}

func TestGeoLiteAvailabilityReportsInstalled(t *testing.T) {
	availability := geoLiteAvailability{}.Availability()

	assert.True(t, availability.Exists)
	assert.NotEmpty(t, availability.Path)
	assert.Positive(t, availability.Size)
	assert.False(t, availability.LastModified.IsZero())
}

func TestSeedIsStableAcrossCalls(t *testing.T) {
	first := seed("geo", "203.0.113.1")
	for range 100 {
		assert.Equal(t, first, seed("geo", "203.0.113.1"))
	}
	assert.NotEqual(t, first, seed("geo", "203.0.113.2"))
	assert.NotEqual(t, first, seed("upstream", "203.0.113.1"))
}

func TestPickHandlesEmptyTable(t *testing.T) {
	assert.Empty(t, pick([]string{}, 42))
	assert.Equal(t, "only", pick([]string{"only"}, 42))
}

func TestOverseasCountriesIsStablyOrdered(t *testing.T) {
	first := overseasCountries()
	for range 50 {
		assert.Equal(t, first, overseasCountries())
	}
}

// documentationSample returns n consecutive addresses from TEST-NET-3.
func documentationSample(t *testing.T, n int) []string {
	t.Helper()

	base := netip.MustParseAddr("203.0.113.0")
	out := make([]string, 0, n)
	for i := range n {
		addr := base
		for range i {
			addr = addr.Next()
		}
		require.True(t, isDocumentationIP(addr))
		out = append(out, addr.String())
	}
	return out
}
