package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

var (
	errInvalidCompactionOutput = errors.New("invalid session compaction model output")
	errPermanentProvider       = errors.New("permanent session compaction provider failure")
	errTerminalCompaction      = errors.New("terminal session compaction failure")
	errLowPriorityUnavailable  = errors.New("low-priority model capacity unavailable")
)

type invalidCompactionOutputError struct {
	code string
}

func (e *invalidCompactionOutputError) Error() string {
	return "invalid session compaction model output: " + e.code
}

func (e *invalidCompactionOutputError) Unwrap() error {
	return errInvalidCompactionOutput
}

type permanentProviderError struct {
	statusCode int
}

func (e *permanentProviderError) Error() string {
	return fmt.Sprintf("permanent session compaction provider failure: HTTP %d", e.statusCode)
}

func (e *permanentProviderError) Unwrap() error {
	return errPermanentProvider
}

type terminalCompactionError struct {
	code string
}

func (e *terminalCompactionError) Error() string {
	return "terminal session compaction failure: " + e.code
}

func (e *terminalCompactionError) Unwrap() error {
	return errTerminalCompaction
}

func invalidCompactionOutput(code string) error {
	return &invalidCompactionOutputError{code: code}
}

func terminalCompaction(code string) error {
	return &terminalCompactionError{code: code}
}

func compactionErrorCode(err error) string {
	var invalid *invalidCompactionOutputError
	if errors.As(err, &invalid) {
		return invalid.code
	}
	var terminal *terminalCompactionError
	if errors.As(err, &terminal) {
		return terminal.code
	}
	if errors.Is(err, errPermanentProvider) {
		return "provider_request_rejected"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "transient_timeout"
	}
	var httpErr *llm.ChatHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooEarly:
			return "transient_timeout"
		case http.StatusTooManyRequests:
			return "transient_rate_limit"
		default:
			return "transient_provider"
		}
	}
	return "transient_runtime"
}
