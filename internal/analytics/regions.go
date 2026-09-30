package analytics

import (
	"context"
	"fmt"
	"sort"

	"github.com/nginxui/plugin-log-analytics/internal/geolite"
	"github.com/nginxui/plugin-log-analytics/internal/searcher"
)

// regionFacetSize covers the subdivisions of the largest country at both levels.
const regionFacetSize = 500

// GetRegionShares counts the requests per subdivision of a country. Both
// levels are counted, since the outlines of a country use one of them, and
// the shares are of all requests of the country.
func (s *service) GetRegionShares(ctx context.Context, req *GeoQueryRequest, countryCode string) ([]RegionShare, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	if err := s.ValidateTimeRange(req.StartTime, req.EndTime); err != nil {
		return nil, fmt.Errorf("invalid time range: %w", err)
	}

	result, err := s.searcher.Search(ctx, &searcher.SearchRequest{
		StartTime:      &req.StartTime,
		EndTime:        &req.EndTime,
		LogPaths:       req.LogPaths,
		UseMainLogPath: req.UseMainLogPath,
		Countries:      []string{countryCode},
		Limit:          -1,
		IncludeFacets:  true,
		FacetFields:    []string{"sub1", "sub2"},
		FacetSize:      regionFacetSize,
		UseCache:       true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to count the regions: %w", err)
	}

	// A code counted at both levels keeps the level it was first seen at
	byCode := map[string]*RegionShare{}
	for level, field := range []string{"sub1", "sub2"} {
		if facet := result.Facets[field]; facet != nil {
			for _, term := range facet.Terms {
				share := byCode[term.Term]
				if share == nil {
					share = &RegionShare{Code: term.Term, Level: level + 1}
					byCode[term.Term] = share
				}
				share.Value += term.Count
			}
		}
	}
	shares := make([]RegionShare, 0, len(byCode))
	for _, share := range byCode {
		share.Percent = percentOf(share.Value, int(result.TotalHits))
		shares = append(shares, *share)
	}
	sort.Slice(shares, func(i, j int) bool {
		if shares[i].Value != shares[j].Value {
			return shares[i].Value > shares[j].Value
		}
		return shares[i].Code < shares[j].Code
	})
	return shares, nil
}

// GetCityPoints returns the busiest cities with their coordinates, of one
// country or of all when countryCode is empty. req.Limit caps the list.
func (s *service) GetCityPoints(ctx context.Context, req *GeoQueryRequest, countryCode string) ([]CityPoint, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	if err := s.ValidateTimeRange(req.StartTime, req.EndTime); err != nil {
		return nil, fmt.Errorf("invalid time range: %w", err)
	}

	searchReq := &searcher.SearchRequest{
		StartTime:      &req.StartTime,
		EndTime:        &req.EndTime,
		LogPaths:       req.LogPaths,
		UseMainLogPath: req.UseMainLogPath,
		Limit:          -1,
		IncludeFacets:  true,
		FacetFields:    []string{"city_point"},
		FacetSize:      req.Limit,
		UseCache:       true,
	}
	if countryCode != "" {
		searchReq.Countries = []string{countryCode}
	}
	result, err := s.searcher.Search(ctx, searchReq)
	if err != nil {
		return nil, fmt.Errorf("failed to count the cities: %w", err)
	}

	facet := result.Facets["city_point"]
	if facet == nil {
		return []CityPoint{}, nil
	}
	// The facet total of the searcher counts distinct terms, so the requests
	// placed in a city are the listed counts and those past the list.
	placed := facet.Other
	for _, term := range facet.Terms {
		placed += term.Count
	}
	points := make([]CityPoint, 0, len(facet.Terms))
	for _, term := range facet.Terms {
		country, city, lat, lon, ok := geolite.ParseCityPoint(term.Term)
		if !ok {
			continue
		}
		points = append(points, CityPoint{
			Country: country,
			City:    city,
			Lat:     lat,
			Lon:     lon,
			Value:   term.Count,
			Percent: percentOf(term.Count, placed),
		})
	}
	return points, nil
}

func percentOf(count, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(count) / float64(total) * 100
}
