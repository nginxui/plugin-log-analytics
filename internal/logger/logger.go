// Package logger routes the plugin log lines through the SDK logger, which
// writes to stderr and mirrors info and above to the host log.
//
// Debug lines are dropped before they are formatted unless the process runs
// with NGINX_UI_PLUGIN_DEBUG=1: the indexing paths log per file and per batch,
// and formatting those lines would cost more than the work they describe.
package logger

import (
	"fmt"
	"os"
	"sync/atomic"

	sdk "github.com/nginxui/plugin-sdk-go"
)

// EnvDebug turns debug lines on when set to 1.
const EnvDebug = "NGINX_UI_PLUGIN_DEBUG"

var debugEnabled atomic.Bool

func init() {
	debugEnabled.Store(os.Getenv(EnvDebug) == "1")
}

// SetDebug switches debug lines on or off.
func SetDebug(enabled bool) { debugEnabled.Store(enabled) }

// DebugEnabled reports whether debug lines are written.
func DebugEnabled() bool { return debugEnabled.Load() }

// Debug logs at debug level.
func Debug(args ...any) {
	if debugEnabled.Load() {
		sdk.Logger.Log(sdk.LevelDebug, fmt.Sprint(args...), nil)
	}
}

// Debugf logs a formatted message at debug level.
func Debugf(format string, args ...any) {
	if debugEnabled.Load() {
		sdk.Debugf(format, args...)
	}
}

// Info logs at info level.
func Info(args ...any) { sdk.Logger.Log(sdk.LevelInfo, fmt.Sprint(args...), nil) }

// Infof logs a formatted message at info level.
func Infof(format string, args ...any) { sdk.Infof(format, args...) }

// Warn logs at warn level.
func Warn(args ...any) { sdk.Logger.Log(sdk.LevelWarn, fmt.Sprint(args...), nil) }

// Warnf logs a formatted message at warn level.
func Warnf(format string, args ...any) { sdk.Warnf(format, args...) }

// Error logs at error level.
func Error(args ...any) { sdk.Logger.Log(sdk.LevelError, fmt.Sprint(args...), nil) }

// Errorf logs a formatted message at error level.
func Errorf(format string, args ...any) { sdk.Errorf(format, args...) }
