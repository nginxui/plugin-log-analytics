package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/require"
)

// The endpoints answer with the keys and kinds the web pages declare in
// src/api/types.ts of plugin-log-analytics-webapp, read from a checkout next
// to this repository or from LOG_ANALYTICS_WEBAPP_DIR. The Rust plugin runs
// the same check, so both answer what the shared pages read.

// tsField is a field of a TypeScript interface.
type tsField struct {
	optional bool
	ty       string
}

type tsInterfaces map[string]map[string]tsField

func webappDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("LOG_ANALYTICS_WEBAPP_DIR"); dir != "" {
		return dir
	}
	return filepath.Join("..", "..", "..", "plugin-log-analytics-webapp")
}

// parseTypes reads the interfaces of types.ts. Only the fields at the top
// level of an interface are read, which is what the pages declare.
func parseTypes(t *testing.T) tsInterfaces {
	t.Helper()
	path := filepath.Join(webappDir(t), "src", "api", "types.ts")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "check out plugin-log-analytics-webapp next to this repository")

	identifier := func(s string) string {
		end := strings.IndexFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if end < 0 {
			return s
		}
		return s[:end]
	}

	out := tsInterfaces{}
	current, depth := "", 0
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if current == "" {
			rest, ok := strings.CutPrefix(trimmed, "export interface ")
			if !ok {
				continue
			}
			current, depth = identifier(rest), 1
			out[current] = map[string]tsField{}
			if _, base, found := strings.Cut(rest, " extends "); found {
				for key, field := range out[identifier(base)] {
					out[current][key] = field
				}
			}
			continue
		}
		if depth == 1 && !strings.HasPrefix(trimmed, "/**") && !strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "//") {
			if key, ty, found := strings.Cut(trimmed, ":"); found {
				optional := strings.HasSuffix(key, "?")
				key = strings.TrimSpace(strings.TrimSuffix(key, "?"))
				if key != "" && strings.IndexFunc(key, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) < 0 {
					out[current][key] = tsField{optional: optional, ty: strings.TrimSpace(ty)}
				}
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			current = ""
		}
	}
	return out
}

// shapeErr checks a value against an interface: every declared field that is
// not optional is there, and every field has the kind its type says.
func shapeErr(types tsInterfaces, iface string, value any, path string) error {
	fields, ok := types[iface]
	if !ok {
		return fmt.Errorf("no interface %s", iface)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: expected an object for %s, got %v", path, iface, value)
	}
	for key, field := range fields {
		v, present := object[key]
		if !present {
			if !field.optional {
				return fmt.Errorf("%s.%s is missing (%s)", path, key, iface)
			}
			continue
		}
		if err := kindErr(types, field.ty, v, path+"."+key); err != nil {
			return err
		}
	}
	return nil
}

func kindErr(types tsInterfaces, ty string, value any, path string) error {
	ty = strings.TrimSuffix(strings.TrimSpace(ty), ",")
	kind := func(ok bool, want string) error {
		if ok {
			return nil
		}
		return fmt.Errorf("%s: expected %s, got %v", path, want, value)
	}
	switch {
	case strings.HasSuffix(ty, "[]"):
		items, ok := value.([]any)
		if !ok {
			return kind(false, "an array")
		}
		for i, item := range items {
			if i == 3 {
				break
			}
			if err := kindErr(types, strings.TrimSuffix(ty, "[]"), item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case strings.HasPrefix(ty, "(") && strings.HasSuffix(ty, ")"):
		// A union of interfaces: the value matches one of them
		var errs []string
		for _, member := range strings.Split(strings.Trim(ty, "()"), "|") {
			err := kindErr(types, member, value, path)
			if err == nil {
				return nil
			}
			errs = append(errs, err.Error())
		}
		return fmt.Errorf("%s matches no member of %s: %s", path, ty, strings.Join(errs, "; "))
	case strings.HasPrefix(ty, "number"):
		_, ok := value.(float64)
		return kind(ok, "a number")
	case strings.HasPrefix(ty, "boolean"):
		_, ok := value.(bool)
		return kind(ok, "a boolean")
	case strings.HasPrefix(ty, "string") || strings.Contains(ty, "'") || strings.Contains(ty, "| string"):
		_, ok := value.(string)
		return kind(ok, "a string")
	case strings.HasPrefix(ty, "{"):
		_, ok := value.(map[string]any)
		return kind(ok, "an object")
	default:
		if _, known := types[ty]; known {
			return shapeErr(types, ty, value, path)
		}
		return nil
	}
}

func checkShape(t *testing.T, types tsInterfaces, iface string, value any, path string) {
	t.Helper()
	require.NoError(t, shapeErr(types, iface, value, path))
}

func checkKind(t *testing.T, types tsInterfaces, ty string, value any, path string) {
	t.Helper()
	require.NoError(t, kindErr(types, ty, value, path))
}

func TestTheParserOfTypesReadsTheDeclarations(t *testing.T) {
	types := parseTypes(t)
	require.Contains(t, types["AccessLogEntry"], "browser_version")
	require.True(t, types["PreflightResponse"]["time_range"].optional)
	require.False(t, types["DashboardSummary"]["total_uv"].optional)
	require.Contains(t, types["RegionMapData"], "level")
}

func TestEndpointsFollowTheTypesOfThePages(t *testing.T) {
	types := parseTypes(t)
	env := startHTTPEnv(t, 30)
	start := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC).Unix()
	end := start + 86400
	rangeBody := map[string]any{"path": env.logPath, "start_time": start, "end_time": end}

	answer := func(method, path string, body any) any {
		t.Helper()
		recorder := env.do(t, method, path, body)
		require.Equal(t, http.StatusOK, recorder.Code, "%s %s: %s", method, path, recorder.Body.String())
		var out any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
		return out
	}
	field := func(value any, key string) any {
		return value.(map[string]any)[key]
	}

	checkShape(t, types, "LogStatusResponse", answer(http.MethodGet, "/logs/status", nil), "status")

	search := answer(http.MethodPost, "/search", map[string]any{"query": "", "limit": 10, "log_path": env.logPath})
	checkShape(t, types, "AdvancedSearchResponse", search, "search")
	require.NotEmpty(t, field(search, "entries"))

	entries := answer(http.MethodGet, "/entries?limit=5&path="+url.QueryEscape(env.logPath), nil)
	if list, _ := field(entries, "entries").([]any); len(list) > 0 {
		checkShape(t, types, "AccessLogEntry", list[0], "entries[0]")
	}

	dashboard := answer(http.MethodPost, "/dashboard", map[string]any{"log_path": env.logPath, "start_date": "2026-08-12", "end_date": "2026-08-12"})
	checkShape(t, types, "DashboardAnalytics", dashboard, "dashboard")

	checkKind(t, types, "WorldMapData[]", field(answer(http.MethodPost, "/geo/world", rangeBody), "data"), "world.data")
	regions := map[string]any{"path": env.logPath, "start_time": start, "end_time": end, "country": "US"}
	checkKind(t, types, "RegionMapData[]", field(answer(http.MethodPost, "/geo/regions", regions), "data"), "regions.data")
	checkKind(t, types, "CityPointData[]", field(answer(http.MethodPost, "/geo/points", rangeBody), "data"), "points.data")

	checkShape(t, types, "PreflightResponse", answer(http.MethodGet, "/preflight?log_path="+url.QueryEscape(env.logPath), nil), "preflight")
	checkShape(t, types, "GeoLiteStatus", answer(http.MethodGet, "/geolite/status", nil), "geolite")
}
