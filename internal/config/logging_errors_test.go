package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
)

type unformattedStorageError struct{ cause error }

func (unformattedStorageError) Error() string   { panic("error text must not be read") }
func (e unformattedStorageError) Unwrap() error { return e.cause }

func TestSQLiteErrorCodesAndPrivacy(t *testing.T) {
	// Obtain an actual driver error with its private message populated; the
	// sqlite3.Error message field is deliberately not exported by the driver.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const canary = "private_sql_column_canary"
	_, err = db.Exec("SELECT " + canary)
	var original sqlite3.Error
	if !errors.As(err, &original) || !strings.Contains(original.Error(), canary) {
		t.Fatal("fixture did not produce a typed SQLite error containing the canary")
	}
	for _, tc := range []struct {
		code sqlite3.ErrNo
		want string
	}{
		{sqlite3.ErrBusy, "storage_busy"}, {sqlite3.ErrLocked, "storage_busy"},
		{sqlite3.ErrConstraint, "storage_constraint"},
		{sqlite3.ErrCorrupt, "storage_corrupt"}, {sqlite3.ErrNotADB, "storage_corrupt"},
		{sqlite3.ErrReadonly, "storage_read_only"}, {sqlite3.ErrCantOpen, "storage_open_failed"},
		{sqlite3.ErrIoErr, "storage_io"}, {sqlite3.ErrFull, "storage_io"},
		{sqlite3.ErrError, "storage_error"}, {sqlite3.ErrNo(255), "storage_error"},
	} {
		storage := original
		storage.Code = tc.code
		storage.ExtendedCode = tc.code.Extend(1)
		for _, wrapped := range []error{
			storage, &storage, fmt.Errorf("private wrapper: %w", storage),
			unformattedStorageError{&storage},
			errors.Join(context.Canceled, unformattedStorageError{storage}),
			errors.Join(unformattedStorageError{&storage}, context.Canceled),
			errors.Join(context.DeadlineExceeded, storage),
		} {
			if got := ErrorCode(wrapped); got != tc.want {
				t.Fatalf("code %d: got %q, want %q", tc.code, got, tc.want)
			}
			for _, level := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
				logger := NewLogger(LevelDebug)
				record := captureLog(t, logger, func() { logger.log(level, "storage.failed", "storage operation failed", ErrorField(wrapped)) })
				if record["error_code"] != tc.want {
					t.Fatalf("missing typed storage code: %v", record)
				}
				encoded, _ := json.Marshal(record)
				for _, forbidden := range []string{canary, "private wrapper", "SELECT", "no such column"} {
					if strings.Contains(string(encoded), forbidden) {
						t.Fatal("SQLite error text escaped logging boundary")
					}
				}
			}
		}
	}
	if ErrorCode(context.Canceled) != "canceled" {
		t.Fatal("cancellation without storage error changed")
	}
}
