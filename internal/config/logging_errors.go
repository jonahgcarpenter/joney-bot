package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"

	"github.com/mattn/go-sqlite3"
)

// HTTPStatus returns a wrapped provider's HTTPStatusCode, or zero when absent or
// outside 100..599. The structural interface avoids provider dependency cycles.
func HTTPStatus(err error) int {
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		if code := status.HTTPStatusCode(); code >= 100 && code <= 599 {
			return code
		}
	}
	return 0
}

// ErrorCode returns a fixed, bounded classification, never arbitrary error text.
// Cancellation alone is not a failure: callers should emit status=ok,
// outcome=canceled. A joined SQLite failure takes precedence over cancellation.
// Unknown errors deliberately lose their details. SafeErrorText is only for user
// responses and must not be used as log content.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	// Inspect only numeric driver codes. SQLite messages can contain SQL, paths,
	// and user data. Handle both the driver's value form and wrapped pointers.
	var storage sqlite3.Error
	var storagePtr *sqlite3.Error
	hasStorage := errors.As(err, &storage)
	if !hasStorage && errors.As(err, &storagePtr) && storagePtr != nil {
		storage, hasStorage = *storagePtr, true
	}
	if hasStorage {
		switch storage.Code {
		case sqlite3.ErrBusy, sqlite3.ErrLocked:
			return "storage_busy"
		case sqlite3.ErrConstraint:
			return "storage_constraint"
		case sqlite3.ErrCorrupt, sqlite3.ErrNotADB:
			return "storage_corrupt"
		case sqlite3.ErrReadonly:
			return "storage_read_only"
		case sqlite3.ErrCantOpen:
			return "storage_open_failed"
		case sqlite3.ErrIoErr, sqlite3.ErrFull:
			return "storage_io"
		default:
			return "storage_error"
		}
	}
	for _, entry := range []struct {
		err  error
		code string
	}{
		{context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline_exceeded"},
		{sql.ErrNoRows, "sql_no_rows"}, {sql.ErrTxDone, "sql_tx_done"},
		{sql.ErrConnDone, "sql_conn_done"}, {fs.ErrNotExist, "file_not_found"},
		{fs.ErrPermission, "permission_denied"}, {fs.ErrExist, "file_exists"},
		{fs.ErrClosed, "file_closed"}, {fs.ErrInvalid, "invalid_argument"},
		{io.ErrUnexpectedEOF, "unexpected_eof"}, {io.EOF, "eof"},
		{io.ErrClosedPipe, "closed_pipe"}, {net.ErrClosed, "network_closed"},
	} {
		if errors.Is(err, entry.err) {
			return entry.code
		}
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "json_syntax"
	}
	var unmarshal *json.UnmarshalTypeError
	if errors.As(err, &unmarshal) {
		return "json_type"
	}
	var invalid *json.InvalidUnmarshalError
	if errors.As(err, &invalid) {
		return "json_invalid_target"
	}
	if status := HTTPStatus(err); status != 0 {
		switch {
		case status == 429:
			return "http_rate_limited"
		case status >= 500:
			return "http_server_error"
		case status >= 400:
			return "http_client_error"
		default:
			return "http_error"
		}
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network_timeout"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "network_error"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "url_error"
	}
	if network != nil {
		return "network_error"
	}
	var path *fs.PathError
	if errors.As(err, &path) {
		return "filesystem_error"
	}
	var link *os.LinkError
	if errors.As(err, &link) {
		return "filesystem_error"
	}
	var syscall *os.SyscallError
	if errors.As(err, &syscall) {
		return "system_error"
	}
	return "unknown_error"
}
