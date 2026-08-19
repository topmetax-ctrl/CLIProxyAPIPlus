package executor

import (
	"errors"
	"fmt"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
)

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var se interface{ StatusCode() int }
	if !errors.As(err, &se) {
		t.Fatalf("error %v does not carry a status code", err)
	}
	return se.StatusCode()
}

func TestClassifyCursorErrorConnectCodes(t *testing.T) {
	cases := map[string]int{
		"invalid_argument":    400,
		"failed_precondition": 400,
		"out_of_range":        400,
		"already_exists":      400,
		"resource_exhausted":  429,
		"unauthenticated":     401,
		"permission_denied":   403,
		"unavailable":         503,
		"internal":            500,
		"deadline_exceeded":   504,
		"not_found":           404,
		"unimplemented":       501,
		"some_future_code":    502,
	}
	for code, want := range cases {
		err := classifyCursorError(&cursorproto.ConnectError{Code: code, Message: "x"})
		if got := statusOf(t, err); got != want {
			t.Fatalf("code %q mapped to %d, want %d", code, got, want)
		}
	}
}

func TestClassifyCursorErrorLocalSessionPassthrough(t *testing.T) {
	err := cursorLocalError(localMixedGeneration, 409, errMixedToolResultGenerations)
	if got := classifyCursorError(err); got != err {
		t.Fatalf("classifyCursorError remapped local error to %v", got)
	}
	if got := classifyCursorError(fmt.Errorf("wrap: %w", err)); !isCursorLocalSessionError(got) {
		t.Fatalf("classifyCursorError dropped wrapped local error: %v", got)
	}
}

func TestClassifyCursorErrorNilPassthrough(t *testing.T) {
	if err := classifyCursorError(nil); err != nil {
		t.Fatalf("classifyCursorError(nil) = %v, want nil", err)
	}
}

func TestIsTransientCursorAuthErr(t *testing.T) {
	unauthenticated := classifyCursorError(&cursorproto.ConnectError{Code: "unauthenticated", Message: "Error"})
	if !isTransientCursorAuthErr(unauthenticated) {
		t.Fatalf("classified unauthenticated error should be retried once, got false for %v", unauthenticated)
	}
	permissionDenied := classifyCursorError(&cursorproto.ConnectError{Code: "permission_denied", Message: "Error"})
	if isTransientCursorAuthErr(permissionDenied) {
		t.Fatalf("permission_denied (403) must not trigger the auth retry")
	}
	// A 401 that is not an upstream unauthenticated rejection (e.g. bad local
	// API key) must not be retried.
	if isTransientCursorAuthErr(cursorStatusErr{code: 401, msg: "invalid api key"}) {
		t.Fatalf("non-unauthenticated 401 must not trigger the auth retry")
	}
	if isTransientCursorAuthErr(nil) {
		t.Fatalf("nil error must not trigger the auth retry")
	}
}
