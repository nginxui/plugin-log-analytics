package geolite

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ulikunitz/xz"

	"github.com/nginxui/plugin-log-analytics/internal/apierr"
	"github.com/nginxui/plugin-log-analytics/internal/config"
)

const (
	DownloadURL = "http://cloud.nginxui.com/geolite/GeoLite2-City.mmdb.xz"
)

type DownloadProgressWriter struct {
	io.Writer
	totalSize      int64
	currentSize    int64
	progressChan   chan<- float64
	lastReported   float64
	reportInterval float64 // Report only when progress changes by this amount
}

func (pw *DownloadProgressWriter) Write(p []byte) (int, error) {
	n, err := pw.Writer.Write(p)
	pw.currentSize += int64(n)
	progress := float64(pw.currentSize) / float64(pw.totalSize) * 100

	// Debounce: only send updates when progress changes by reportInterval or reaches 100%
	if progress-pw.lastReported >= pw.reportInterval || progress >= 100 {
		select {
		case pw.progressChan <- progress:
			pw.lastReported = progress
		default:
		}
	}
	return n, err
}

// GetDBPath returns the path to the GeoLite2 database file
func GetDBPath() string {
	defaultPath := getDefaultDBPath()
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath
	}

	if customPath := getCustomDBPath(); customPath != "" {
		return customPath
	}

	return defaultPath
}

// GetDBXZPath returns the path to the compressed GeoLite2 database file
func GetDBXZPath() string {
	return filepath.Join(config.GeoLiteDir(), "GeoLite2-City.mmdb.xz")
}

func getDefaultDBPath() string {
	return filepath.Join(config.GeoLiteDir(), "GeoLite2-City.mmdb")
}

func getCustomDBPath() string {
	customPath := config.Get().IndexCustomMMDB
	if customPath == "" {
		return ""
	}

	if filepath.IsAbs(customPath) {
		return customPath
	}

	return filepath.Join(config.GeoLiteDir(), customPath)
}

// newHTTPClient returns the client used for the download. It follows the proxy
// environment of the plugin process.
func newHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
}

// DownloadGeoLiteDB downloads the GeoLite2 database. Canceling ctx stops the
// download.
func DownloadGeoLiteDB(ctx context.Context, progressChan chan float64) error {
	client := newHTTPClient()

	if err := os.MkdirAll(config.GeoLiteDir(), 0o755); err != nil {
		return apierr.WithParams(ErrFailedToCreateFile, err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, "GET", DownloadURL, nil)
	if err != nil {
		return apierr.WithParams(ErrDownloadFailed, err.Error())
	}

	resp, err := client.Do(req)
	if err != nil {
		return apierr.WithParams(ErrDownloadFailed, err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apierr.WithParams(ErrDownloadFailed, fmt.Sprintf("status code: %d", resp.StatusCode))
	}

	totalSize, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return apierr.WithParams(ErrFailedToGetFileSize, err.Error())
	}

	xzPath := GetDBXZPath()
	file, err := os.Create(xzPath)
	if err != nil {
		return apierr.WithParams(ErrFailedToCreateFile, err.Error())
	}
	defer file.Close()

	progressWriter := &DownloadProgressWriter{
		Writer:         file,
		totalSize:      totalSize,
		progressChan:   progressChan,
		reportInterval: 1.0, // Report every 1% change
	}

	_, err = io.Copy(progressWriter, resp.Body)
	if err != nil {
		os.Remove(xzPath) // Clean up on error
		return apierr.WithParams(ErrFailedToSaveFile, err.Error())
	}

	return nil
}

// DecompressGeoLiteDB decompresses the .xz file to .mmdb
func DecompressGeoLiteDB(progressChan chan float64) error {
	xzPath := GetDBXZPath()
	// Always the default location: GetDBPath may name a custom database, which
	// a download must never overwrite.
	dbPath := getDefaultDBPath()

	// Open compressed file
	xzFile, err := os.Open(xzPath)
	if err != nil {
		return apierr.WithParams(ErrFailedToOpenFile, err.Error())
	}
	defer xzFile.Close()

	// Get compressed file size
	fileInfo, err := xzFile.Stat()
	if err != nil {
		return apierr.WithParams(ErrFailedToGetFileSize, err.Error())
	}
	compressedSize := fileInfo.Size()

	// Create XZ reader
	xzReader, err := xz.NewReader(xzFile)
	if err != nil {
		return apierr.WithParams(ErrFailedToCreateXZReader, err.Error())
	}

	// Write next to the final name and rename when done: an indexing round
	// that still maps the previous database keeps reading the old file.
	tmpPath := dbPath + ".tmp"
	outFile, err := os.Create(tmpPath)
	if err != nil {
		return apierr.WithParams(ErrFailedToCreateFile, err.Error())
	}
	defer outFile.Close()

	// Decompress with progress tracking
	buf := make([]byte, 64*1024) // 64KB buffer for better performance
	var decompressedSize int64
	var lastReportedProgress float64
	const reportInterval = 2.0 // Report every 2% change

	// Estimate: XZ typically compresses to 10-20% of original size
	// We'll use 15% (compression ratio ~6.67) as middle estimate
	const estimatedCompressionRatio = 6.67
	estimatedTotalSize := float64(compressedSize) * estimatedCompressionRatio

	for {
		n, readErr := xzReader.Read(buf)
		if n > 0 {
			if _, writeErr := outFile.Write(buf[:n]); writeErr != nil {
				os.Remove(tmpPath) // Clean up on error
				return apierr.WithParams(ErrFailedToWriteData, writeErr.Error())
			}
			decompressedSize += int64(n)

			// Calculate progress based on estimated total size
			progress := (float64(decompressedSize) / estimatedTotalSize) * 100
			if progress > 99 {
				progress = 99 // Cap at 99% until actually complete
			}

			// Debounce: only send updates when progress changes significantly
			if progress-lastReportedProgress >= reportInterval || readErr == io.EOF {
				select {
				case progressChan <- progress:
					lastReportedProgress = progress
				default:
				}
			}
		}
		if readErr == io.EOF {
			// Send 100% on completion
			select {
			case progressChan <- 100:
			default:
			}
			break
		}
		if readErr != nil {
			os.Remove(tmpPath) // Clean up on error
			return apierr.WithParams(ErrFailedToReadData, readErr.Error())
		}
	}

	if err := outFile.Close(); err != nil {
		os.Remove(tmpPath)
		return apierr.WithParams(ErrFailedToWriteData, err.Error())
	}
	if err := os.Rename(tmpPath, dbPath); err != nil {
		os.Remove(tmpPath)
		return apierr.WithParams(ErrFailedToWriteData, err.Error())
	}

	// Delete the .xz file after successful decompression
	if err := os.Remove(xzPath); err != nil {
		// Log but don't fail if we can't delete the compressed file
		return apierr.WithParams(ErrFailedToDeleteCompressed, err.Error())
	}

	return nil
}

// DBExists checks if the GeoLite2 database file exists
func DBExists() bool {
	_, err := os.Stat(GetDBPath())
	return err == nil
}
