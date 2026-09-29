package analytics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/nginxui/plugin-log-analytics/internal/searcher"
)

// MockSearcher implements searcher.Searcher for testing
type MockSearcher struct {
	mock.Mock
}

func (m *MockSearcher) Search(ctx context.Context, req *searcher.SearchRequest) (*searcher.SearchResult, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*searcher.SearchResult), args.Error(1)
}

func (m *MockSearcher) ClearCache() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockSearcher) GetCacheStats() *searcher.CacheStats {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*searcher.CacheStats)
}

func (m *MockSearcher) IsHealthy() bool {
	args := m.Called()
	return args.Bool(0)
}

func (m *MockSearcher) IsRunning() bool {
	args := m.Called()
	return args.Bool(0)
}

func (m *MockSearcher) GetStats() *searcher.Stats {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*searcher.Stats)
}

func (m *MockSearcher) GetConfig() *searcher.Config {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*searcher.Config)
}

func (m *MockSearcher) Stop() error {
	args := m.Called()
	return args.Error(0)
}

// MockCardinalityCounter implements searcher.Counter for testing
type MockCardinalityCounter struct {
	mock.Mock
}

func (m *MockCardinalityCounter) CountCardinality(ctx context.Context, req *searcher.CardinalityRequest) (*searcher.CardinalityResult, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*searcher.CardinalityResult), args.Error(1)
}

func (m *MockCardinalityCounter) EstimateCardinality(ctx context.Context, req *searcher.CardinalityRequest) (*searcher.CardinalityResult, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*searcher.CardinalityResult), args.Error(1)
}

func (m *MockCardinalityCounter) BatchCountCardinality(ctx context.Context, fields []string, baseReq *searcher.CardinalityRequest) (map[string]*searcher.CardinalityResult, error) {
	args := m.Called(ctx, fields, baseReq)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(map[string]*searcher.CardinalityResult), args.Error(1)
}

func TestNewService(t *testing.T) {
	mockSearcher := &MockSearcher{}
	service := NewService(mockSearcher)

	assert.NotNil(t, service)
	assert.Implements(t, (*Service)(nil), service)
}

func TestService_ValidateLogPath(t *testing.T) {
	mockSearcher := &MockSearcher{}
	s := NewService(mockSearcher)

	tests := []struct {
		name    string
		logPath string
		wantErr bool
	}{
		{
			name:    "empty path should be valid",
			logPath: "",
			wantErr: false,
		},
		// {
		// 	name:    "non-empty path should be invalid without whitelist",
		// 	logPath: "/var/log/nginx/access.log",
		// 	wantErr: true, // In test environment, no whitelist is configured
		// },
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.ValidateLogPath(tt.logPath)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestService_ValidateTimeRange(t *testing.T) {
	mockSearcher := &MockSearcher{}
	s := NewService(mockSearcher)

	tests := []struct {
		name      string
		startTime int64
		endTime   int64
		wantErr   bool
	}{
		{
			name:      "valid time range",
			startTime: 1000,
			endTime:   2000,
			wantErr:   false,
		},
		{
			name:      "same start and end time should error",
			startTime: 1000,
			endTime:   1000,
			wantErr:   true,
		},
		{
			name:      "start time after end time should error",
			startTime: 2000,
			endTime:   1000,
			wantErr:   true,
		},
		{
			name:      "negative start time should error",
			startTime: -1000,
			endTime:   2000,
			wantErr:   true,
		},
		{
			name:      "negative end time should error",
			startTime: 1000,
			endTime:   -2000,
			wantErr:   true,
		},
		{
			name:      "zero values should be valid",
			startTime: 0,
			endTime:   0,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.ValidateTimeRange(tt.startTime, tt.endTime)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestService_GetLogEntriesStats_Basic(t *testing.T) {
	mockSearcher := &MockSearcher{}
	s := NewService(mockSearcher)

	ctx := context.Background()
	req := &searcher.SearchRequest{
		Limit:  100,
		Offset: 0,
	}

	expectedResult := &searcher.SearchResult{
		TotalHits: 1000,
		Facets: map[string]*searcher.Facet{
			"status": {
				Terms: []*searcher.FacetTerm{
					{Term: "200", Count: 800},
					{Term: "404", Count: 150},
					{Term: "500", Count: 50},
				},
			},
			"method": {
				Terms: []*searcher.FacetTerm{
					{Term: "GET", Count: 700},
					{Term: "POST", Count: 300},
				},
			},
			"path_exact": {
				Terms: []*searcher.FacetTerm{
					{Term: "/api/users", Count: 400},
					{Term: "/api/posts", Count: 300},
				},
			},
			"ip": {
				Terms: []*searcher.FacetTerm{
					{Term: "192.168.1.1", Count: 500},
					{Term: "192.168.1.2", Count: 300},
				},
			},
			"user_agent": {
				Terms: []*searcher.FacetTerm{
					{Term: "Chrome", Count: 600},
					{Term: "Firefox", Count: 400},
				},
			},
		},
		Stats: &searcher.SearchStats{
			TotalBytes: 1000000,
			AvgBytes:   1000,
			MinBytes:   100,
			MaxBytes:   5000,
			AvgReqTime: 0.5,
			MinReqTime: 0.1,
			MaxReqTime: 2.0,
		},
	}

	mockSearcher.On("Search", ctx, mock.AnythingOfType("*searcher.SearchRequest")).Return(expectedResult, nil)

	result, err := s.GetLogEntriesStats(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, int64(1000), result.TotalEntries)
	assert.Equal(t, 800, result.StatusCodeDist["200"])
	assert.Equal(t, 150, result.StatusCodeDist["404"])
	assert.Equal(t, 700, result.MethodDist["GET"])
	assert.Equal(t, 300, result.MethodDist["POST"])
	assert.NotNil(t, result.BytesStats)
	assert.Equal(t, int64(1000000), result.BytesStats.Total)
	assert.NotNil(t, result.ResponseTimeStats)
	assert.Equal(t, 0.5, result.ResponseTimeStats.Average)

	mockSearcher.AssertExpectations(t)
}
