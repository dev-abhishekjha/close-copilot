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

  function start() {
    scan(document);
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
