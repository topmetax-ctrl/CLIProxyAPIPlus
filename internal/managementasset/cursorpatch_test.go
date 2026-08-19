package managementasset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// miniPanel mimics the structure of the minified upstream panel asset: three
// locale tables carrying the Codex OAuth strings and the builtin provider tile
// array ending with the xai entry (whose icon identifiers are minified names
// that change on every build).
const miniPanel = "var zh={auth_login:{codex_oauth_title:`Codex OAuth`,codex_oauth_button:`开始 Codex 登录`}};" +
	"var tw={auth_login:{codex_oauth_title:`Codex OAuth`,codex_oauth_button:`開始 Codex 登入`}};" +
	"var en={auth_login:{codex_oauth_title:`Codex OAuth`,codex_oauth_button:`Start Codex Login`}};" +
	"const tiles=[{kind:`builtin`,id:`codex`,titleKey:`auth_login.codex_oauth_title`,icon:aB}" +
	",{kind:`builtin`,id:`xai`,titleKey:`auth_login.xai_oauth_title`,icon:{light:pS,dark:mS}}];rest()"

func TestApplyCursorPanelPatchInjectsTileAndI18n(t *testing.T) {
	patched := ApplyCursorPanelPatch(miniPanel)

	if got := strings.Count(patched, "cursor_oauth_title:`Cursor OAuth`"); got != 3 {
		t.Fatalf("expected cursor i18n injected into 3 locales, found %d", got)
	}
	if !strings.Contains(patched, "cursor_oauth_button:`开始 Cursor 登录`") ||
		!strings.Contains(patched, "cursor_oauth_button:`開始 Cursor 登入`") ||
		!strings.Contains(patched, "cursor_oauth_button:`Start Cursor Login`") {
		t.Fatalf("locale-specific cursor strings missing:\n%s", patched)
	}

	tileIdx := strings.Index(patched, "id:`cursor`,titleKey:`auth_login.cursor_oauth_title`")
	if tileIdx < 0 {
		t.Fatalf("cursor tile not injected:\n%s", patched)
	}
	xaiIdx := strings.Index(patched, "id:`xai`")
	arrayEnd := strings.Index(patched, "];rest()")
	if !(xaiIdx < tileIdx && tileIdx < arrayEnd) {
		t.Fatalf("cursor tile must sit after the xai entry and inside the tile array (xai=%d cursor=%d end=%d)", xaiIdx, tileIdx, arrayEnd)
	}
	if !strings.Contains(patched, "icon:{light:`data:image/svg+xml") {
		t.Fatalf("cursor tile must carry self-contained icons, not minified identifiers")
	}
}

func TestApplyCursorPanelPatchIsIdempotent(t *testing.T) {
	once := ApplyCursorPanelPatch(miniPanel)
	twice := ApplyCursorPanelPatch(once)
	if once != twice {
		t.Fatalf("patch must be a no-op on already patched content")
	}
}

func TestApplyCursorPanelPatchFallsBackWhenLocaleAnchorsDrift(t *testing.T) {
	// Upstream reworded the Codex button labels: exact locale anchors miss,
	// but the key name survives, so English strings are injected everywhere.
	drifted := strings.ReplaceAll(miniPanel, "codex_oauth_button:`开始 Codex 登录`", "codex_oauth_button:`登录 Codex`")
	drifted = strings.ReplaceAll(drifted, "codex_oauth_button:`開始 Codex 登入`", "codex_oauth_button:`登入 Codex`")
	drifted = strings.ReplaceAll(drifted, "codex_oauth_button:`Start Codex Login`", "codex_oauth_button:`Codex Sign-in`")

	patched := ApplyCursorPanelPatch(drifted)
	if got := strings.Count(patched, "cursor_oauth_title:`Cursor OAuth`"); got != 3 {
		t.Fatalf("fallback should inject English strings before each codex_oauth_title, found %d", got)
	}
	if !strings.Contains(patched, "id:`cursor`") {
		t.Fatalf("tile injection should still work when only i18n anchors drift")
	}
}

func TestApplyCursorPanelPatchLeavesUnknownLayoutUntouched(t *testing.T) {
	unknown := "<html>completely different asset</html>"
	if got := ApplyCursorPanelPatch(unknown); got != unknown {
		t.Fatalf("unknown layout must be returned unchanged, got:\n%s", got)
	}
}

const miniPanelHTML = `<html><body><div id="root"></div></body></html>`

