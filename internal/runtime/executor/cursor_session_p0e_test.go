package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCursorLocalSessionErrorsAreRequestScoped(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		class  cursorLocalErrorClass
		status int
	}{
		{name: "mixed", err: cursorLocalError(localMixedGeneration, http.StatusConflict, errMixedToolResultGenerations), class: localMixedGeneration, status: http.StatusConflict},
		{name: "duplicate", err: cursorLocalError(localDuplicateResult, http.StatusConflict, errToolResultAlreadyConsumed), class: localDuplicateResult, status: http.StatusConflict},
		{name: "in_flight", err: cursorLocalError(localInFlightResult, http.StatusConflict, errToolResultInFlight), class: localInFlightResult, status: http.StatusConflict},
		{name: "final_rejected", err: cursorLocalError(localFinalRejectedResult, http.StatusBadRequest, errToolResultFinalRejected), class: localFinalRejectedResult, status: http.StatusBadRequest},
		{name: "not_found", err: cursorLocalError(localToolResultNotFound, http.StatusBadRequest, errToolResultNotFound), class: localToolResultNotFound, status: http.StatusBadRequest},
		{name: "mismatch", err: cursorLocalError(localSessionStateMismatch, http.StatusBadRequest, errToolResultMismatch), class: localSessionStateMismatch, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var scoped cliproxyexecutor.RequestScopedError
			if !errors.As(tc.err, &scoped) || scoped == nil || !scoped.IsRequestScoped() {
				t.Fatalf("error %v is not request-scoped", tc.err)
			}
			var local *cursorLocalSessionError
			if !errors.As(tc.err, &local) || local.class != tc.class {
				t.Fatalf("class = %v, want %s", local, tc.class)
			}
			if got := statusOf(t, tc.err); got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
			if classified := classifyCursorError(tc.err); classified != tc.err {
				t.Fatalf("classifyCursorError remapped local error to %v", classified)
			}
		})
	}
}

func TestCursorExecutor_LocalToolResultErrorsDoNotMutateOrCooldown(t *testing.T) {
	cliproxyauth.SetQuotaCooldownDisabled(false)
	cliproxyauth.SetTransientErrorCooldownSeconds(5)
	t.Cleanup(func() { cliproxyauth.SetTransientErrorCooldownSeconds(0) })

	cases := []struct {
		name string
		run  func(t *testing.T, e *CursorExecutor, sessionID string) error
	}{
		{
			name: "duplicate_consumed",
			run: func(t *testing.T, e *CursorExecutor, sessionID string) error {
				idA := normalizeToolCallID("p0e-dup-A")
				conv, key, owner := p0eBind(e, sessionID)
				p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
				if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
					t.Fatalf("first consume failed: %v", err)
				}
				p0cCommitResults(e, sessionID, idA)
				err := p0cSendResults(e, sessionID, "req-A-retry", idA)
				if !errors.Is(err, errToolResultAlreadyConsumed) {
					t.Fatalf("duplicate error = %v", err)
				}
				return err
			},
		},
		{
			name: "in_flight",
			run: func(t *testing.T, e *CursorExecutor, sessionID string) error {
				idA := normalizeToolCallID("p0e-inflight-A")
				conv, key, owner := p0eBind(e, sessionID)
				p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
				if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
					t.Fatalf("first consume failed: %v", err)
				}
				err := p0cSendResults(e, sessionID, "req-A-retry", idA)
				if !errors.Is(err, errToolResultInFlight) {
					t.Fatalf("in-flight error = %v", err)
				}
				return err
			},
		},
		{
			name: "unknown_tool_result",
			run: func(t *testing.T, e *CursorExecutor, sessionID string) error {
				idA := normalizeToolCallID("p0e-unknown-A")
				idX := normalizeToolCallID("p0e-unknown-X")
				conv, key, owner := p0eBind(e, sessionID)
				p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
				err := p0cSendResults(e, sessionID, "req-unknown", idX)
				if !errors.Is(err, errToolResultNotFound) {
					t.Fatalf("unknown error = %v", err)
				}
				if !p0cContains(e.livePendingToolIDs(key), idA) {
					t.Fatal("unknown reject consumed gen-A")
				}
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := "p0e-" + tc.name
			e := NewCursorExecutor(nil)
			err := tc.run(t, e, sessionID)
			if err == nil {
				t.Fatal("expected local session error")
			}
			if !isCursorLocalSessionError(err) {
				t.Fatalf("expected cursor local session error, got %T %v", err, err)
			}
			p0eAssertAccountStaysSelectable(t, err, "cursor-p0e-"+tc.name)
		})
	}
}

