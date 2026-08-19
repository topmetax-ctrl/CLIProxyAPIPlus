/*
 * Cursor quota + model-picker overlay (CURSOR-QUOTA-OVERLAY-V1)
 *
 * The upstream management panel classifies quota credentials as
 * antigravity|claude|codex|kimi|xai only, so Cursor auth files vanish from
 * Quota Management before any fetch happens. This overlay calls the Plus
 * backend GET /v0/management/cursor-quota and renders Cursor cards inside the
 * native quota workbench/grid (same chrome as Claude/Antigravity), not as a
 * floating header widget. Quota fetch errors never impersonate credential
 * health.
 *
 * Also ranks model-picker search: a one-letter query like "g" is treated as
 * a family prefix, not a substring of "high"/"thinking".
 */
(function () {
  if (window.__cursorQuotaOverlayV1) return;
  window.__cursorQuotaOverlayV1 = true;

  var MGMT_MARKER = "/v0/management";
  var ROOT_ID = "cursor-quota-root";
  var TAB_ID = "cursor-quota-tab";
  var HINT_ID = "cursor-model-search-hint";
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
  function headers() {
    var h = { Accept: "application/json" };
    if (captured.token) h.Authorization = "Bearer " + captured.token;
    return h;
  }
  function el(tag, props, children) {
    var e = document.createElement(tag);
    if (props) for (var k in props) {
      if (k === "style") e.setAttribute("style", props[k]);
      else if (k === "class") e.className = props[k];
      else e[k] = props[k];
    }
    (children || []).forEach(function (c) {
      if (c == null) return;
      e.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    });
    return e;
  }
  function onQuotaPage() {
    var path = String(location.pathname || "") + String(location.hash || "");
    if (/\/quota(\/|$|\?|#)/.test(path) || path.indexOf("#/quota") !== -1) return true;
    var headings = document.querySelectorAll("h1,h2");
    for (var i = 0; i < headings.length; i++) {
      var t = (headings[i].textContent || "").trim();
      if (t === "Quota Management" || t === "配额管理" || t === "配額管理") return true;
    }
    return false;
  }
  function dollars(cents) {
    if (cents == null || cents === "") return "—";
    return "$" + (Number(cents) / 100).toFixed(2);
  }
  function pct(n) {
    if (n == null || !isFinite(n)) return "—";
    return Math.round(Number(n)) + "%";
  }
  function qsClass(prefix) {
    return document.querySelector('[class*="' + prefix + '"]');
  }
  var hashedCache = {};
  function hashed(prefix) {
    if (Object.prototype.hasOwnProperty.call(hashedCache, prefix)) return hashedCache[prefix];
    var found = "";
    var escaped = prefix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    var re = new RegExp("(?:^|\\s|\\.)(" + escaped + "_{3}[A-Za-z0-9-]+)");
    try {
      for (var i = 0; i < document.styleSheets.length && !found; i++) {
        var rules;
        try { rules = document.styleSheets[i].cssRules; } catch (e) { continue; }
        if (!rules) continue;
        for (var j = 0; j < rules.length; j++) {
          var sel = rules[j].selectorText || "";
          var m = sel.match(re);
          if (m) { found = m[1]; break; }
        }
      }
    } catch (e) {}
    hashedCache[prefix] = found;
    return found;
  }
  function cx() {
    var out = [];
    for (var i = 0; i < arguments.length; i++) if (arguments[i]) out.push(arguments[i]);
    return out.join(" ");
  }

  var cursorTabActive = false;
  function nativeTabFilter() {
    if (cursorTabActive) return "cursor";
    var group = document.querySelector('[class*="QuotaPage-module__tabsRow"] [role="group"]');
    if (!group) return "all";
    var btns = group.querySelectorAll("button");
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].id === TAB_ID) continue;
      if (btns[i].getAttribute("aria-pressed") !== "true") continue;
      var label = (btns[i].textContent || "").replace(/\s+/g, " ").trim().toLowerCase();
      if (label.indexOf("all") === 0) return "all";
      return "other";
    }
    return "all";
  }
  function shouldShowCursorCards() {
    var f = nativeTabFilter();
    return f === "all" || f === "cursor";
  }

  function hideNativeEmpty() {
    var page = qsClass("QuotaPage-module__page") || document;
    var nodes = page.querySelectorAll ? page.querySelectorAll(".empty-state") : [];
    for (var i = 0; i < nodes.length; i++) {
      if (shouldShowCursorCards() && lastRows && lastRows.length) nodes[i].style.display = "none";
      else nodes[i].style.display = "";
    }
  }

  function patchHeaderMeta(total, loadedCount) {
    if (!shouldShowCursorCards() || !total) return;
    var metaTotal = qsClass("QuotaHeader-module__metaTotal");
    var metaLoaded = qsClass("QuotaHeader-module__metaLoaded");
    if (metaTotal && /0\s+credentials/.test(metaTotal.textContent || "")) {
      metaTotal.textContent = total + (total === 1 ? " credential" : " credentials");
    }
    if (metaLoaded && /0\s+loaded/.test(metaLoaded.textContent || "") && loadedCount) {
      metaLoaded.textContent = loadedCount + " loaded";
    }
  }

  function tabActiveClass() {
    return hashed("ProviderTabs-module__tabActive");
  }
  function syncTabChrome() {
    var group = document.querySelector('[class*="QuotaPage-module__tabsRow"] [role="group"]');
    if (!group) return;
    var activeCls = tabActiveClass();
    var btns = group.querySelectorAll("button");
    for (var i = 0; i < btns.length; i++) {
      var isOurs = btns[i].id === TAB_ID;
      if (isOurs) {
        btns[i].setAttribute("aria-pressed", cursorTabActive ? "true" : "false");
        if (activeCls) btns[i].classList.toggle(activeCls, !!cursorTabActive);
      } else if (cursorTabActive) {
        btns[i].setAttribute("aria-pressed", "false");
        if (activeCls) btns[i].classList.remove(activeCls);
      }
    }
  }
  function injectCursorTab(count) {
    var row = qsClass("QuotaPage-module__tabsRow");
    if (!row) return;
    var group = row.querySelector('[role="group"]');
    if (!group) return;
    var existing = document.getElementById(TAB_ID);
    if (existing) {
      var countEl = existing.querySelector('[class*="tabCount"]');
      if (countEl) countEl.textContent = String(count);
      syncTabChrome();
      return;
    }
    var sample = group.querySelector("button");
    if (!sample) return;
    var tab = sample.cloneNode(true);
    tab.id = TAB_ID;
    tab.type = "button";
    tab.setAttribute("aria-pressed", "false");
    var activeCls = tabActiveClass();
    if (activeCls) tab.classList.remove(activeCls);
    var label = tab.querySelector('[class*="tabLabel"]');
    var countNode = tab.querySelector('[class*="tabCount"]');
    var fallback = tab.querySelector('[class*="tabIconFallback"]');
    var img = tab.querySelector("img");
    var glyph = tab.querySelector('[class*="tabGlyph"]');
    if (label) label.textContent = "Cursor";
    if (countNode) countNode.textContent = String(count);
    if (img) img.remove();
    if (glyph && glyph.parentElement) {
      var wrapCls = hashed("ProviderTabs-module__tabIconWrap") || "";
      var fbCls = hashed("ProviderTabs-module__tabIconFallback") || "";
      var wrap = el("span", { class: wrapCls }, [el("span", { class: fbCls || wrapCls }, ["C"])]);
      glyph.parentElement.replaceChild(wrap, glyph);
    } else if (fallback) fallback.textContent = "C";
    if (!label) {
      tab.textContent = "";
      tab.appendChild(document.createTextNode("Cursor (" + count + ")"));
    }
    tab.addEventListener("click", function (ev) {
      ev.preventDefault();
      ev.stopPropagation();
      cursorTabActive = true;
      paint();
    });
    group.appendChild(tab);
    syncTabChrome();
  }

  document.addEventListener("click", function (ev) {
    var btn = ev.target && ev.target.closest && ev.target.closest('[class*="QuotaPage-module__tabsRow"] button');
    if (!btn || btn.id === TAB_ID) return;
    cursorTabActive = false;
    var ours = document.getElementById(TAB_ID);
    if (ours) ours.setAttribute("aria-pressed", "false");
  }, true);

  function quotaRow(label, value, usedPercent) {
    var rowCls = hashed("QuotaBody-module__quotaRow") || "";
    var headCls = hashed("QuotaBody-module__quotaRowHeader") || "";
    var modelCls = hashed("QuotaBody-module__quotaModel") || "";
    var metaCls = hashed("QuotaBody-module__quotaMeta") || "";
    var pctCls = hashed("QuotaBody-module__quotaPercent") || "";
    var barCls = hashed("QuotaBody-module__quotaBar") || "";
    var fillCls = hashed("QuotaBody-module__quotaBarFill") || "";
    var used = Math.max(0, Math.min(100, Number(usedPercent) || 0));
    var tone = used >= 90 ? hashed("QuotaBody-module__quotaBarFillHigh")
      : used >= 70 ? hashed("QuotaBody-module__quotaBarFillMedium")
      : hashed("QuotaBody-module__quotaBarFillLow");
    var fill = el("div", { class: cx(fillCls, tone), style: fillCls ? ("width:" + used + "%") : ("height:100%;width:" + used + "%;background:" + (used >= 90 ? "#c65746" : used >= 70 ? "#c48a3a" : "#3d8b6e")) });
    var bar = el("div", { class: barCls, style: barCls ? "" : "height:8px;border-radius:99px;background:var(--border-color,#e3e1db);overflow:hidden;" }, [fill]);
    var header = el("div", { class: headCls, style: headCls ? "" : "display:flex;justify-content:space-between;align-items:baseline;gap:8px;" }, [
      el("span", { class: modelCls }, [label]),
      el("div", { class: metaCls }, [
        el("span", { class: pctCls }, [value]),
      ]),
    ]);
    return el("div", { class: rowCls, style: rowCls ? "" : "display:flex;flex-direction:column;gap:6px;" }, [header, usedPercent == null ? null : bar]);
  }

  function renderCard(file, payload, onRefresh) {
    var q = (payload && payload.quota) || {};
    var title = file.label || file.name || "cursor";
    var plan = q.plan || "Cursor";
    var status = q.status || "unavailable";
    var health = (payload && payload.credential_health) || "healthy";
    var cardCls = hashed("QuotaCard-module__card") || "";
    var enterCls = hashed("QuotaCard-module__cardEnter") || "";
    var headCls = hashed("QuotaCard-module__head") || "";
    var iconWrapCls = hashed("QuotaCard-module__iconWrap") || "";
    var iconFbCls = hashed("QuotaCard-module__iconFallback") || "";
    var fileCls = hashed("QuotaCard-module__fileName") || "";
    var bodyCls = hashed("QuotaCard-module__body") || "";
    var errCls = hashed("QuotaCard-module__errorStrip") || "";
    var actionRowCls = hashed("QuotaCard-module__actionRow") || "";
    var pillCls = hashed("QuotaCard-module__actionPill") || "";
    var planCls = hashed("QuotaBody-module__codexPlan") || "";
    var planLabelCls = hashed("QuotaBody-module__codexPlanLabel") || "";
    var planValueCls = hashed("QuotaBody-module__codexPlanValue") || "";
    var msgCls = hashed("QuotaBody-module__quotaMessage") || "";
    var warnCls = hashed("QuotaBody-module__quotaWarningMessage") || "";
    var resetCls = hashed("QuotaBody-module__quotaReset") || "";

    var wrap = el("article", {
      class: cx("cursor-quota-card", cardCls, enterCls),
      style: cardCls ? "" : "border:1px solid var(--border-color,#e3e1db);border-radius:14px;padding:14px 16px;background:color-mix(in srgb, var(--bg-primary) 82%, transparent);display:flex;flex-direction:column;gap:10px;",
    });
    wrap.appendChild(el("header", { class: headCls, style: headCls ? "" : "display:flex;align-items:center;gap:9px;" }, [
      el("span", { class: iconWrapCls, title: "Cursor", style: iconWrapCls ? "" : "width:26px;height:26px;border-radius:8px;display:flex;align-items:center;justify-content:center;background:var(--bg-tertiary,#e9e6df);" }, [
        el("span", { class: iconFbCls }, ["C"]),
      ]),
      el("span", { class: fileCls, title: title }, [title]),
    ]));

    var body = el("div", { class: bodyCls, style: bodyCls ? "" : "display:flex;flex-direction:column;gap:9px;" });
    body.appendChild(el("div", { class: planCls, style: planCls ? "" : "display:flex;gap:8px;font-size:12px;" }, [
      el("span", { class: planLabelCls, style: "color:var(--text-secondary,#6d6760);" }, ["Plan"]),
      el("span", { class: planValueCls, style: "color:var(--text-primary,#2d2a26);font-weight:650;" }, [plan + (q.planPrice ? " · " + q.planPrice : "")]),
    ]));
    var healthLine = "credential " + health + " / quota " + status + (q.stale ? " · stale" : "");
    body.appendChild(el("div", { class: msgCls, style: "font-size:11.5px;color:var(--text-tertiary,#a29c95);" }, [healthLine]));

    if (q.error && status !== "ok") {
      body.appendChild(el("div", { class: errCls, role: "alert" }, [
        (q.error.code || "quota_fetch_error") + ": " + (q.error.message || "unavailable"),
      ]));
    }
    if (q.displayMessage) {
      var warn = /limit|exceed|over/i.test(q.displayMessage);
      body.appendChild(el("div", { class: warn ? warnCls : msgCls }, [q.displayMessage]));
    }
    if (q.usage) body.appendChild(quotaRow("Total usage", pct(q.usage.usedPercent) + " used", q.usage.usedPercent));
    if (q.auto) body.appendChild(quotaRow("Auto", pct(q.auto.usedPercent), q.auto.usedPercent));
    if (q.api) body.appendChild(quotaRow("API / manual", pct(q.api.usedPercent), q.api.usedPercent));
    if (q.spend) {
      body.appendChild(quotaRow(
        "Included spend",
        dollars(q.spend.usedCents) + " / " + dollars(q.spend.limitCents),
        q.spend.limitCents ? (q.spend.usedCents / q.spend.limitCents) * 100 : 0
      ));
    }
    if (q.credits) body.appendChild(quotaRow("Credits left", dollars(q.credits.remainingCents)));
    if (q.onDemand && (q.onDemand.limitCents || q.onDemand.usedCents)) {
      body.appendChild(quotaRow("On-demand", dollars(q.onDemand.usedCents) + (q.onDemand.limitCents ? " / " + dollars(q.onDemand.limitCents) : "")));
    }
    if (q.requests && q.requests.length) {
      q.requests.slice(0, 6).forEach(function (b) {
        body.appendChild(quotaRow(b.model, b.used + " / " + b.limit, b.limit ? (b.used / b.limit) * 100 : 0));
      });
    }
    var period = [];
    if (q.periodStart) period.push("start " + String(q.periodStart).replace("T", " ").slice(0, 16));
    if (q.periodEnd) period.push("resets " + String(q.periodEnd).replace("T", " ").slice(0, 16));
    if (period.length) {
      body.appendChild(el("div", { class: resetCls, style: "font-size:11px;color:var(--text-tertiary,#a29c95);" }, [
        period.join(" · ") + " · " + (q.source || "cursor-dashboard"),
      ]));
    }
    wrap.appendChild(body);
    wrap.appendChild(el("footer", { class: actionRowCls, style: actionRowCls ? "" : "display:flex;justify-content:flex-end;" }, [
      el("button", {
        type: "button",
        class: pillCls,
        style: pillCls ? "" : "cursor:pointer;border:1px solid var(--border-color);border-radius:999px;padding:4px 10px;background:transparent;color:var(--text-secondary);font-size:12px;",
        onclick: onRefresh,
      }, ["Refresh"]),
    ]));
    return wrap;
  }

  var loading = false;
  var loaded = false;
  var lastRows = null;
  var lastCursorCount = 0;

  function ensureRoot() {
    var existing = document.getElementById(ROOT_ID);
    if (existing && existing.isConnected) return existing;

    var nativeGrid = qsClass("QuotaPage-module__grid");
    var workbench = qsClass("QuotaPage-module__workbench");
    var tabs = qsClass("QuotaPage-module__tabsRow");
    var header = qsClass("QuotaHeader-module__header");
    var gridCls = hashed("QuotaPage-module__grid");

    var root = el("div", { id: ROOT_ID, class: "cursor-quota-root" });
    if (nativeGrid && shouldShowCursorCards()) {
      nativeGrid.insertBefore(root, nativeGrid.firstChild);
      root.style.display = "contents";
      return root;
    }

    if (gridCls) root.className = cx("cursor-quota-root", gridCls);
    else root.setAttribute("style", "display:grid;grid-template-columns:repeat(auto-fill,minmax(min(100%,340px),1fr));align-items:stretch;gap:16px;");

    if (tabs && tabs.parentElement) {
      if (tabs.nextSibling) tabs.parentElement.insertBefore(root, tabs.nextSibling);
      else tabs.parentElement.appendChild(root);
    } else if (workbench) {
      workbench.appendChild(root);
    } else if (header && header.parentElement) {
      if (header.nextSibling) header.parentElement.insertBefore(root, header.nextSibling);
      else header.parentElement.appendChild(root);
    } else if (document.body) {
      document.body.appendChild(root);
    }
    return root;
  }

  function paint() {
    injectCursorTab(lastCursorCount);
    var show = shouldShowCursorCards() && lastRows && lastRows.length;
    hideNativeEmpty();
    var stale = document.getElementById(ROOT_ID);
    if (!show) {
      if (stale) stale.remove();
      return;
    }
    if (stale && stale.parentElement && stale.parentElement.matches && stale.parentElement.matches('[class*="QuotaHeader-module__"]')) {
      stale.remove();
      stale = null;
    }
    var root = ensureRoot();
    if (!root) return;
    root.innerHTML = "";
    var loadedOk = 0;
    lastRows.forEach(function (row) {
      if (row.ok && row.payload && row.payload.quota && row.payload.quota.status === "ok") loadedOk += 1;
      root.appendChild(renderCard(row.file, row.payload, function () {
        loading = false;
        loaded = false;
        loadQuota(true);
      }));
    });
    patchHeaderMeta(lastCursorCount, loadedOk || lastCursorCount);
  }

  function loadQuota(force) {
    if (!captured.token || loading) return;
    if (loaded && !force) {
      var root = document.getElementById(ROOT_ID);
      var misplaced = root && root.closest && root.closest('[class*="QuotaHeader-module__"]');
      var want = shouldShowCursorCards() && lastRows && lastRows.length;
      if (misplaced) root.remove();
      if ((want && (!root || misplaced || !root.isConnected)) || (!want && root && root.isConnected)) paint();
      else {
        injectCursorTab(lastCursorCount);
        hideNativeEmpty();
        if (want) patchHeaderMeta(lastCursorCount, lastRows.length);
      }
      return;
    }
    loading = true;
    fetch(base() + "/auth-files", { headers: headers() })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        var files = (data && data.files) || [];
        var cursorFiles = files.filter(function (f) {
          var p = String(f.provider || f.type || "").toLowerCase();
          return p === "cursor";
        });
        lastCursorCount = cursorFiles.length;
        if (!cursorFiles.length) {
          lastRows = [];
          loaded = true;
          paint();
          return Promise.resolve();
        }
        return Promise.all(cursorFiles.map(function (file) {
          var idx = encodeURIComponent(file.auth_index || file.authIndex || "");
          var url = base() + "/cursor-quota?auth_index=" + idx + (force ? "&refresh=1" : "");
          return fetch(url, { headers: headers() }).then(function (r) {
            return r.json().then(function (d) { return { file: file, payload: d, ok: r.ok }; });
          });
        })).then(function (rows) {
          lastRows = rows;
          loaded = true;
          paint();
        });
      })
      .catch(function () {})
      .then(function () { loading = false; });
  }

  function modelFamily(id) {
    var s = String(id || "").toLowerCase();
    if (s.indexOf("grok") !== -1) return "Grok";
    if (s.indexOf("claude") !== -1) return "Claude";
    if (s.indexOf("gemini") !== -1) return "Gemini";
    if (s.indexOf("gpt") !== -1 || s.indexOf("codex") !== -1) return "GPT";
    if (s.indexOf("composer") !== -1) return "Composer";
    if (s.indexOf("cursor") === 0) return "Cursor";
    return "Other";
  }
  function modelProvider(id) {
    var s = String(id || "").toLowerCase();
    if (s.indexOf("cursor-") === 0) return "Cursor";
    if (s.indexOf("grok-") === 0) return "xAI";
    return "";
  }
  function rankScore(id, q) {
    var s = String(id || "").toLowerCase();
    q = String(q || "").toLowerCase().trim();
    if (!q) return 0;
    if (s === q) return 1000;
    if (s.indexOf(q) === 0) return 800;
    var fam = modelFamily(s).toLowerCase();
    if (fam.indexOf(q) === 0) return 700;
    var prov = modelProvider(s).toLowerCase();
    if (prov && prov.indexOf(q) === 0) return 600;
    if (q.length >= 3 && s.indexOf(q) !== -1) return 200;
    if (q.length < 3) return -1;
    return 0;
  }

  function decorateModelsDialog() {
    var dialogs = document.querySelectorAll("div");
    var dialog = null;
    for (var i = 0; i < dialogs.length; i++) {
      var t = dialogs[i].textContent || "";
      if (t.indexOf("Models") !== -1 && t.indexOf(" of ") !== -1 && t.indexOf("models") !== -1) {
        var input = dialogs[i].querySelector("input");
        if (input) { dialog = dialogs[i]; break; }
      }
    }
    if (!dialog) return;
    var input = dialog.querySelector("input");
    if (!input) return;
    var q = (input.value || "").trim();
    var rows = [];
    var buttons = dialog.querySelectorAll("button");
    for (var b = 0; b < buttons.length; b++) {
      if ((buttons[b].textContent || "").trim() !== "Copy") continue;
      var row = buttons[b].parentElement;
      if (!row) continue;
      var id = "";
      var texts = row.childNodes;
      for (var n = 0; n < texts.length; n++) {
        if (texts[n].nodeType === 3) continue;
        var label = (texts[n].textContent || "").trim();
        if (label && label !== "Copy" && label !== "Test" && label !== "Route") { id = label.split("\n")[0]; break; }
      }
      if (!id) continue;
      rows.push({ row: row, id: id, family: modelFamily(id), provider: modelProvider(id) || "Cursor" });
      if (!row.querySelector(".cursor-model-badge")) {
        var badge = el("span", {
          class: "cursor-model-badge",
          style: "margin-left:8px;padding:1px 6px;border-radius:999px;background:#eceae4;color:#6d6760;font-size:10px;font-weight:650;vertical-align:middle;",
        }, [modelFamily(id) + " · " + (modelProvider(id) || "Cursor")]);
        var titleNode = row.firstElementChild;
        if (titleNode) titleNode.appendChild(badge);
        else row.insertBefore(badge, buttons[b]);
      }
    }
    var hint = document.getElementById(HINT_ID);
    if (!hint) {
      hint = el("div", { id: HINT_ID, style: "font-size:12px;color:#6d6760;margin:6px 0 10px;" });
      if (input.parentElement) input.parentElement.appendChild(hint);
    }
    if (!q) {
      hint.textContent = "Search ranks exact ID, then prefix, then family (Grok / Claude / GPT). One-letter matches like \"g\" ignore substrings inside high/thinking.";
      return;
    }
    var scored = rows.map(function (r) { return { r: r, s: rankScore(r.id, q) }; }).filter(function (x) { return x.s >= 0; });
    scored.sort(function (a, b) { return b.s - a.s; });
    var families = {};
    scored.forEach(function (x) { families[x.r.family] = (families[x.r.family] || 0) + 1; });
    var famTxt = Object.keys(families).map(function (k) { return k + " (" + families[k] + ")"; }).join(", ");
    hint.textContent = scored.length + " ranked match" + (scored.length === 1 ? "" : "es") + (famTxt ? " · " + famTxt : "") + ". Prefix/family hits stay above contains.";
    var parent = scored[0] && scored[0].r.row.parentElement;
    if (!parent) return;
    scored.forEach(function (x) { parent.appendChild(x.r.row); });
  }

  var debounce;
  function tick() {
    var float = document.getElementById("cursor-ov-card");
    if (onQuotaPage()) {
      if (float) float.style.display = "none";
      loadQuota(false);
    } else {
      if (float) float.style.display = "";
      var root = document.getElementById(ROOT_ID);
      if (root) root.remove();
      var tab = document.getElementById(TAB_ID);
      if (tab) tab.remove();
      loaded = false;
    }
    decorateModelsDialog();
  }
  function schedule() { clearTimeout(debounce); debounce = setTimeout(tick, 250); }
  function start() {
    tick();
    var mo = new MutationObserver(schedule);
    mo.observe(document.body, { childList: true, subtree: true });
    document.addEventListener("input", schedule, true);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
