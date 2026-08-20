package executor

import (
	"errors"
	"net/http"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Local Cursor session faults. These are proxy-state problems, not upstream
// account/auth/quota failures, and must never trigger credential cooldown.
type cursorLocalErrorClass string

const (
	localSessionStateMismatch cursorLocalErrorClass = "LOCAL_SESSION_STATE_MISMATCH"
	localToolResultNotFound   cursorLocalErrorClass = "LOCAL_TOOL_RESULT_NOT_FOUND"
	localMixedGeneration      cursorLocalErrorClass = "LOCAL_MIXED_GENERATION"
	localDuplicateResult      cursorLocalErrorClass = "LOCAL_DUPLICATE_RESULT"
	localInFlightResult       cursorLocalErrorClass = "LOCAL_IN_FLIGHT_RESULT"
	localFinalRejectedResult  cursorLocalErrorClass = "LOCAL_FINAL_REJECTED_RESULT"
)

var (
	errMixedToolResultGenerations = errors.New("cursor: MIXED_TOOL_RESULT_GENERATIONS")
	errToolResultAlreadyConsumed  = errors.New("cursor: TOOL_RESULT_ALREADY_CONSUMED")
	errToolResultInFlight         = errors.New("cursor: TOOL_RESULT_IN_FLIGHT")
	errToolResultFinalRejected    = errors.New("cursor: TOOL_RESULT_FINAL_REJECTED")
	errToolResultNotFound         = errors.New("cursor: TOOL_RESULT_NOT_FOUND")
	errToolResultMismatch         = errors.New("cursor: tool results do not match any pending tool call")
	errSessionMissingToolChannel  = errors.New("cursor: session has no toolResultCh (stale session?)")
	errSessionMissingResumeOut    = errors.New("cursor: session has no resumeOutCh")
)

type cursorLocalSessionError struct {
	class  cursorLocalErrorClass
	status int
	err    error
}

func cursorLocalError(class cursorLocalErrorClass, status int, sentinel error) error {
	if sentinel == nil {
		sentinel = errToolResultMismatch
	}
	if status == 0 {
		status = http.StatusBadRequest
	}
	return &cursorLocalSessionError{class: class, status: status, err: sentinel}
}

func (e *cursorLocalSessionError) Error() string {
	if e == nil || e.err == nil {
		return "cursor: local session error"
	}
	return e.err.Error()
}

func (e *cursorLocalSessionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *cursorLocalSessionError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *cursorLocalSessionError) IsRequestScoped() bool { return true }

func isCursorLocalSessionError(err error) bool {
	var local *cursorLocalSessionError
	return errors.As(err, &local)
}

var (
	_ error                               = (*cursorLocalSessionError)(nil)
	_ cliproxyexecutor.StatusError        = (*cursorLocalSessionError)(nil)
	_ cliproxyexecutor.RequestScopedError = (*cursorLocalSessionError)(nil)
)
