package store

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// BaseModelUUID is a base model with a UUID primary key.
type BaseModelUUID struct {
	ID        uuid.UUID `gorm:"type:uuid;primary_key;" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BeforeCreate sets a UUID rather than a numeric ID.
func (base *BaseModelUUID) BeforeCreate(_ *gorm.DB) error {
	if base.ID == uuid.Nil {
		base.ID = uuid.New()
	}
	return nil
}

// NginxLogIndex represents the incremental index position and metadata for a log file.
// The table and the JSON field names are the ones the host wrote before the
// log analytics moved into this plugin, so old rows import unchanged.
type NginxLogIndex struct {
	BaseModelUUID
	Path           string     `gorm:"uniqueIndex;size:500;not null" json:"path"` // Log file path
	MainLogPath    string     `gorm:"index;size:500" json:"main_log_path"`       // Main log path for grouping related files (access.log for access.log.1, access.log.1.gz, etc.)
	LastModified   time.Time  `json:"last_modified"`                             // File last modified time when indexed
	LastSize       int64      `gorm:"default:0" json:"last_size"`                // Total index size of all related log files when last indexed
	LastPosition   int64      `gorm:"default:0" json:"last_position"`            // Last byte position indexed in file
	LastIndexed    time.Time  `json:"last_indexed"`                              // When file was last indexed
	IndexStartTime *time.Time `json:"index_start_time"`                          // When the last indexing operation started
	IndexDuration  *int64     `json:"index_duration"`                            // Duration of last indexing operation in milliseconds
	TimeRangeStart *time.Time `json:"timerange_start"`                           // Earliest log entry time
	TimeRangeEnd   *time.Time `json:"timerange_end"`                             // Latest log entry time
	DocumentCount  uint64     `gorm:"default:0" json:"document_count"`           // Total documents indexed from this file
	Enabled        bool       `gorm:"default:true" json:"enabled"`               // Whether indexing is enabled for this file
	HasTimeRange   bool       `gorm:"-" json:"has_timerange"`                    // Whether a time range is available (not persisted)

	// Content tracking. LastPosition is an offset in the decompressed stream
	// and Fingerprint identifies the content by its first line, so a file that
	// was renamed, copied or compressed is recognized by what it holds and not
	// by its path. A row with SyncVersion 0 was written before this tracking.
	Fingerprint string `gorm:"size:64" json:"fingerprint"`    // Hash of the first line of the content
	SyncVersion int    `gorm:"default:0" json:"sync_version"` // Version of the tracking that wrote this row

	// Extended status fields
	IndexStatus   string     `gorm:"default:'not_indexed';size:50" json:"index_status"` // Current index status
	ErrorMessage  string     `gorm:"type:text" json:"error_message,omitempty"`          // Last error message
	ErrorTime     *time.Time `json:"error_time,omitempty"`                              // When error occurred
	RetryCount    int        `gorm:"default:0" json:"retry_count"`                      // Number of retry attempts
	QueuePosition int        `gorm:"default:0" json:"queue_position,omitempty"`         // Position in indexing queue
}
