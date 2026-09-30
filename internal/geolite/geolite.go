package geolite

import (
	"fmt"
	"math"
	"net/netip"
	"strings"
	"sync"

	"github.com/oschwald/maxminddb-golang/v2"
)

type IPLocation struct {
	RegionCode string `json:"region_code"`
	Province   string `json:"province"`
	City       string `json:"city"`
	C1         string `json:"c1,omitempty"`
	C2         string `json:"c2,omitempty"`
	C3         string `json:"c3,omitempty"`
	C4         string `json:"c4,omitempty"`
	// Sub1 and Sub2 are the ISO 3166-2 codes of the first two subdivision
	// levels, like US-CA or FR-IDF and FR-75. Empty when the database has none.
	Sub1 string `json:"sub1,omitempty"`
	Sub2 string `json:"sub2,omitempty"`
	// CityPoint is the city with its coordinates for the hotspot map, see
	// CityPoint. Empty without coordinates.
	CityPoint string `json:"city_point,omitempty"`
}

type Service struct {
	// mu keeps a lookup from racing the unmapping of the database in Close.
	mu     sync.RWMutex
	cityDB *maxminddb.Reader
}

type mmdbNames struct {
	English           string `maxminddb:"en"`
	SimplifiedChinese string `maxminddb:"zh-CN"`
}

type mmdbCountry struct {
	ISOCode string    `maxminddb:"iso_code"`
	Names   mmdbNames `maxminddb:"names"`
	Name    string    `maxminddb:"name"`
	NameZH  string    `maxminddb:"name_zh"`
}

type mmdbProvince struct {
	Names   mmdbNames `maxminddb:"names"`
	Name    string    `maxminddb:"name"`
	NameZH  string    `maxminddb:"name_zh"`
	ISOCode string    `maxminddb:"iso_code"`
}

type mmdbLocation struct {
	Latitude  *float64 `maxminddb:"latitude"`
	Longitude *float64 `maxminddb:"longitude"`
}

type mmdbCity struct {
	Names  mmdbNames `maxminddb:"names"`
	Name   string    `maxminddb:"name"`
	NameZH string    `maxminddb:"name_zh"`
}

type mmdbRecord struct {
	Country      mmdbCountry    `maxminddb:"country"`
	Subdivisions []mmdbProvince `maxminddb:"subdivisions"`
	Province     mmdbProvince   `maxminddb:"province"`
	City         mmdbCity       `maxminddb:"city"`
	Location     mmdbLocation   `maxminddb:"location"`
	C1           string         `maxminddb:"c1"`
	C2           string         `maxminddb:"c2"`
	C3           string         `maxminddb:"c3"`
	C4           string         `maxminddb:"c4"`
}

var (
	instance *Service
	mu       sync.Mutex
)

// GetService returns the shared city database service, opening it on first
// use. A failure is not remembered: the next call tries again, so a database
// downloaded in the meantime is picked up without a restart.
func GetService() (*Service, error) {
	mu.Lock()
	defer mu.Unlock()

	if instance != nil {
		return instance, nil
	}

	service := &Service{}
	if err := service.init(); err != nil {
		return nil, err
	}
	instance = service
	return instance, nil
}

// CloseService closes the shared city database service, so the mapped file and
// the country database are released. The next GetService opens them again.
func CloseService() {
	mu.Lock()
	service := instance
	instance = nil
	mu.Unlock()

	if service != nil {
		service.Close()
	}
	closeCountryDB()
}

func (s *Service) init() error {
	// Load database from file (memory-mapped for efficiency)
	dbPath := GetDBPath()

	if !DBExists() {
		return fmt.Errorf("GeoLite2 database not found at %s. Please download it first", dbPath)
	}

	if err := s.loadFromFile(dbPath); err != nil {
		return fmt.Errorf("failed to load GeoLite2 database: %v", err)
	}

	return nil
}

