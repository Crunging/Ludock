// Small progressive enhancements; every page works without them.
"use strict";
(function () {
  function onReady(fn) {
    if (document.readyState !== "loading") fn();
    else document.addEventListener("DOMContentLoaded", fn);
  }

  // Show or hide every password field of a form.
  document.addEventListener("change", function (event) {
    var toggle = event.target.closest && event.target.closest("[data-show-passwords]");
    if (!toggle) return;
    var form = toggle.form || document;
    form.querySelectorAll("input[data-password]").forEach(function (input) {
      input.type = toggle.checked ? "text" : "password";
    });
  });

  // Disable a submit button and show its busy label while the form posts.
  document.addEventListener("submit", function (event) {
    var form = event.target;
    var confirmation = form.querySelector("[name=confirmation][data-password]");
    var password = form.querySelector("[name=password]");
    if (confirmation && password && confirmation.value !== password.value) {
      event.preventDefault();
      confirmation.setAttribute("aria-invalid", "true");
      var error = form.querySelector(".login-card__error");
      if (!error) {
        error = document.createElement("div");
        error.className = "login-card__error";
        error.setAttribute("role", "alert");
        form.querySelector("[type=submit]").before(error);
      }
      error.textContent = "Passwords do not match.";
      confirmation.focus();
      return;
    }
    var button = form.querySelector("button[type=submit][data-busy-label]");
    if (button && !form.hasAttribute("hx-post") && !form.hasAttribute("hx-put")) {
      button.disabled = true;
      button.textContent = button.getAttribute("data-busy-label");
    }
  });

  // Open and close native dialogs: data-dialog-open="id", data-dialog-close.
  document.addEventListener("click", function (event) {
    var opener = event.target.closest && event.target.closest("[data-dialog-open]");
    if (opener) {
      var dialog = document.getElementById(opener.getAttribute("data-dialog-open"));
      if (dialog && dialog.showModal) {
        event.preventDefault();
        dialog.showModal();
      }
      return;
    }
    var closer = event.target.closest && event.target.closest("[data-dialog-close]");
    if (closer) {
      var parent = closer.closest("dialog");
      if (parent) {
        event.preventDefault();
        parent.close();
      }
    }
  });

  // htmx buttons (or a form's submit button) show their busy label and stay
  // disabled while the request runs.
  function busyButton(elt) {
    if (!elt || !elt.matches) return null;
    if (elt.matches("button[data-busy-label]")) return elt;
    return elt.matches("form") ? elt.querySelector("button[type=submit][data-busy-label]") : null;
  }
  document.addEventListener("htmx:beforeRequest", function (event) {
    var button = busyButton(event.detail.elt);
    if (!button || button.hasAttribute("data-idle-label")) return;
    button.setAttribute("data-idle-label", button.textContent);
    button.textContent = button.getAttribute("data-busy-label");
    button.disabled = true;
  });
  // Restore busy buttons, and close a dialog after its htmx form succeeds.
  document.addEventListener("htmx:afterRequest", function (event) {
    var button = busyButton(event.detail.elt);
    if (button && button.hasAttribute("data-idle-label")) {
      button.textContent = button.getAttribute("data-idle-label");
      button.removeAttribute("data-idle-label");
      button.disabled = false;
    }
    var form = event.detail.elt;
    if (event.detail.successful && form && form.matches && form.matches("dialog form[data-close-on-success]")) {
      form.closest("dialog").close();
    }
  });

  // Session expiry or revocation sends the browser to sign in again.
  document.addEventListener("htmx:responseError", function (event) {
    if (event.detail.xhr && event.detail.xhr.status === 401) window.location.assign("/login");
  });

  // data-shown-by="checkbox-id" shows an element while the box is checked;
  // data-hidden-by hides it.
  document.addEventListener("change", function (event) {
    if (!event.target.id) return;
    document.querySelectorAll('[data-shown-by="' + event.target.id + '"]').forEach(function (element) {
      element.hidden = !event.target.checked;
    });
    document.querySelectorAll('[data-hidden-by="' + event.target.id + '"]').forEach(function (element) {
      element.hidden = event.target.checked;
    });
  });

  // data-confirm-value="name" keeps the form's submit button disabled until
  // the field matches.
  function confirmations(root) {
    (root || document).querySelectorAll("input[data-confirm-value]").forEach(function (input) {
      var button = input.form && input.form.querySelector("button[type=submit]");
      if (button) button.disabled = input.value !== input.getAttribute("data-confirm-value");
    });
  }
  document.addEventListener("input", function (event) {
    if (event.target.matches && event.target.matches("input[data-confirm-value]")) confirmations(event.target.form);
  });

  // Copy buttons: data-copy="text".
  document.addEventListener("click", function (event) {
    var button = event.target.closest && event.target.closest("[data-copy]");
    if (!button || !navigator.clipboard) return;
    event.preventDefault();
    navigator.clipboard.writeText(button.getAttribute("data-copy")).then(function () {
      var label = button.getAttribute("data-copied-label") || "Copied";
      var original = button.textContent;
      button.textContent = label;
      setTimeout(function () { button.textContent = original; }, 1500);
    });
  });

  // Local, relative times for <time data-relative>.
  var relative = window.Intl && Intl.RelativeTimeFormat ? new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }) : null;
  var dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
  var dateOnly = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });
  function relativeText(timestamp, now) {
    var elapsed = now - timestamp, distance = Math.abs(elapsed);
    if (!relative) return dateTime.format(timestamp);
    if (distance < 60000) return elapsed >= 0 ? "just now" : "in under a minute";
    if (distance < 3600000) return relative.format(-Math.round(elapsed / 60000), "minute");
    if (distance < 86400000) return relative.format(-Math.round(elapsed / 3600000), "hour");
    if (distance < 7 * 86400000) return relative.format(-Math.round(elapsed / 86400000), "day");
    return dateOnly.format(timestamp);
  }
  function formatTimes(root) {
    var now = Date.now();
    (root || document).querySelectorAll("time[datetime]").forEach(function (element) {
      var timestamp = Date.parse(element.getAttribute("datetime"));
      if (isNaN(timestamp)) return;
      var mode = element.getAttribute("data-relative");
      element.title = dateTime.format(timestamp);
      if (mode === "sentence" || mode === "plain") {
        var text = relativeText(timestamp, now);
        element.textContent = mode === "sentence" ? text.charAt(0).toUpperCase() + text.slice(1) : text;
      } else {
        element.textContent = dateTime.format(timestamp);
      }
    });
  }
  setInterval(function () { formatTimes(); }, 30000);

  // Disclosure menus: [data-action-menu] > button + content.
  document.addEventListener("click", function (event) {
    var trigger = event.target.closest && event.target.closest("[data-action-menu] > button");
    document.querySelectorAll("[data-action-menu]").forEach(function (menu) {
      var button = menu.querySelector(":scope > button");
      var content = menu.querySelector(".action-menu__content");
      var open = trigger && trigger === button ? button.getAttribute("aria-expanded") !== "true" : false;
      if (!trigger && menu.contains(event.target) && !event.target.closest("[data-dialog-open], a")) return;
      button.setAttribute("aria-expanded", open ? "true" : "false");
      content.hidden = !open;
    });
  });
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Escape") return;
    document.querySelectorAll("[data-action-menu] > button[aria-expanded=true]").forEach(function (button) {
      button.setAttribute("aria-expanded", "false");
      button.parentElement.querySelector(".action-menu__content").hidden = true;
      button.focus();
    });
  });

  // Live update connection status and session expiry over SSE.
  function liveStatus(connected) {
    document.querySelectorAll("[data-live-status]").forEach(function (element) { element.hidden = connected; });
  }
  document.addEventListener("htmx:sseOpen", function () { liveStatus(true); });
  document.addEventListener("htmx:sseError", function () { liveStatus(false); });
  document.addEventListener("htmx:sseMessage", function (event) {
    if (event.detail && event.detail.type === "expired") window.location.assign("/login");
  });

  // Long lists show their first items until expanded.
  function collapseLists(root) {
    (root || document).querySelectorAll("[data-collapse-after]").forEach(function (list) {
      if (list.dataset.collapsed) return;
      var limit = parseInt(list.getAttribute("data-collapse-after"), 10);
      var items = list.children;
      if (items.length <= limit) return;
      list.dataset.collapsed = "true";
      for (var index = limit; index < items.length; index++) items[index].hidden = true;
      var button = document.createElement("button");
      button.type = "button";
      button.className = "secondary-btn needs-attention__expand";
      button.setAttribute("aria-expanded", "false");
      button.textContent = "Show all " + items.length + " items";
      button.addEventListener("click", function () {
        var expanded = button.getAttribute("aria-expanded") !== "true";
        for (var index = limit; index < items.length; index++) items[index].hidden = !expanded;
        button.setAttribute("aria-expanded", expanded ? "true" : "false");
        button.textContent = expanded ? "Show fewer items" : "Show all " + items.length + " items";
      });
      list.after(button);
    });
  }

  // htmx:load fires for each element htmx adds, including outerHTML swaps.
  document.addEventListener("htmx:load", function (event) {
    formatTimes(event.detail.elt);
    collapseLists(event.detail.elt);
    confirmations(event.detail.elt);
  });

  onReady(function () {
    formatTimes();
    collapseLists();
    confirmations();
    // Error and success responses both render into their targets.
    if (window.htmx) {
      htmx.config.responseHandling = [
        { code: "204", swap: false },
        { code: "[23]..", swap: true },
        { code: "401", swap: false, error: true },
        { code: "[45]..", swap: true, error: false },
      ];
    }
  });
})();
