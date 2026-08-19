package management

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestImportCursorAPIKeyRequiresKey ensures the synchronous import rejects an
// empty api_key before making any network call.
func TestImportCursorAPIKeyRequiresKey(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auths")
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.POST("/cursor-api-key", handler.ImportCursorAPIKey)

	cases := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "empty api_key", body: `{"api_key":"   ","label":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/cursor-api-key", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "api_key is required") {
				t.Fatalf("unexpected body: %s", rec.Body.String())
			}
		})
	}
}
