package store

import "time"

// NeedsIndexing checks if the file needs incremental indexing
func (nli *NginxLogIndex) NeedsIndexing(fileModTime time.Time, fileSize int64) bool {
	// If never indexed, needs full indexing
	if nli.LastIndexed.IsZero() {
		return true
	}

	// If file was modified after last index and size increased, needs incremental indexing
	if fileModTime.After(nli.LastModified) && fileSize > nli.LastSize {
		return true
	}

	// If file size decreased, file might have been rotated, needs full re-indexing
	if fileSize < nli.LastSize {
		return true
	}

	return false
}

// ShouldFullReindex checks if a full re-index is needed (file rotation detected)
func (nli *NginxLogIndex) ShouldFullReindex(fileModTime time.Time, fileSize int64) bool {
	// File size decreased - likely file rotation
	if fileSize < nli.LastSize {
		return true
	}

	// File significantly older than last index - might be a replaced file
	if fileModTime.Before(nli.LastModified.Add(-time.Hour)) {
		return true
	}

	return false
}

// UpdateProgress updates the indexing progress
func (nli *NginxLogIndex) UpdateProgress(modTime time.Time, size int64, position int64, docCount uint64, timeStart, timeEnd *time.Time) {
	nli.LastModified = modTime
	nli.LastSize = size
	nli.LastPosition = position
	nli.LastIndexed = time.Now()
	nli.DocumentCount = docCount

	// Merge time ranges: preserve existing historical range and expand if necessary
	// This prevents incremental updates from losing historical time range data
	if timeStart != nil {
		if nli.TimeRangeStart == nil || timeStart.Before(*nli.TimeRangeStart) {
			nli.TimeRangeStart = timeStart
		}
	}
	if timeEnd != nil {
		if nli.TimeRangeEnd == nil || timeEnd.After(*nli.TimeRangeEnd) {
			nli.TimeRangeEnd = timeEnd
		}
	}
}

// SetIndexStartTime records when indexing operation started
func (nli *NginxLogIndex) SetIndexStartTime(startTime time.Time) {
	nli.IndexStartTime = &startTime
}

// SetIndexDuration records how long the indexing operation took
func (nli *NginxLogIndex) SetIndexDuration(startTime time.Time) {
	// If IndexStartTime is not set, set it to the provided startTime
	if nli.IndexStartTime == nil {
		nli.IndexStartTime = &startTime
	}
	duration := time.Since(startTime).Milliseconds()
	nli.IndexDuration = &duration
}

// Reset clears all index position data for full re-indexing
func (nli *NginxLogIndex) Reset() {
	nli.LastModified = time.Time{} // Clear last modified time
	nli.LastSize = 0               // Clear index size
	nli.LastPosition = 0
	nli.Fingerprint = ""
	nli.SyncVersion = 0
	nli.LastIndexed = time.Time{} // Clear last indexed time
	nli.IndexStartTime = nil      // Clear index start time
	nli.IndexDuration = nil       // Clear index duration
	nli.DocumentCount = 0
	nli.TimeRangeStart = nil
	nli.TimeRangeEnd = nil

	// Reset status fields
	nli.IndexStatus = "not_indexed"
	nli.ErrorMessage = ""
	nli.ErrorTime = nil
	nli.RetryCount = 0
	nli.QueuePosition = 0
}

// SetIndexingStatus updates the indexing status
func (nli *NginxLogIndex) SetIndexingStatus(status string) {
	nli.IndexStatus = status
	if status == "indexing" {
		now := time.Now()
		nli.IndexStartTime = &now
	}
}

// SetErrorStatus records an error status with message
func (nli *NginxLogIndex) SetErrorStatus(errorMessage string) {
	nli.IndexStatus = "error"
	nli.ErrorMessage = errorMessage
	now := time.Now()
	nli.ErrorTime = &now
	nli.RetryCount++
}

// SetCompletedStatus marks indexing as completed successfully
func (nli *NginxLogIndex) SetCompletedStatus() {
	nli.IndexStatus = "indexed"
	nli.ErrorMessage = ""
	nli.ErrorTime = nil
	nli.QueuePosition = 0
}

// SetQueuedStatus marks the file as queued for indexing
func (nli *NginxLogIndex) SetQueuedStatus(position int) {
	nli.IndexStatus = "queued"
	nli.QueuePosition = position
}

// IsHealthy returns true if the index is in a good state
func (nli *NginxLogIndex) IsHealthy() bool {
	return nli.IndexStatus == "indexed" || nli.IndexStatus == "indexing"
}

// CanRetry returns true if the file can be retried for indexing
func (nli *NginxLogIndex) CanRetry() bool {
	return nli.IndexStatus == "error"
}
