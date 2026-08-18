package managementasset

import (
	_ "embed"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// The management panel asset is built by a separate upstream project
// (Cli-Proxy-API-Management-Center) that does not know about the Cursor
// provider, and the on-disk copy is replaced wholesale by the auto-updater.
// Editing the downloaded file directly therefore does not survive updates and
// breaks the updater's local-vs-remote hash comparison. Instead the Cursor
// OAuth login tile, its i18n strings, and the API-key import overlay are
// injected in memory at serve time, keeping the artifact on disk pristine.

//go:embed cursor_overlay.js
var cursorOverlayJS string

const cursorOverlayMarker = "CURSOR-OVERLAY-V1"

const cursorPatchMarker = "cursor_oauth_title"

// cursorPanelTile is a self-contained provider tile (inline SVG icons, no
// references to minified identifiers) appended to the builtin OAuth tile list.
const cursorPanelTile = ",{kind:`builtin`,id:`cursor`,titleKey:`auth_login.cursor_oauth_title`,icon:{light:`data:image/svg+xml,%3Csvg%20height='1em'%20style='flex:none;line-height:1'%20viewBox='0%200%2024%2024'%20width='1em'%20xmlns='http://www.w3.org/2000/svg'%3E%3Ctitle%3ECursor%3C/title%3E%3Cpath%20d='M11.503.131%201.891%205.678a.84.84%200%200%200-.42.726v11.188c0%20.3.162.575.42.724l9.609%205.55a1%201%200%200%200%20.998%200l9.61-5.55a.84.84%200%200%200%20.42-.724V6.404a.84.84%200%200%200-.42-.726L12.497.131a1.01%201.01%200%200%200-.996%200M2.657%206.338h18.55c.263%200%20.43.287.297.515L12.23%2022.918c-.062.107-.229.064-.229-.06V12.335a.59.59%200%200%200-.295-.51l-9.11-5.257c-.109-.063-.064-.23.061-.23'%20fill='%23000000'%3E%3C/path%3E%3C/svg%3E`,dark:`data:image/svg+xml,%3Csvg%20height='1em'%20style='flex:none;line-height:1'%20viewBox='0%200%2024%2024'%20width='1em'%20xmlns='http://www.w3.org/2000/svg'%3E%3Ctitle%3ECursor%3C/title%3E%3Cpath%20d='M11.503.131%201.891%205.678a.84.84%200%200%200-.42.726v11.188c0%20.3.162.575.42.724l9.609%205.55a1%201%200%200%200%20.998%200l9.61-5.55a.84.84%200%200%200%20.42-.724V6.404a.84.84%200%200%200-.42-.726L12.497.131a1.01%201.01%200%200%200-.996%200M2.657%206.338h18.55c.263%200%20.43.287.297.515L12.23%2022.918c-.062.107-.229.064-.229-.06V12.335a.59.59%200%200%200-.295-.51l-9.11-5.257c-.109-.063-.064-.23.061-.23'%20fill='%23ffffff'%3E%3C/path%3E%3C/svg%3E`}}"

// cursorPanelTileAnchor marks the builtin tile the Cursor entry is inserted
// after. It intentionally stops before the icon expression because icon
// identifiers are renamed by the minifier on every panel build.
const cursorPanelTileAnchor = "id:`xai`,titleKey:`auth_login.xai_oauth_title`"

// cursorPanelI18n maps a locale-identifying anchor (the Codex OAuth strings,
// which are literal UI text and stable across panel builds) to the Cursor
// strings inserted immediately before it.
var cursorPanelI18n = []struct{ anchor, insert string }{
	{
		anchor: "codex_oauth_title:`Codex OAuth`,codex_oauth_button:`开始 Codex 登录`",
		insert: "cursor_oauth_title:`Cursor OAuth`,cursor_oauth_button:`开始 Cursor 登录`,cursor_oauth_hint:`通过 OAuth 流程登录 Cursor 服务，自动获取并保存认证文件。`,cursor_oauth_url_label:`授权链接:`,cursor_open_link:`打开链接`,cursor_copy_link:`复制链接`,cursor_oauth_status_waiting:`等待认证中...`,cursor_oauth_status_success:`认证成功！`,cursor_oauth_status_error:`认证失败:`,cursor_oauth_start_error:`启动 Cursor OAuth 失败:`,cursor_oauth_polling_error:`检查认证状态失败:`,",
	},
	{
		anchor: "codex_oauth_title:`Codex OAuth`,codex_oauth_button:`開始 Codex 登入`",
		insert: "cursor_oauth_title:`Cursor OAuth`,cursor_oauth_button:`開始 Cursor 登入`,cursor_oauth_hint:`透過 OAuth 流程登入 Cursor 服務，自動取得並儲存驗證檔案。`,cursor_oauth_url_label:`授權連結:`,cursor_open_link:`開啟連結`,cursor_copy_link:`複製連結`,cursor_oauth_status_waiting:`等待驗證中...`,cursor_oauth_status_success:`驗證成功！`,cursor_oauth_status_error:`驗證失敗:`,cursor_oauth_start_error:`啟動 Cursor OAuth 失敗:`,cursor_oauth_polling_error:`檢查驗證狀態失敗:`,",
	},
	{
		anchor: "codex_oauth_title:`Codex OAuth`,codex_oauth_button:`Start Codex Login`",
		insert: cursorPanelI18nEnglish,
	},
}

const cursorPanelI18nEnglish = "cursor_oauth_title:`Cursor OAuth`,cursor_oauth_button:`Start Cursor Login`,cursor_oauth_hint:`Login to Cursor service through OAuth flow, automatically obtain and save authentication files.`,cursor_oauth_url_label:`Authorization link:`,cursor_open_link:`Open Link`,cursor_copy_link:`Copy Link`,cursor_oauth_status_waiting:`Waiting for authentication...`,cursor_oauth_status_success:`Authentication successful!`,cursor_oauth_status_error:`Authentication failed:`,cursor_oauth_start_error:`Failed to start Cursor OAuth:`,cursor_oauth_polling_error:`Failed to check authentication status:`,"

// ApplyCursorPanelPatch injects the Cursor OAuth tile, i18n strings, and the
// API-key import overlay into a management panel asset. It is idempotent:
// content that already contains a piece is left as-is for that piece. When the
// upstream panel changes enough that an anchor no longer matches, the
// corresponding piece is skipped with a warning instead of corrupting the asset.
func ApplyCursorPanelPatch(content string) string {
	content = applyCursorOAuthTile(content)
	return applyCursorAPIKeyOverlay(content)
}

func applyCursorOAuthTile(content string) string {
	if strings.Contains(content, cursorPatchMarker) {
		return content
	}

	matched := 0
	for _, loc := range cursorPanelI18n {
		if strings.Contains(content, loc.anchor) {
			content = strings.ReplaceAll(content, loc.anchor, loc.insert+loc.anchor)
			matched++
		}
	}
	if matched == 0 {
		// Locale anchors drifted (upstream reworded the Codex strings). Fall
		// back to untranslated English keys so the tile still renders a title.
		if strings.Contains(content, "codex_oauth_title:") {
			content = strings.ReplaceAll(content, "codex_oauth_title:", cursorPanelI18nEnglish+"codex_oauth_title:")
			log.Warn("cursor panel patch: locale anchors not found, injected English i18n fallback")
		} else {
			log.Warn("cursor panel patch: no i18n anchor found in management asset; panel layout changed upstream")
		}
	}

	idx := strings.Index(content, cursorPanelTileAnchor)
	if idx < 0 {
		log.Warn("cursor panel patch: provider tile anchor not found; Cursor login tile not injected")
		return content
	}
	// The tile entry ends before the `]` closing the builtin tile array; the
	// icon expression between anchor and `]` never contains a bracket.
	end := strings.Index(content[idx:], "]")
	if end < 0 {
		log.Warn("cursor panel patch: provider tile array terminator not found; Cursor login tile not injected")
		return content
	}
	insertAt := idx + end
	return content[:insertAt] + cursorPanelTile + content[insertAt:]
}

func applyCursorAPIKeyOverlay(content string) string {
	if strings.Contains(content, cursorOverlayMarker) {
		return content
	}
	if !strings.Contains(content, `id="root"`) && !strings.Contains(content, `id='root'`) {
		return content
	}
	if strings.Contains(strings.ToLower(cursorOverlayJS), "</script") {
		log.Warn("cursor panel patch: overlay script contains a closing script tag; refusing to inject")
		return content
	}
	idx := strings.LastIndex(content, "</body>")
	if idx < 0 {
		idx = strings.LastIndex(content, "</BODY>")
	}
	if idx < 0 {
		log.Warn("cursor panel patch: </body> not found; API-key import overlay not injected")
		return content
	}
	snippet := "\n  <script id=\"cursor-overlay\">\n" + cursorOverlayJS + "\n  </script>\n"
	return content[:idx] + snippet + content[idx:]
}

var cursorPatchedCache struct {
	sync.Mutex
	path    string
	modTime time.Time
	size    int64
	data    []byte
}

// CursorPatchedManagementHTML reads the management panel asset from disk and
// returns it with the Cursor OAuth tile and API-key import overlay applied.
// Patched output is cached and invalidated by file modification time and size,
// so the (auto-updated) asset is re-patched transparently after a new panel
// release is downloaded.
func CursorPatchedManagementHTML(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	cursorPatchedCache.Lock()
	defer cursorPatchedCache.Unlock()
	if cursorPatchedCache.path == path &&
		cursorPatchedCache.modTime.Equal(info.ModTime()) &&
		cursorPatchedCache.size == info.Size() &&
		cursorPatchedCache.data != nil {
		return cursorPatchedCache.data, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	patched := []byte(ApplyCursorPanelPatch(string(raw)))

	cursorPatchedCache.path = path
	cursorPatchedCache.modTime = info.ModTime()
	cursorPatchedCache.size = info.Size()
	cursorPatchedCache.data = patched
	return patched, nil
}
