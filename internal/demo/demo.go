// Package demo fabricates the geo enrichment a public demo instance cannot
// obtain honestly: it ships no city database, so provinces and cities of the
// documentation addresses in its synthetic access log are made up here.
//
// The mechanism is deliberate. The indexer and the GeoLite provider expose a
// slot that defaults to nil, and this package is the only one that fills it,
// from a single call in main. A normal run never calls Install, so the slots
// stay nil and this code is unreachable rather than merely un-taken.
//
// Fabrication happens at the INPUT to the real pipeline, never at the output of
// a handler. A fabricated GeoIPService feeds the real parser, the real indexer
// and the real search layer, so facets, filters and time ranges all stay honest
// code under test.
package demo

import (
	"os"
	"strings"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/geolite"
	"github.com/nginxui/plugin-log-analytics/internal/indexer"
)

// EnvDemo is the environment variable that turns the demo mode on.
const EnvDemo = "NGINX_UI_DEMO"

// Enabled reports whether this node runs as a public demo. Any non empty value
// other than 0 and false turns it on.
func Enabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(EnvDemo)))
	return value != "" && value != "0" && value != "false"
}

// demoBuildTime anchors every fabricated timestamp so the demo does not appear
// to have been built moments ago on each cold start.
var demoBuildTime = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// geoLiteAvailability reports the city database as installed.
//
// The demo image deliberately does not ship GeoLite2-City (61 MB). Country
// codes come from the embedded database, and provinces and cities are
// fabricated by geoService, so the feature works, there is simply nothing for
// the operator to download. Reporting it as present hides a download button
// that would only waste bandwidth.
type geoLiteAvailability struct{}

var _ geolite.AvailabilityProvider = (*geoLiteAvailability)(nil)

func (geoLiteAvailability) Availability() geolite.Availability {
	return geolite.Availability{
		Exists:       true,
		Path:         geolite.GetDBPath(),
		Size:         64_212_480,
		LastModified: demoBuildTime,
	}
}

// Install fills the provider slots this package owns and returns the names of
// the overrides it applied. It is a no-op returning nil when the demo mode is
// off. It must run before the first indexing round: geo enrichment is baked
// into the indexed document.
func Install() []string {
	if !Enabled() {
		return nil
	}

	indexer.SetGeoIPService(&geoService{})
	geolite.SetAvailabilityProvider(geoLiteAvailability{})
	return []string{"geoip", "geolite-availability"}
}