func TestCursorExecutor_TranscriptExtrasResumeCurrentGeneration(t *testing.T) {
	idA := normalizeToolCallID("p0e-hist-A")
	idB := normalizeToolCallID("p0e-hist-B")
	sessionID := "p0e-transcript-extras"
	e := NewCursorExecutor(nil)
	conv, key, owner := p0eBind(e, sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
		t.Fatalf("consume gen-A: %v", err)
	}
	p0cCommitResults(e, sessionID, idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)
	payload := p0cClaudeHistoryThenCurrentPayload(sessionID, idA, idB)
	_, err := e.ExecuteStream(
		logging.WithRequestID(context.Background(), "req-B-with-history"),
		cursorTestAuth(),
		cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payload},
		p0bClaudeOpts(payload),
	)
	if err != nil {
		t.Fatalf("transcript history must not 409 MIXED against the current pending generation: %v", err)
	}
	if p0cContains(e.livePendingToolIDs(key), idB) {
		t.Fatal("gen-B should be consumed")
	}
}

func TestCursorLocalSessionErrorsDoNotCooldownAccount(t *testing.T) {
	cliproxyauth.SetQuotaCooldownDisabled(false)
	cliproxyauth.SetTransientErrorCooldownSeconds(5)
	t.Cleanup(func() { cliproxyauth.SetTransientErrorCooldownSeconds(0) })

	localErrors := []error{
		cursorLocalError(localMixedGeneration, http.StatusConflict, errMixedToolResultGenerations),
		cursorLocalError(localDuplicateResult, http.StatusConflict, errToolResultAlreadyConsumed),
		cursorLocalError(localInFlightResult, http.StatusConflict, errToolResultInFlight),
		cursorLocalError(localFinalRejectedResult, http.StatusBadRequest, errToolResultFinalRejected),
		cursorLocalError(localToolResultNotFound, http.StatusBadRequest, errToolResultNotFound),
		cursorLocalError(localSessionStateMismatch, http.StatusBadRequest, errToolResultMismatch),
	}
	for _, fail := range localErrors {
		t.Run(fail.Error(), func(t *testing.T) {
			p0eAssertAccountStaysSelectable(t, fail, "cursor-p0e-mgr-"+strings.ReplaceAll(fail.Error(), " ", "_"))
		})
	}
}

