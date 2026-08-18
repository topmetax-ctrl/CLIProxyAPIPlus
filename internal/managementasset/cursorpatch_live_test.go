package managementasset

import (
	"os"
	"strings"
	"testing"
)

// Opt-in contract check for panel releases: point CURSOR_PATCH_REAL_ASSET at
// a freshly downloaded management.html to verify the patch anchors still
// match before rolling a new panel version. Skipped in normal test runs.
func TestApplyCursorPanelPatchAgainstRealArtifact(t *testing.T) {
	path := os.Getenv("CURSOR_PATCH_REAL_ASSET")
	if path == "" {
		t.Skip("CURSOR_PATCH_REAL_ASSET not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patched := ApplyCursorPanelPatch(string(raw))
	if got := strings.Count(patched, "cursor_oauth_title:`Cursor OAuth`"); got != 3 {
		t.Fatalf("expected 3 locale injections in real asset, got %d", got)
	}
	if !strings.Contains(patched, "id:`cursor`,titleKey:`auth_login.cursor_oauth_title`") {
		t.Fatalf("tile not injected in real asset")
	}
	if !strings.Contains(patched, cursorOverlayMarker) {
		t.Fatalf("API-key overlay not injected in real asset")
	}
	t.Logf("real asset patched: %d -> %d bytes", len(raw), len(patched))
}
