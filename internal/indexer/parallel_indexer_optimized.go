package indexer

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// IndexLogFile reads and indexes a single log file using the streaming
// pipeline. Kept as a thin compatibility wrapper around IndexSingleFile.
func (pi *ParallelIndexer) IndexLogFile(filePath string) error {
	_, _, _, err := pi.IndexSingleFile(filePath)
	return err
}

// IndexSingleFile contains the logic to process one physical log file.
// It returns the number of documents indexed from the file, and the min/max timestamps.
func (pi *ParallelIndexer) IndexSingleFile(filePath string) (uint64, *time.Time, *time.Time, error) {
	return pi.IndexSingleFileWithProgress(filePath, nil)
}

// IndexSingleFileWithProgress reads one file from its start, whatever the
// stored state says, and stores the new state. The documents carry ids that
// depend on the content, so a second read of the same file replaces them. The
// counts are the documents written by this read.
func (pi *ParallelIndexer) IndexSingleFileWithProgress(filePath string, progressTracker *ProgressTracker) (uint64, *time.Time, *time.Time, error) {
	// Validate log path before accessing it
	if !utils.IsValidLogPath(filePath) {
		return 0, nil, nil, fmt.Errorf("invalid log path: %s", filePath)
	}

	if progressTracker != nil {
		if info, err := os.Stat(filePath); err == nil {
			progressTracker.SetFileSize(filePath, info.Size())
			progressTracker.SetFileEstimate(filePath, max(info.Size()/150, 100))
		}
	}

	logger.Infof("Starting to process file: %s", filePath)

	snapshot := loadGroupSnapshot(getMainLogPathFromFile(filePath))
	file, err := pi.syncTrackedFile(filePath, getMainLogPathFromFile(filePath), snapshot, true, progressTracker)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to index file %s: %w", filePath, err)
	}

	logger.Infof("Finished processing file: %s. Total documents indexed: %d", filePath, file.Docs)
	return file.Docs, file.MinTime, file.MaxTime, nil
}

// isValidLogEntry validates if a parsed log entry is correct
func isValidLogEntry(doc *LogDocument) bool {
	if doc == nil {
		return false
	}

	// Check IP address - should be a valid IP format
	// Allow empty IP for now but reject obvious non-IP strings
	if doc.IP != "" && doc.IP != "-" {
		// Simple check: IP shouldn't contain URLs, paths, or binary data
		if strings.Contains(doc.IP, "http") ||
			strings.Contains(doc.IP, "/") ||
			strings.Contains(doc.IP, "\\x") ||
			strings.Contains(doc.IP, "%") ||
			len(doc.IP) > 45 { // Max IPv6 length is 45 chars
			return false
		}
	}

	// Check timestamp - should be reasonable (not 0, not in far future)
	now := time.Now().Unix()
	if doc.Timestamp <= 0 || doc.Timestamp > now+86400 { // Allow up to 1 day in future
		return false
	}

	// Check HTTP method if present
	if doc.Method != "" && !validHTTPMethods[doc.Method] {
		return false
	}

	// Check status code - should be in valid HTTP range
	if doc.Status != 0 && (doc.Status < 100 || doc.Status > 599) {
		return false
	}

	// Check for binary data in path
	if strings.Contains(doc.Path, "\\x") {
		return false
	}

	// If raw log line contains obvious binary data, reject it
	if strings.Contains(doc.Raw, "\\x16\\x03") || // SSL/TLS handshake
		strings.Contains(doc.Raw, "\\xFF\\xD8") { // JPEG header
		return false
	}

	return true
}

// validHTTPMethods contains the standard HTTP and WebDAV (RFC 4918) methods
// accepted during document validation. Package-level so the per-document
// validation loop does not allocate a map per call.
var validHTTPMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"HEAD": true, "OPTIONS": true, "PATCH": true, "CONNECT": true, "TRACE": true,
	"PROPFIND": true, "PROPPATCH": true, "MKCOL": true,
	"COPY": true, "MOVE": true, "LOCK": true, "UNLOCK": true,
}
