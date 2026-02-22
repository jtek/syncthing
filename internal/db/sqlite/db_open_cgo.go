// Copyright (C) 2025 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build cgo

package sqlite

import (
	"fmt"
	"log/slog"
	"database/sql"
	"database/sql/driver"
        "github.com/mattn/go-sqlite3" // register sqlite3 database driver
)

const (
	FolderDBDriver = "sqlite3_folder"
	MainDBDriver   = "sqlite3_main"
	commonOptions  = "_fk=true&_rt=true&_sync=1&_txlock=immediate"
	// TODO: maybe get a better estimate ?
	// (64 bit for integers, bools and timestamps, 512 bit for hash :
	// 832 bits rounded up to 1024 bits = 128 bytes to have a slightly oversized cache for the indexes
	filesRowIndexCost = 128
)

var folder_pragmas = []string{
	"journal_mode = WAL",
	"optimize = 0x10002",
	"auto_vacuum = INCREMENTAL",
	fmt.Sprintf("application_id = %d", applicationIDFolder),
	// This avoids blocked writes to fail immediately and especially checkpoint(TRUNCATE),
	// It depends on other connexions not locking the DB too long though (TODO)
	"busy_timeout = 5000",
	// even on large folders the temp store doesn't seem used for large data, memory is faster
	"temp_store = MEMORY",
	// Don't fsync on each commit but only during checkpoints which guarantees the DB is consistent
	// although last transactions might be missing (this is however OK for Synchting)
	"synchronous = NORMAL",
	// Note: this is a max target. SQLite checkpoints might fail to keep it below depending
	// on concurrent activity
	"journal_size_limit = 8388608",
	"cache_spill = FALSE", // avoids locking the DB during transactions at the cost of memory use
}
var main_pragmas = []string{
	"journal_mode = WAL",
	"optimize = 0x10002",
	"auto_vacuum = INCREMENTAL",
	fmt.Sprintf("application_id = %d", applicationIDMain),
}

func registerMainDriver() {
	sql.Register(MainDBDriver,
		&sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				slog.Debug("DB Connect (main)")
				for _, pragma := range main_pragmas {
					pragma_stmt := fmt.Sprintf("PRAGMA %s", pragma)
					slog.Debug(fmt.Sprintf("Main: %s", pragma_stmt))
					conn.Exec(pragma_stmt, nil)
				}
				return nil
			},
		})
}

func registerFolderDriver() {
	sql.Register(FolderDBDriver,
		&sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				slog.Debug("DB Connect (folder)")
				for _, pragma := range folder_pragmas {
					pragma_stmt := fmt.Sprintf("PRAGMA %s", pragma)
					slog.Debug(fmt.Sprintf("Folder: %s", pragma_stmt))
					conn.Exec(pragma_stmt, nil)
				}
				tuneFolderCacheSize(conn)
				return nil
			},
		})
}

func tuneFolderCacheSize(conn *sqlite3.SQLiteConn) {
	// the files table is repeatedly scanned to find entries to garbage collect
	// it uses conditions on name_idx, version_idx, deleted, sequence, modified and blocklist_hash
	// all of those have indexes except modified
	// This tunes the cache size according to these index sizes
	// Note: the dbstat virtual table giving actual size on disk is too slow and maybe not appropriate as it includes
	// unused space in pages. So we use an estimate of the rows count multiplied by an estimate of
	// index space used by each row
	// we count the files and blocklists tables to have a more reliable number of files
	// as files has been seen underevaluated by a large factor
	count_query := `SELECT CAST(SUBSTR(stat, 1, INSTR(stat, ' ') - 1) AS INTEGER) FROM sqlite_stat1 WHERE tbl = '%s' LIMIT 1`
	files_count := int64(0)
	blocklists_count := int64(0)
	target_cache_size := int64(0)

	// This is performance tuning, these are allowed to fail so log errors and continue
	for _, table := range []string{"files", "blocklists"} {
		stmt, err := conn.Prepare(fmt.Sprintf(count_query, table))
		if err != nil { slog.Warn("Couldn't prepare query for cache tuning", "error", err); continue }
		rows, err := stmt.Query(nil)
		if err != nil { slog.Warn("Couldn't execute query for cache tuning", "error", err); continue }
		results := make([]driver.Value, len(rows.Columns()))
		err = rows.Next(results)
		if err != nil { slog.Warn("Couldn't fetch row count estimate for cache tuning", "error", err); continue }
		rows.Close()
		stmt.Close()
		val, ok := results[0].(int64)
		if !ok { slog.Warn("Couldn't convert row count to int64"); continue }
		if table == "files" { files_count = val } else { blocklists_count = val }
		slog.Debug(table, "count", val)
	}
	count_estimate := max(files_count, blocklists_count)

	target_cache_size = 128 * count_estimate
	target_cache_size = min(target_cache_size, folderMaxCacheSize)
	// Actual size is in kB
	target_cache_size /= 1000
	if target_cache_size > 2000 {
		// "-size" is used to indicate the cache size in bytes instead of pages
		pragma := fmt.Sprintf("PRAGMA cache_size = -%d", target_cache_size)
		conn.Exec(pragma, nil)
		slog.Info("Folder DB cache tuned", "size", target_cache_size)
	}
}


