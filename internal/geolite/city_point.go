package geolite

import (
	"fmt"
	"strconv"
	"strings"
)

// CityPoint is the hotspot key of a city: country|name|latitude|longitude|id,
// the coordinates rounded to two decimals and the GeoNames id left out when the
// database has none. A | in the name would split the key, so it is replaced.
// The Rust plugin writes the same keys.
func CityPoint(country, city string, id uint, lat, lon float64) string {
	key := fmt.Sprintf("%s|%s|%.2f|%.2f", country, strings.ReplaceAll(city, "|", "/"), lat, lon)
	if id == 0 {
		return key
	}
	return key + "|" + strconv.FormatUint(uint64(id), 10)
}

// ParseCityPoint splits a hotspot key. id is 0 for a key without one. The last
// result is false for text in another form.
func ParseCityPoint(key string) (country, city string, id uint, lat, lon float64, ok bool) {
	parts := strings.Split(key, "|")
	if len(parts) != 4 && len(parts) != 5 {
		return "", "", 0, 0, 0, false
	}
	lat, errLat := strconv.ParseFloat(parts[2], 64)
	lon, errLon := strconv.ParseFloat(parts[3], 64)
	if errLat != nil || errLon != nil {
		return "", "", 0, 0, 0, false
	}
	if len(parts) == 5 {
		parsed, err := strconv.ParseUint(parts[4], 10, 0)
		if err != nil {
			return "", "", 0, 0, 0, false
		}
		id = uint(parsed)
	}
	return parts[0], parts[1], id, lat, lon, true
}

// subdivisionCode returns the ISO 3166-2 code of one subdivision level, like
// US-CA, or an empty string.
func subdivisionCode(country string, subdivisions []mmdbProvince, level int) string {
	if country == "" || level >= len(subdivisions) {
		return ""
	}
	code := strings.TrimSpace(subdivisions[level].ISOCode)
	if code == "" {
		return ""
	}
	return country + "-" + code
}
