package utils

import (
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"

	"github.com/nginxui/plugin-log-analytics/internal/apierr"
)

// ErrPathNotWhitelisted answers a request for a log the host does not list.
var ErrPathNotWhitelisted = apierr.NewScope("nginx_log").New(50014, "log path is not under whitelist")

// HostLog is one nginx log file the host lists for the plugin.
type HostLog struct {
	Path       string
	Type       string // "access" or "error"
	Source     string // "config" or "default"
	ConfigFile string
}

// hostLogs is the current answer of host.logs.list. It is replaced as a whole,
// so readers never see a half updated list.
type hostLogs struct {
	entries []HostLog
	listed  map[string]struct{}
}

var currentHostLogs atomic.Pointer[hostLogs]

// SetHostLogs replaces the set of log files the plugin may read. Every path is
// cleaned first. IsValidLogPath allows exactly these paths and their rotated
// siblings, nothing else.
func SetHostLogs(logs []HostLog) {
	next := &hostLogs{
		entries: make([]HostLog, 0, len(logs)),
		listed:  make(map[string]struct{}, len(logs)),
	}
	for _, entry := range logs {
		if entry.Path == "" || !filepath.IsAbs(entry.Path) {
			continue
		}
		entry.Path = filepath.Clean(entry.Path)
		if _, dup := next.listed[entry.Path]; dup {
			continue
		}
		next.listed[entry.Path] = struct{}{}
		next.entries = append(next.entries, entry)
	}
	currentHostLogs.Store(next)
}

// HostLogList returns a copy of the log files the host lists.
func HostLogList() []HostLog {
	current := currentHostLogs.Load()
	if current == nil {
		return nil
	}
	return slices.Clone(current.entries)
}

// DefaultAccessLogPath returns the nginx default access log, or an empty
// string when the host lists none.
func DefaultAccessLogPath() string {
	current := currentHostLogs.Load()
	if current == nil {
		return ""
	}
	for _, entry := range current.entries {
		if entry.Type == "access" && entry.Source == "default" {
			return entry.Path
		}
	}
	return ""
}

// listedBase returns the listed log path that logPath is or belongs to as a
// rotated file (access.log.1, access.log.2.gz, access.log-20240101, ...).
func listedBase(logPath string) (string, bool) {
	current := currentHostLogs.Load()
	if current == nil {
		return "", false
	}
	if _, ok := current.listed[logPath]; ok {
		return logPath, true
	}
	if main := MainLogPathFromFile(logPath); main != logPath {
		if _, ok := current.listed[main]; ok {
			return main, true
		}
	}
	return "", false
}

// IsValidLogPath is the only gate for reading log files. A path is valid when:
//  1. it is a path the host lists, or a rotated file of one, and
//  2. it is a regular file or a symlink to a regular file, so a console or
//     other device is never opened. A path that does not exist is accepted: the
//     log may be created later, and historical index data can outlive the file.
//
// A symlink that is not itself listed must resolve into the directory of the
// listed log, which keeps a rotated file from pointing somewhere else.
func IsValidLogPath(logPath string) bool {
	if logPath == "" {
		return false
	}
	logPath = filepath.Clean(logPath)
	if !filepath.IsAbs(logPath) {
		return false
	}

	base, ok := listedBase(logPath)
	if !ok {
		return false
	}

	info, err := os.Lstat(logPath)
	if err != nil {
		return true
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return info.Mode().IsRegular()
	}

	resolved, err := filepath.EvalSymlinks(logPath)
	if err != nil {
		return false
	}
	if logPath != base && !sameDir(resolved, base) {
		return false
	}
	target, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	return target.Mode().IsRegular()
}

// sameDir reports whether resolved sits in the directory of base, following
// symlinks in both.
func sameDir(resolved, base string) bool {
	baseDir, err := filepath.EvalSymlinks(filepath.Dir(base))
	if err != nil {
		baseDir = filepath.Dir(base)
	}
	return filepath.Dir(resolved) == baseDir
}