func (s *Service) loadFromFile(path string) error {
	// Open database file with memory mapping (more efficient than loading into memory)
	cityDB, err := maxminddb.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open GeoLite2 database: %v", err)
	}

	s.cityDB = cityDB
	return nil
}

func (s *Service) Search(ipStr string) (*IPLocation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.cityDB == nil {
		return nil, fmt.Errorf("no databases loaded")
	}

	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return nil, fmt.Errorf("invalid IP address: %s", ipStr)
	}

	loc := &IPLocation{}

	// The embedded country database gives the country code
	loc.RegionCode = CountryCode(ipStr)

	// Decode both standard GeoLite fields and custom enterprise fields.
	var record mmdbRecord
	lookupResult := s.cityDB.Lookup(ip)
	if err := lookupResult.Decode(&record); err == nil {
		if loc.RegionCode == "" {
			loc.RegionCode = strings.TrimSpace(record.Country.ISOCode)
		}

		loc.C1 = strings.TrimSpace(record.C1)
		loc.C2 = strings.TrimSpace(record.C2)
		loc.C3 = strings.TrimSpace(record.C3)
		loc.C4 = strings.TrimSpace(record.C4)

		provinceEN := firstNonEmpty(
			record.Province.Name,
			record.Province.Names.English,
			firstSubdivisionValue(record.Subdivisions, false),
		)
		provinceZH := firstNonEmpty(
			record.Province.NameZH,
			record.Province.Names.SimplifiedChinese,
			firstSubdivisionValue(record.Subdivisions, true),
		)

		cityEN := firstNonEmpty(record.City.Name, record.City.Names.English)
		cityZH := firstNonEmpty(record.City.NameZH, record.City.Names.SimplifiedChinese)

		country := strings.TrimSpace(record.Country.ISOCode)
		if country == "" {
			country = loc.RegionCode
		}
		loc.Sub1 = subdivisionCode(country, record.Subdivisions, 0)
		loc.Sub2 = subdivisionCode(country, record.Subdivisions, 1)

		loc.Province = provinceEN
		loc.City = cityEN

		if IsChineseRegion(loc.RegionCode) || IsChineseRegion(record.Country.ISOCode) {
			if provinceZH != "" {
				loc.Province = provinceZH
			} else if loc.Province == "" {
				loc.Province = "其它"
			}

			if cityZH != "" {
				loc.City = cityZH
			}

			loc.RegionCode = "CN"
		}

		lat, lon := record.Location.Latitude, record.Location.Longitude
		if loc.City != "" && lat != nil && lon != nil && !math.IsNaN(*lat) && !math.IsNaN(*lon) {
			loc.CityPoint = CityPoint(loc.RegionCode, loc.City, *lat, *lon)
		}

		return loc, nil
	}

	// If city database lookup fails, return minimal info with country code
	if loc.RegionCode != "" {
		return loc, nil
	}

	return nil, fmt.Errorf("no location data found for IP: %s", ipStr)
}

func (s *Service) SearchWithISO(ipStr string) (*IPLocation, error) {
	return s.Search(ipStr)
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cityDB != nil {
		s.cityDB.Close()
		s.cityDB = nil
	}
}

func IsChineseIP(loc *IPLocation) bool {
	return loc != nil && (loc.RegionCode == "CN" ||
		loc.RegionCode == "HK" ||
		loc.RegionCode == "MO" ||
		loc.RegionCode == "TW")
}

func IsChineseRegion(regionCode string) bool {
	chineseRegionCodes := []string{"CN", "HK", "MO", "TW"}
	for _, region := range chineseRegionCodes {
		if regionCode == region {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}

	return ""
}

func firstSubdivisionValue(subdivisions []mmdbProvince, chinese bool) string {
	if len(subdivisions) == 0 {
		return ""
	}

	if chinese {
		return firstNonEmpty(subdivisions[0].NameZH, subdivisions[0].Names.SimplifiedChinese)
	}

	return firstNonEmpty(subdivisions[0].Name, subdivisions[0].Names.English)
}
