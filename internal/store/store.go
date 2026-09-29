// Package store owns the SQLite database that keeps the per file indexing
// state (position, size, document count, time range and status). The Bleve
// index itself lives next to it in the data directory.
package store

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync/atomic"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite" // registers the pure Go "sqlite" database/sql driver
)

// DatabaseFile is the file name of the state database inside the data directory.
const DatabaseFile = "index.db"

// ErrNotOpen is returned when the database was not opened yet.
var ErrNotOpen = errors.New("nginx log metadata database is not initialized")

var current atomic.Pointer[gorm.DB]

// DB returns the open database or ErrNotOpen.
func DB() (*gorm.DB, error) {
	db := current.Load()
	if db == nil {
		return nil, ErrNotOpen
	}
	return db, nil
}

// Use installs an already opened database as the default one. It returns the
// previously installed database, which tests use to restore it.
func Use(db *gorm.DB) *gorm.DB {
	return current.Swap(db)
}

// Open opens the state database in dataDir, migrates it and installs it as the
// default one.
//
// The pure Go driver keeps the plugin free of cgo, so one static binary per
// platform is enough. The dialector is the one the host used, only the
// database/sql driver underneath is different.
func Open(dataDir string) (*gorm.DB, error) {
	db, err := open(dsn(filepath.Join(dataDir, DatabaseFile)))
	if err != nil {
		return nil, err
	}
	Use(db)
	return db, nil
}

var memoryCounter atomic.Uint64

// OpenMemory opens a fresh in-memory database that all its connections share,
// for tests.
func OpenMemory() (*gorm.DB, error) {
	name := fmt.Sprintf("file:nginx-log-mem-%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_time_format=sqlite", memoryCounter.Add(1))
	return open(name)
}

// Close closes the default database and removes it.
func Close() error {
	db := current.Swap(nil)
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// dsn builds the connection string of a database file. WAL keeps readers from
// blocking the writers, the busy timeout makes concurrent writers wait, and
// immediate transactions take the write lock when they begin so a
// read-then-write transaction cannot fail on lock upgrade.
func dsn(path string) string {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(10000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(NORMAL)")
	query.Set("_txlock", "immediate")
	// ISO 8601 text, which sorts and compares like time.
	query.Set("_time_format", "sqlite")
	return "file:" + filepath.ToSlash(path) + "?" + query.Encode()
}

func open(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: dsn}, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open nginx log metadata database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(8)

	if err := db.AutoMigrate(&NginxLogIndex{}); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate nginx log metadata database: %w", err)
	}
	return db, nil
}