func TestApplyCursorPanelPatchInjectsAPIKeyOverlay(t *testing.T) {
	if !strings.Contains(cursorOverlayJS, cursorOverlayMarker) {
		t.Fatal("embedded overlay must carry CURSOR-OVERLAY-V1")
	}
	patched := ApplyCursorPanelPatch(miniPanelHTML)
	if !strings.Contains(patched, cursorOverlayMarker) {
		t.Fatalf("panel HTML must receive the API-key overlay:\n%s", patched)
	}
	if !strings.Contains(patched, `id="cursor-overlay"`) {
		t.Fatalf("overlay script tag missing:\n%s", patched)
	}
	if !strings.Contains(patched, "Import API Key") {
		t.Fatalf("overlay body missing:\n%s", patched)
	}
	bodyIdx := strings.LastIndex(patched, "</body>")
	scriptIdx := strings.Index(patched, `id="cursor-overlay"`)
	if !(scriptIdx >= 0 && bodyIdx > scriptIdx) {
		t.Fatalf("overlay must sit before </body> (script=%d body=%d)", scriptIdx, bodyIdx)
	}
}

func TestApplyCursorPanelPatchInjectsQuotaOverlay(t *testing.T) {
	if !strings.Contains(cursorQuotaOverlayJS, cursorQuotaOverlayMarker) {
		t.Fatal("embedded quota overlay must carry CURSOR-QUOTA-OVERLAY-V1")
	}
	patched := ApplyCursorPanelPatch(miniPanelHTML)
	if !strings.Contains(patched, cursorQuotaOverlayMarker) {
		t.Fatalf("panel HTML must receive the quota overlay:\n%s", patched)
	}
	if !strings.Contains(patched, `id="cursor-quota-overlay"`) {
		t.Fatalf("quota overlay script tag missing:\n%s", patched)
	}
	if strings.Count(patched, cursorQuotaOverlayMarker) != 1 {
		t.Fatalf("quota overlay injected more than once")
	}
	if !strings.Contains(cursorQuotaOverlayJS, "QuotaPage-module__workbench") ||
		!strings.Contains(cursorQuotaOverlayJS, "QuotaPage-module__grid") ||
		!strings.Contains(cursorQuotaOverlayJS, "empty-state") {
		t.Fatal("quota overlay must mount into the native workbench/grid and hide the empty state")
	}
	if !strings.Contains(cursorQuotaOverlayJS, "QuotaHeader-module__") {
		t.Fatal("quota overlay must refuse to mount inside the quota header")
	}
}

func TestApplyCursorPanelPatchOverlayIsIdempotent(t *testing.T) {
	once := ApplyCursorPanelPatch(miniPanelHTML)
	twice := ApplyCursorPanelPatch(once)
	if once != twice {
		t.Fatalf("overlay patch must be a no-op on already patched HTML")
	}
	if got := strings.Count(once, cursorOverlayMarker); got != 1 {
		t.Fatalf("expected one API-key overlay marker, found %d", got)
	}
	if got := strings.Count(once, cursorQuotaOverlayMarker); got != 1 {
		t.Fatalf("expected one quota overlay marker, found %d", got)
	}
}

func TestApplyCursorPanelPatchOverlaySurvivesExistingOAuthTile(t *testing.T) {
	// A panel that already has the OAuth tile (in-memory or upstream) must
	// still receive the import overlay. The OAuth early-return must not skip it.
	already := strings.ReplaceAll(miniPanelHTML, "<div id=\"root\"></div>", "cursor_oauth_title:`Cursor OAuth`<div id=\"root\"></div>")
	patched := ApplyCursorPanelPatch(already)
	if !strings.Contains(patched, cursorOverlayMarker) {
		t.Fatalf("overlay must inject even when OAuth strings are already present:\n%s", patched)
	}
	if !strings.Contains(patched, cursorQuotaOverlayMarker) {
		t.Fatalf("quota overlay must inject even when OAuth strings are already present:\n%s", patched)
	}
}

func TestCursorPatchedManagementHTMLCachesAndRefreshes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "management.html")
	if err := os.WriteFile(path, []byte(miniPanel), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := CursorPatchedManagementHTML(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "id:`cursor`") {
		t.Fatalf("served asset must be patched")
	}

	// Simulate the auto-updater replacing the asset: mutate content and mtime.
	replaced := strings.ReplaceAll(miniPanel, "rest()", "rest2()")
	if err = os.WriteFile(path, []byte(replaced), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err = os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	second, err := CursorPatchedManagementHTML(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "rest2()") {
		t.Fatalf("cache must refresh when the asset on disk changes")
	}
	if !strings.Contains(string(second), "id:`cursor`") {
		t.Fatalf("refreshed asset must be re-patched")
	}
}