func TestCursorUpstream500StillCooldownsAccount(t *testing.T) {
	cliproxyauth.SetQuotaCooldownDisabled(false)
	cliproxyauth.SetTransientErrorCooldownSeconds(5)
	t.Cleanup(func() { cliproxyauth.SetTransientErrorCooldownSeconds(0) })

	authID := "cursor-p0e-upstream-500"
	model := "cursor-p0e-model"
	exec := &p0eStreamExecutor{id: "cursor", fail: cursorStatusErr{code: http.StatusInternalServerError, msg: "cursor upstream 500"}}
	m := p0eNewCursorManager(t, authID, model, exec)

	_, err := m.ExecuteStream(context.Background(), []string{"cursor"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if err == nil {
		t.Fatal("expected upstream 500")
	}
	updated, ok := m.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	state := updated.ModelStates[model]
	if state == nil || state.NextRetryAfter.IsZero() {
		t.Fatalf("upstream 500 must cooldown the account, got state=%#v", state)
	}

	_, err2 := m.ExecuteStream(context.Background(), []string{"cursor"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if err2 == nil {
		t.Fatal("expected synthetic cooldown/unavailable on the next request")
	}
	if isCursorLocalSessionError(err2) {
		t.Fatalf("second request reached executor after cooldown: %v", err2)
	}
	if exec.calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1 (second request must not run after cooldown)", exec.calls.Load())
	}
}

func p0eAssertAccountStaysSelectable(t *testing.T, fail error, authID string) {
	t.Helper()
	model := "cursor-p0e-model"
	exec := &p0eStreamExecutor{id: "cursor", fail: fail}
	m := p0eNewCursorManager(t, authID, model, exec)

	_, err := m.ExecuteStream(context.Background(), []string{"cursor"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if err == nil {
		t.Fatal("expected local session error")
	}
	if !isCursorLocalSessionError(err) && !errors.Is(err, fail) {
		t.Fatalf("first request error = %v, want local session error", err)
	}

	updated, ok := m.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("missing auth")
	}
	if updated.Unavailable {
		t.Fatal("local session error marked auth unavailable")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("local session error set auth cooldown %v", updated.NextRetryAfter)
	}
	if state := updated.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("local session error set model cooldown %#v", state)
	}

	result, err2 := m.ExecuteStream(context.Background(), []string{"cursor"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if err2 != nil {
		t.Fatalf("unrelated follow-up request error = %v (account should remain selectable)", err2)
	}
	if result == nil {
		t.Fatal("follow-up stream result is nil")
	}
	_ = collectCursorStream(t, result)
	if exec.calls.Load() != 2 {
		t.Fatalf("executor calls = %d, want 2", exec.calls.Load())
	}
}

func p0eNewCursorManager(t *testing.T, authID, model string, exec cliproxyauth.ProviderExecutor) *cliproxyauth.Manager {
	t.Helper()
	m := cliproxyauth.NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	m.RegisterExecutor(exec)
	auth := &cliproxyauth.Auth{ID: authID, Provider: "cursor", Status: cliproxyauth.StatusActive, Metadata: map[string]any{"access_token": "test-token"}}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "cursor", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return m
}

type p0eStreamExecutor struct {
	id    string
	fail  error
	calls atomic.Int32
}

func (e *p0eStreamExecutor) Identifier() string { return e.id }

func (e *p0eStreamExecutor) Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.fail
}

func (e *p0eStreamExecutor) ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	n := e.calls.Add(1)
	if n == 1 && e.fail != nil {
		return nil, e.fail
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *p0eStreamExecutor) Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return nil, nil
}

func (e *p0eStreamExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *p0eStreamExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func p0eBind(e *CursorExecutor, sessionID string) (string, string, *cursorStateOwner) {
	conv := deriveConversationId("", sessionID, "")
	key := "cursor-test:" + conv
	return conv, key, e.beginConversationStream(conv)
}

func TestCursorExecutor_LocalErrorsPropagateRequestIDContext(t *testing.T) {
	idA := normalizeToolCallID("p0e-ctx-A")
	idX := normalizeToolCallID("p0e-ctx-X")
	sessionID := "p0e-request-id"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	payload := p0cClaudeResultsPayload(sessionID, []string{idX})
	_, err := e.ExecuteStream(
		logging.WithRequestID(context.Background(), "req-unknown-consumer"),
		cursorTestAuth(),
		cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payload},
		p0bClaudeOpts(payload),
	)
	if !errors.Is(err, errToolResultNotFound) {
		t.Fatalf("error = %v", err)
	}
	if !isCursorLocalSessionError(err) {
		t.Fatalf("unknown error lost local classification: %T", err)
	}
	if !p0cContains(e.livePendingToolIDs(key), idA) {
		t.Fatal("unknown reject consumed gen-A")
	}
}
