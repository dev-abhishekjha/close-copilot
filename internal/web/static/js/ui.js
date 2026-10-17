"use strict";
/* Behaviour for the shared UI components (UI-02). Vanilla, no inline
   handlers: everything is delegated from the document, so it works if the
   script loads late or content is swapped in by htmx. */
(function () {
  var TOAST_MS = 6000;

  function closest(target, selector) {
    return target && target.closest ? target.closest(selector) : null;
  }

  function byId(id) {
    return id ? document.getElementById(id) : null;
  }

  document.addEventListener("click", function (ev) {
    var target = ev.target;

    var opener = closest(target, "[data-dialog-open]");
    if (opener) {
      var dlg = byId(opener.getAttribute("data-dialog-open"));
      if (dlg && typeof dlg.showModal === "function" && !dlg.open) {
        ev.preventDefault();
        dlg.showModal();
      }
      return;
    }

    var closer = closest(target, "[data-dialog-close]");
    if (closer) {
      var parent = closest(closer, "dialog");
      if (parent && typeof parent.close === "function") {
        parent.close();
      }
      return;
    }

    var dismiss = closest(target, "[data-toast-dismiss]");
    if (dismiss) {
      var toast = closest(dismiss, "[data-toast]");
      if (toast) {
        toast.remove();
      }
      return;
    }

    // A click on the backdrop lands on the <dialog> itself; its content sits
    // inside the form, so a click on the content never has the dialog as target.
    if (target && target.nodeName === "DIALOG" && target.open) {
      target.close();
    }
  });

  function arm(toast) {
    if (!toast || toast.getAttribute("data-armed") === "true") {
      return;
    }
    toast.setAttribute("data-armed", "true");
    if (toast.getAttribute("data-kind") === "error") {
      return;
    }
    window.setTimeout(function () {
      toast.remove();
    }, TOAST_MS);
  }

  function scan(root) {
    if (!root || !root.querySelectorAll) {
      return;
    }
    if (root.matches && root.matches("[data-toast]")) {
      arm(root);
    }
    var toasts = root.querySelectorAll("[data-toast]");
    for (var i = 0; i < toasts.length; i++) {
      arm(toasts[i]);
    }
  }

  /* ---- Announcements (UI-13) ---- */
  function announce(message) {
    var region = byId("ui-announce");
    if (!region) {
      return;
    }
    region.textContent = "";
    window.requestAnimationFrame(function () {
      region.textContent = String(message);
    });
  }

  document.addEventListener("cc:announce", function (ev) {
    var d = ev.detail;
    if (d && d.message !== undefined && d.message !== null) {
      announce(d.message);
    }
  });

  function fire(name, detail) {
    document.dispatchEvent(new CustomEvent(name, { detail: detail }));
  }

  /* ---- Toasts (UI-13) ---- */
  var MAX_TOASTS = 4;
  var MAX_MESSAGE = 300;
  var TOAST_KINDS = { info: "info", success: "success", error: "error" };
  var TOAST_ICONS = { info: "info", success: "check", error: "alert" };
  // The SVG namespace is an identifier, not a URL we load; it is split so no
  // scheme literal appears in this file.
  var SVG_NS = "http:" + "//www.w3.org/2000/svg";

  function addToast(kind, message) {
    var region = byId("ui-toasts");
    if (!region) {
      return;
    }
    kind = Object.prototype.hasOwnProperty.call(TOAST_KINDS, kind) ? kind : "info";
    var text = String(message === undefined || message === null ? "" : message).slice(0, MAX_MESSAGE);

    var toast = document.createElement("div");
    toast.className = "ui-toast ui-toast-" + kind;
    toast.setAttribute("data-toast", "");
    toast.setAttribute("data-kind", kind);

    var svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", "ui-icon");
    svg.setAttribute("aria-hidden", "true");
    var use = document.createElementNS(SVG_NS, "use");
    use.setAttribute("href", "/static/icons.svg#icon-" + TOAST_ICONS[kind]);
    svg.appendChild(use);
    toast.appendChild(svg);

    var msg = document.createElement("span");
    msg.className = "ui-toast-message";
    msg.textContent = text;
    toast.appendChild(msg);

    var btn = document.createElement("button");
    btn.setAttribute("type", "button");
    btn.className = "ui-toast-close";
    btn.setAttribute("data-toast-dismiss", "");
    btn.setAttribute("aria-label", "Dismiss");
    btn.textContent = "×";
    toast.appendChild(btn);

    region.appendChild(toast);
    while (region.querySelectorAll("[data-toast]").length > MAX_TOASTS) {
      region.querySelector("[data-toast]").remove();
    }
  }

  document.addEventListener("cc:toast", function (ev) {
    var d = ev.detail || {};
    addToast(d.kind, d.message);
  });

  function statusOf(ev) {
    var xhr = ev.detail && ev.detail.xhr;
    var n = xhr ? Number(xhr.status) : 0;
    return isFinite(n) ? Math.trunc(n) : 0;
  }

  document.addEventListener("htmx:responseError", function (ev) {
    var msg = "That request failed (HTTP " + statusOf(ev) + "). Try again.";
    fire("cc:toast", { kind: "error", message: msg });
    fire("cc:announce", { message: msg });
  });

  function unreachable() {
    var msg = "Could not reach the server. Check your connection.";
    fire("cc:toast", { kind: "error", message: msg });
    fire("cc:announce", { message: msg });
  }
  document.addEventListener("htmx:sendError", unreachable);
  document.addEventListener("htmx:timeout", unreachable);

  document.addEventListener("htmx:afterSwap", function (ev) {
    var target = ev.target;
    if (!target || !target.querySelector) {
      return;
    }
    var holder = target.matches && target.matches("[data-announce]") ? target : target.querySelector("[data-announce]");
    if (holder) {
      announce(holder.getAttribute("data-announce"));
    }
  });

  /* ---- Command palette (UI-13) ---- */
  var announceTimer = 0;

  function paletteItems() {
    return Array.prototype.slice.call(document.querySelectorAll("#ui-palette-list [data-palette-item]"));
  }

  function visibleItems() {
    return paletteItems().filter(function (a) {
      return !a.hidden;
    });
  }

  function setActive(item) {
    var input = byId("ui-palette-input");
    var all = paletteItems();
    for (var i = 0; i < all.length; i++) {
      all[i].removeAttribute("aria-selected");
    }
    if (!input) {
      return;
    }
    if (!item) {
      input.removeAttribute("aria-activedescendant");
      return;
    }
    item.setAttribute("aria-selected", "true");
    input.setAttribute("aria-activedescendant", item.id);
    if (typeof item.scrollIntoView === "function") {
      item.scrollIntoView({ block: "nearest" });
    }
  }

  function matches(item, query) {
    var label = item.querySelector(".ui-palette-label");
    var path = (item.getAttribute("href") || "").replace(/[\/_-]+/g, " ");
    var hay = [label ? label.textContent : "", item.getAttribute("data-group") || "", path].join(" ").toLowerCase();
    return hay.indexOf(query) !== -1;
  }

  function filterPalette() {
    var input = byId("ui-palette-input");
    var query = input ? input.value.trim().toLowerCase() : "";
    var items = paletteItems();
    var shown = 0;
    for (var i = 0; i < items.length; i++) {
      var ok = query === "" || matches(items[i], query);
      items[i].parentNode.hidden = !ok;
      items[i].hidden = !ok;
      if (ok) {
        shown++;
      }
    }
    var empty = byId("ui-palette-empty");
    if (empty) {
      empty.hidden = shown !== 0;
    }
    setActive(visibleItems()[0] || null);
    window.clearTimeout(announceTimer);
    announceTimer = window.setTimeout(function () {
      fire("cc:announce", { message: shown === 0 ? "No results" : shown + " results" });
    }, 400);
  }

  function resetPalette() {
    var input = byId("ui-palette-input");
    if (!input) {
      return;
    }
    input.value = "";
    var items = paletteItems();
    for (var i = 0; i < items.length; i++) {
      items[i].hidden = false;
      items[i].parentNode.hidden = false;
    }
    var empty = byId("ui-palette-empty");
    if (empty) {
      empty.hidden = true;
    }
    setActive(visibleItems()[0] || null);
    input.focus();
  }

  function movePalette(step) {
    var items = visibleItems();
    if (items.length === 0) {
      return;
    }
    var at = -1;
    for (var i = 0; i < items.length; i++) {
      if (items[i].getAttribute("aria-selected") === "true") {
        at = i;
      }
    }
    setActive(items[(at + step + items.length) % items.length]);
  }

  function togglePalette() {
    var dlg = byId("ui-palette");
    if (!dlg || typeof dlg.showModal !== "function") {
      return;
    }
    if (dlg.open) {
      dlg.close();
    } else {
      dlg.showModal();
    }
  }

  function isEditable(el) {
    if (!el || !el.closest) {
      return false;
    }
    return el.closest("input, textarea, select, [contenteditable]") !== null;
  }

  function dialogOpen() {
    return document.querySelector("dialog[open]") !== null;
  }

  var chordTimer = 0;
  var chordArmed = false;

  function disarm() {
    chordArmed = false;
    window.clearTimeout(chordTimer);
  }

  function goTo(item) {
    var href = item ? item.getAttribute("href") : null;
    if (href) {
      window.location.assign(href);
    }
  }

  document.addEventListener("input", function (ev) {
    if (ev.target && ev.target.id === "ui-palette-input") {
      filterPalette();
    }
  });

  document.addEventListener("keydown", function (ev) {
    if (ev.isComposing || ev.repeat || typeof ev.key !== "string") {
      return;
    }
    var key = ev.key;

    if ((ev.ctrlKey || ev.metaKey) && !ev.shiftKey && !ev.altKey && key.toLowerCase() === "k") {
      ev.preventDefault();
      togglePalette();
      return;
    }

    var palette = byId("ui-palette");
    if (palette && palette.open && ev.target && ev.target.id === "ui-palette-input") {
      if (key === "ArrowDown") {
        ev.preventDefault();
        movePalette(1);
      } else if (key === "ArrowUp") {
        ev.preventDefault();
        movePalette(-1);
      } else if (key === "Enter") {
        ev.preventDefault();
        var active = document.querySelector('#ui-palette-list [data-palette-item][aria-selected="true"]');
        goTo(active);
      }
      return;
    }

    if (ev.ctrlKey || ev.metaKey || ev.altKey || isEditable(ev.target) || dialogOpen()) {
      disarm();
      return;
    }

    if (key === "/") {
      ev.preventDefault();
      togglePalette();
      return;
    }
    if (key === "?") {
      var help = byId("ui-shortcuts");
      if (help && typeof help.showModal === "function") {
        ev.preventDefault();
        help.showModal();
      }
      return;
    }
    if (chordArmed) {
      disarm();
      var seq = "g " + key.toLowerCase();
      var items = paletteItems();
      for (var i = 0; i < items.length; i++) {
        if (items[i].getAttribute("data-shortcut") === seq) {
          ev.preventDefault();
          goTo(items[i]);
          return;
        }
      }
      return;
    }
    if (key === "g") {
      chordArmed = true;
      chordTimer = window.setTimeout(disarm, 1500);
    }
  });

  function watchPalette() {
    var dlg = byId("ui-palette");
    if (!dlg || typeof MutationObserver !== "function") {
      return;
    }
    new MutationObserver(function () {
      if (dlg.open) {
        resetPalette();
      }
    }).observe(dlg, { attributes: true, attributeFilter: ["open"] });
  }

  function start() {
    scan(document);
    watchPalette();
    if (typeof MutationObserver !== "function" || !document.body) {
      return;
    }
    new MutationObserver(function (records) {
      for (var i = 0; i < records.length; i++) {
        var added = records[i].addedNodes;
        for (var j = 0; j < added.length; j++) {
          if (added[j].nodeType === 1) {
            scan(added[j]);
          }
        }
      }
    }).observe(document.body, { childList: true, subtree: true });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
