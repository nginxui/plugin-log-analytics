package geolite

import (
	"bytes"
	_ "embed"
	"io"
	"net/netip"
	"sync"

	"github.com/oschwald/maxminddb-golang/v2"
	"github.com/ulikunitz/xz"

	"github.com/nginxui/plugin-log-analytics/internal/logger"
)

// countryDBXZ is the GeoLite2 country database, xz compressed. It answers the
// country code of an address when no city database is installed.
//
//go:embed GeoLite2-Country.mmdb.xz
var countryDBXZ []byte

// unknownCountry is what CountryCode answers for an address it cannot place.
const unknownCountry = "Unknown"

var (
	countryMu     sync.RWMutex
	countryReader *maxminddb.Reader
)

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// openCountryDB unpacks the embedded database on first use. The unpacked
// database is held in memory until closeCountryDB, which the log parser calls
// when an indexing round ends, so an idle plugin does not keep it.
func openCountryDB() (*maxminddb.Reader, error) {
	countryMu.Lock()
	defer countryMu.Unlock()

	if countryReader != nil {
		return countryReader, nil
	}

	reader, err := xz.NewReader(bytes.NewReader(countryDBXZ))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	db, err := maxminddb.OpenBytes(raw)
	if err != nil {
		return nil, err
	}
	countryReader = db
	return countryReader, nil
}

func closeCountryDB() {
	countryMu.Lock()
	defer countryMu.Unlock()

	if countryReader != nil {
		_ = countryReader.Close()
		countryReader = nil
	}
}

// CountryCode returns the ISO country code of an address from the embedded
// country database. It answers "Unknown" when the address cannot be parsed or
// looked up, and an empty string for an address the database does not place.
func CountryCode(input string) string {
	ip, err := netip.ParseAddr(input)
	if err != nil {
		logger.Error(err)
		return unknownCountry
	}

	// The lookup holds the read lock so closeCountryDB cannot release the
	// database under it. A close between open and lock means a second try.
	for range 2 {
		db, err := openCountryDB()
		if err != nil {
			logger.Error(err)
			return unknownCountry
		}

		countryMu.RLock()
		if countryReader != db {
			countryMu.RUnlock()
			continue
		}
		var record countryRecord
		err = db.Lookup(ip).Decode(&record)
		countryMu.RUnlock()

		if err != nil {
			logger.Error(err)
			return unknownCountry
		}
		return record.Country.ISOCode
	}
	return unknownCountry
}
