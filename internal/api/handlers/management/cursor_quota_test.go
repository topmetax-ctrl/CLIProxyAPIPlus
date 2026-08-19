package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetCursorQuotaRequiresAuthIndex(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-quota", nil)
	h.GetCursorQuota(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetCursorQuotaRejectsNonCursorProvider(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "claude-1", FileName: "claude.json", Provider: "claude", Metadata: map[string]any{"access_token": "x"}}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-quota?auth_index="+index, nil)
	h.GetCursorQuota(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetCursorQuotaReturnsSnapshotWithoutInvalidatingCredential(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "cursor-1",
		FileName: "cursor.json",
		Provider: "cursor",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "tok", "refresh_token": "ref"},
	}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}

	orig := fetchCursorQuota
	t.Cleanup(func() { fetchCursorQuota = orig })
	fetchCursorQuota = func(context.Context, cursor.FetchQuotaOptions) cursor.QuotaSnapshot {
		return cursor.QuotaSnapshot{
			Provider:  "cursor",
			AccountID: "cursor-1",
			Plan:      "Pro",
			Status:    cursor.QuotaStatusUnavailable,
			Error:     &cursor.QuotaError{Code: cursor.QuotaErrorSchema, Message: "protocol changed"},
			FetchedAt: time.Now().UTC(),
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-quota?auth_index="+index, nil)
	h.GetCursorQuota(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		CredentialHealth string               `json:"credential_health"`
		AuthIndex        string               `json:"auth_index"`
		Quota            cursor.QuotaSnapshot `json:"quota"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CredentialHealth != "healthy" {
		t.Fatalf("credential_health = %q, quota fetch must not mark the credential dead", payload.CredentialHealth)
	}
	if payload.AuthIndex != index {
		t.Fatalf("auth_index = %q", payload.AuthIndex)
	}
	if payload.Quota.Status != cursor.QuotaStatusUnavailable || payload.Quota.Error == nil || payload.Quota.Error.Code != cursor.QuotaErrorSchema {
		t.Fatalf("quota = %+v", payload.Quota)
	}

	updated, ok := manager.GetByID("cursor-1")
	if !ok || updated.Status != coreauth.StatusActive || updated.Unavailable {
		t.Fatalf("credential mutated: status=%q unavailable=%v", updated.Status, updated.Unavailable)
	}
}

func TestGetCursorQuotaPersistsRefreshedTokens(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "cursor-refresh",
		FileName: "cursor-refresh.json",
		Provider: "cursor",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "old", "refresh_token": "ref"},
	}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}

	orig := fetchCursorQuota
	t.Cleanup(func() { fetchCursorQuota = orig })
	fetchCursorQuota = func(_ context.Context, opts cursor.FetchQuotaOptions) cursor.QuotaSnapshot {
		if opts.OnRefreshed != nil {
			opts.OnRefreshed("new-access", "new-refresh")
		}
		return cursor.QuotaSnapshot{Provider: "cursor", Status: cursor.QuotaStatusOK, Plan: "Pro"}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-quota?auth_index="+index, nil)
	h.GetCursorQuota(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	updated, ok := manager.GetByID("cursor-refresh")
	if !ok {
		t.Fatal("auth missing")
	}
	if got, _ := updated.Metadata["access_token"].(string); got != "new-access" {
		t.Fatalf("access_token = %q", got)
	}
	if got, _ := updated.Metadata["refresh_token"].(string); got != "new-refresh" {
		t.Fatalf("refresh_token = %q", got)
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("status = %q", updated.Status)
	}
}

func TestGetCursorQuotaNotFound(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-quota?auth_index=missing", nil)
	h.GetCursorQuota(ctx)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auth not found") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
