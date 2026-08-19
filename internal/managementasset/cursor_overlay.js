/*
 * Cursor API-key import overlay (CURSOR-OVERLAY-V1)
 *
 * Canonical copy. Injected in memory by ApplyCursorPanelPatch when serving
 * /management.html so panel auto-updates cannot drop the form.
 *
 * The management panel now ships a native "Cursor OAuth" login card, but it has
 * no way to import an account from a Cursor API key (crsr_...). This overlay
 * adds ONLY that missing piece: a compact "Import API key" block appended
 * inside the existing native Cursor card (never a duplicate login card, never
 * nested into another provider). If the native Cursor card is not present, a
 * small floating fallback widget is shown instead.
 *
 * It reuses the Authorization: Bearer <management-key> header the panel already
 * sends (captured via XHR/fetch patch) to call:
 *   POST /v0/management/cursor-api-key   { api_key, label }
 *
 * Idempotent across injection and React re-renders.
 */
(function () {
  if (window.__cursorOverlayV1) return;
  window.__cursorOverlayV1 = true;

  var MGMT_MARKER = "/v0/management";
  var CURSOR_BTN = /Start\s+Cursor\s+Login/i;
  var CURSOR_TITLE = "Cursor OAuth";
  var IMPORT_ID = "cursor-apikey-import";
  var captured = window.__cursorMgmtCapture || { token: "", base: "" };
  window.__cursorMgmtCapture = captured;

  function rememberFromUrl(url) {
    if (!url || typeof url !== "string") return;
    var idx = url.indexOf(MGMT_MARKER);
    if (idx === -1) return;
    var prefix = url.substring(0, idx + MGMT_MARKER.length);
    try { captured.base = new URL(prefix, window.location.href).href; }
    catch (e) { captured.base = window.location.origin + MGMT_MARKER; }
  }
  function rememberAuth(value) {
    if (!value || typeof value !== "string") return;
    var m = /bearer\s+(.+)/i.exec(value);
    captured.token = m ? m[1].trim() : value.trim();
  }
  if (!window.__cursorMgmtHooked) {
    window.__cursorMgmtHooked = true;
    var origOpen = XMLHttpRequest.prototype.open;
    XMLHttpRequest.prototype.open = function (method, url) { rememberFromUrl(url); return origOpen.apply(this, arguments); };
    var origSet = XMLHttpRequest.prototype.setRequestHeader;
    XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
      if (name && String(name).toLowerCase() === "authorization") rememberAuth(value);
      return origSet.apply(this, arguments);
    };
    if (window.fetch) {
      var origFetch = window.fetch;
      window.fetch = function (input, init) {
        try {
          var url = typeof input === "string" ? input : input && input.url;
          rememberFromUrl(url);
          var h = init && init.headers;
          if (h) {
            if (typeof h.get === "function") rememberAuth(h.get("authorization"));
            else if (h.Authorization || h.authorization) rememberAuth(h.Authorization || h.authorization);
          }
        } catch (e) {}
        return origFetch.apply(this, arguments);
      };
    }
  }

  function base() { return captured.base || window.location.origin + MGMT_MARKER; }
  function headers(extra) {
    var h = { Accept: "application/json" };
    if (captured.token) h.Authorization = "Bearer " + captured.token;
    if (extra) for (var k in extra) h[k] = extra[k];
    return h;
  }

  function el(tag, props, children) {
    var e = document.createElement(tag);
    if (props) for (var k in props) {
      if (k === "style") e.setAttribute("style", props[k]);
      else if (k === "class") e.className = props[k];
      else e[k] = props[k];
    }
    (children || []).forEach(function (c) { e.appendChild(typeof c === "string" ? document.createTextNode(c) : c); });
    return e;
  }

  // Build the "Import API key" controls. dark=true for the floating fallback.
  function buildImportBlock(dark) {
    var field = "display:block;width:100%;box-sizing:border-box;margin:6px 0;padding:8px 10px;border-radius:8px;font-size:13px;border:1px solid " +
      (dark ? "#33405a;background:#0f1626;color:#e6ebf2;" : "#d6dbe4;background:#fff;color:#1a2130;");
    var primary = "margin-top:2px;padding:8px 16px;border:0;border-radius:8px;background:#2b6cff;color:#fff;font-weight:600;cursor:pointer;font-size:13px;";
    var caption = "font-size:12px;font-weight:700;color:" + (dark ? "#c7d0dd" : "#5b6675") + ";margin-bottom:4px;";

    var keyLabel = el("input", { placeholder: "label (optional, for multi-account)", style: field });
    var key = el("input", { type: "password", placeholder: "crsr_... (Cursor API key)", style: field });
    var btn = el("button", { style: primary }, ["Import API Key"]);
    var status = el("div", { style: "margin-top:8px;font-size:12px;line-height:1.4;color:#8b93a1;min-height:16px;" }, []);

    function setStatus(msg, kind) {
      status.textContent = msg || "";
      status.style.color = kind === "err" ? "#e5484d" : kind === "ok" ? "#2ba25f" : "#8b93a1";
    }
    btn.onclick = function () {
      if (!captured.token) { setStatus("No management session yet. Open Dashboard once, then retry.", "err"); return; }
      var k = (key.value || "").trim();
      var lbl = (keyLabel.value || "").trim();
      if (!k) { setStatus("Enter a Cursor API key (crsr_...) first.", "err"); return; }
      setStatus("Importing API key...");
      fetch(base() + "/cursor-api-key", {
        method: "POST",
        headers: headers({ "Content-Type": "application/json" }),
        body: JSON.stringify({ api_key: k, label: lbl }),
      })
        .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
        .then(function (res) {
          if (res.ok && res.d && res.d.status === "ok") {
            setStatus("Imported! Saved " + (res.d.saved_path || ""), "ok");
            key.value = "";
          } else {
            setStatus("Import failed: " + ((res.d && res.d.error) || "unknown"), "err");
          }
        })
        .catch(function (e) { setStatus("Import request failed: " + e, "err"); });
    };

    var block = el("div", {
      id: IMPORT_ID,
      style: "width:100%;flex-basis:100%;margin-top:14px;padding-top:12px;border-top:1px solid " + (dark ? "#2a3448" : "#e3e7ee") + ";",
    }, [
      el("div", { style: caption }, ["Or import a Cursor API key (crsr_...)"]),
      keyLabel, key, btn, status,
    ]);
    return block;
  }

  // ----- attach import block inside the native Cursor OAuth card -----
  function findCursorCard() {
    var buttons = document.getElementsByTagName("button");
    for (var i = 0; i < buttons.length; i++) {
      if (CURSOR_BTN.test(buttons[i].textContent || "")) {
        var node = buttons[i];
        for (var j = 0; j < 8 && node.parentElement; j++) {
          node = node.parentElement;
          if ((node.textContent || "").indexOf(CURSOR_TITLE) !== -1) return node;
        }
      }
    }
    return null;
  }
  function ensureImportUI() {
    if (document.getElementById(IMPORT_ID)) return true;
    var card = findCursorCard();
    if (!card) return false;
    card.appendChild(buildImportBlock(false));
    return true;
  }

  // ----- floating fallback (only if no native Cursor card) -----
  var floatingBuilt = false;
  function buildFloating() {
    if (floatingBuilt || document.getElementById("cursor-ov-card")) return;
    floatingBuilt = true;
    var body = el("div", { style: "display:none;padding:12px;" }, [buildImportBlock(true)]);
    var header = el("div", {
      style: "display:flex;align-items:center;justify-content:space-between;padding:10px 12px;cursor:pointer;background:#141c2e;border-radius:12px 12px 0 0;",
    }, [
      el("span", { style: "font-weight:700;font-size:13px;color:#e6ebf2;" }, ["Cursor API key"]),
      el("span", { style: "font-size:11px;color:#7a869a;" }, ["click to toggle"]),
    ]);
    header.onclick = function () { body.style.display = body.style.display === "none" ? "block" : "none"; };
    var card = el("div", {
      id: "cursor-ov-card",
      style: "position:fixed;right:16px;bottom:16px;width:280px;z-index:2147483000;background:#0b1120;border:1px solid #263149;border-radius:12px;box-shadow:0 10px 30px rgba(0,0,0,.45);font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;",
    }, [header, body]);
    document.body.appendChild(card);
  }
  function onQuotaPage() {
    return String(location.hash || "").indexOf("quota") !== -1;
  }
  function syncFloating() {
    var card = document.getElementById("cursor-ov-card");
    if (!card) return;
    card.style.display = onQuotaPage() ? "none" : "";
  }

  // ----- bootstrap -----
  var debounce;
  function tick() { ensureImportUI(); syncFloating(); }
  function schedule() { clearTimeout(debounce); debounce = setTimeout(tick, 200); }
  function start() {
    tick();
    var mo = new MutationObserver(schedule);
    mo.observe(document.body, { childList: true, subtree: true });
    setTimeout(function () {
      if (onQuotaPage()) return;
      if (!document.getElementById(IMPORT_ID)) buildFloating();
    }, 6000);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
