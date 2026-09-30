package geolite

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCityPointsRoundTrip(t *testing.T) {
	key := CityPoint("US", "Salt Lake|City", 40.7608, -111.8910)
	assert.Equal(t, "US|Salt Lake/City|40.76|-111.89", key)

	country, city, lat, lon, ok := ParseCityPoint(key)
	assert.True(t, ok)
	assert.Equal(t, "US", country)
	assert.Equal(t, "Salt Lake/City", city)
	assert.InDelta(t, 40.76, lat, 1e-9)
	assert.InDelta(t, -111.89, lon, 1e-9)

	_, _, _, _, ok = ParseCityPoint("broken")
	assert.False(t, ok)
}

func TestSubdivisionCodesCarryTheCountry(t *testing.T) {
	subdivisions := []mmdbProvince{{ISOCode: "IDF"}, {ISOCode: "75"}}
	assert.Equal(t, "FR-IDF", subdivisionCode("FR", subdivisions, 0))
	assert.Equal(t, "FR-75", subdivisionCode("FR", subdivisions, 1))
	assert.Empty(t, subdivisionCode("FR", subdivisions, 2))
	assert.Empty(t, subdivisionCode("", subdivisions, 0))
}
