package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var fetchCursorQuota = cursorauth.FetchQuota

// GetCursorQuota returns a canonical Cursor provider-quota snapshot.
//
// Endpoint:
//
//	GET /v0/management/cursor-quota?auth_index=<AUTH_INDEX>
//
// A DashboardService failure is reported as quota.status=unavailable with an
// error code. It does not change credential health: AgentService/Run may still
// succeed when the private usage RPC has drifted.
func (h *Handler) GetCursorQuota(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "cursor") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth is not a Cursor credential"})
		return
	}

	access := metadataString(auth, "access_token")
	refresh := metadataString(auth, "refresh_token")
	skipCache := strings.EqualFold(strings.TrimSpace(c.Query("refresh")), "1") ||
		strings.EqualFold(strings.TrimSpace(c.Query("refresh")), "true")

	snap := fetchCursorQuota(c.Request.Context(), cursorauth.FetchQuotaOptions{
		CacheKey:     auth.ID,
		AccountID:    auth.ID,
		AccessToken:  access,
		RefreshToken: refresh,
		SkipCache:    skipCache,
		Transport:    h.apiCallTransport(auth),
		OnRefreshed: func(accessToken, refreshToken string) {
			h.persistCursorQuotaTokens(c, auth, accessToken, refreshToken)
		},
	})
	if snap.Error != nil {
		log.WithFields(log.Fields{
			"auth_id": auth.ID,
			"code":    snap.Error.Code,
			"stale":   snap.Stale,
		}).Debug("cursor quota fetch unavailable")
	}

	c.JSON(http.StatusOK, gin.H{
		"credential_health": "healthy",
		"auth_index":        auth.Index,
		"name":              strings.TrimSpace(auth.FileName),
		"label":             strings.TrimSpace(auth.Label),
		"quota":             snap,
	})
}

func (h *Handler) persistCursorQuotaTokens(c *gin.Context, auth *coreauth.Auth, accessToken, refreshToken string) {
	if h == nil || h.authManager == nil || auth == nil {
		return
	}
	accessToken = strings.TrimSpace(accessToken)
	refreshToken = strings.TrimSpace(refreshToken)
	if accessToken == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = accessToken
	if refreshToken != "" {
		auth.Metadata["refresh_token"] = refreshToken
	}
	if expiry := cursorauth.GetTokenExpiry(accessToken); !expiry.IsZero() {
		auth.Metadata["expires_at"] = expiry.Format(time.RFC3339)
	}
	now := time.Now()
	auth.LastRefreshedAt = now
	auth.UpdatedAt = now
	ctx := c.Request.Context()
	if _, err := h.authManager.Update(ctx, auth); err != nil {
		log.WithError(err).Debug("cursor quota token persist failed")
	}
}

func metadataString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if v, ok := auth.Metadata[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
