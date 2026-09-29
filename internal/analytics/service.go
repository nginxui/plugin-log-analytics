package analytics

import (
	"context"
	"fmt"

	"github.com/nginxui/plugin-log-analytics/internal/searcher"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// Service defines the interface for analytics operations
type Service interface {
	GetDashboardAnalytics(ctx context.Context, req *DashboardQueryRequest) (*DashboardAnalytics, error)

	GetLogEntriesStats(ctx context.Context, req *searcher.SearchRequest) (*EntriesStats, error)

	GetGeoDistribution(ctx context.Context, req *GeoQueryRequest) (*GeoDistribution, error)
	GetGeoDistributionByCountry(ctx context.Context, req *GeoQueryRequest, countryCode string) (*GeoDistribution, error)
	GetGeoDistributionByProvince(ctx context.Context, req *GeoQueryRequest, countryCode, province string) (*GeoDistribution, error)
	GetTopCountries(ctx context.Context, req *GeoQueryRequest) ([]CountryStats, error)

	ValidateLogPath(logPath string) error
	ValidateTimeRange(startTime, endTime int64) error

	Stop() error
}

// service implements the Service interface
type service struct {
	searcher searcher.SearcherInterface
}

// NewService creates a new analytics service
func NewService(s searcher.SearcherInterface) Service {
	return &service{
		searcher: s,
	}
}

// Stop gracefully stops the analytics service. It holds no resources of its own.
func (s *service) Stop() error {
	return nil
}

// ValidateLogPath validates the log path against whitelist
func (s *service) ValidateLogPath(logPath string) error {
	if logPath == "" {
		return nil // Empty path is acceptable for global search
	}
	if !utils.IsValidLogPath(logPath) {
		return utils.ErrPathNotWhitelisted
	}
	return nil
}

// ValidateTimeRange validates the time range parameters
func (s *service) ValidateTimeRange(startTime, endTime int64) error {
	if startTime < 0 || endTime < 0 {
		return fmt.Errorf("time values cannot be negative")
	}

	if startTime > 0 && endTime > 0 && startTime >= endTime {
		return fmt.Errorf("start time must be before end time")
	}

	return nil
}

// buildBaseSearchRequest builds a base search request with common parameters
func (s *service) buildBaseSearchRequest(startTime, endTime int64, logPath string) *searcher.SearchRequest {
	req := &searcher.SearchRequest{
		Limit:    DefaultLimit,
		Offset:   0,
		UseCache: true,
	}

	if startTime > 0 {
		req.StartTime = &startTime
	}

	if endTime > 0 {
		req.EndTime = &endTime
	}

	if logPath != "" {
		req.LogPaths = []string{logPath}
	}

	return req
}

// validateAndNormalizeSearchRequest validates and normalizes a search request
func (s *service) validateAndNormalizeSearchRequest(req *searcher.SearchRequest) error {
	if req == nil {
		return fmt.Errorf("request cannot be nil")
	}

	if req.Limit <= 0 {
		req.Limit = DefaultLimit
	}

	if req.Limit > MaxLimit {
		req.Limit = MaxLimit
	}

	if req.Offset < 0 {
		req.Offset = 0
	}

	return nil
}
