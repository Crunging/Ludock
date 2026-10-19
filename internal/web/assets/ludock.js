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
  // A radio's siblings change too, without their own events.
  // data-shown-when="field=value" shows an element while the form's field
  // has that value.
  function toggles(input) {
    var inputs = input.type === "radio" && input.form ? input.form.querySelectorAll('input[type=radio][name="' + input.name + '"]') : [input];
    inputs.forEach(function (box) {
      if (!box.id) return;
      document.querySelectorAll('[data-shown-by="' + box.id + '"]').forEach(function (element) { element.hidden = !box.checked; });
      document.querySelectorAll('[data-hidden-by="' + box.id + '"]').forEach(function (element) { element.hidden = box.checked; });
    });
    if (!input.form || !input.name) return;
    input.form.querySelectorAll("[data-shown-when]").forEach(function (element) {
      var rule = element.getAttribute("data-shown-when").split("=");
      if (rule[0] !== input.name) return;
      var field = input.form.elements[rule[0]];
      element.hidden = !field || field.value !== rule[1];
    });
  }
  document.addEventListener("change", function (event) {
    if (event.target.matches && event.target.matches("input, select")) toggles(event.target);
  });

  // Schedule forms: day presets and the browser's time zone.
  var localTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  document.addEventListener("click", function (event) {
    var preset = event.target.closest && event.target.closest("button[data-days]");
    if (preset && preset.form) {
      var days = preset.getAttribute("data-days").split(", ");
      preset.form.querySelectorAll("input[name=days]").forEach(function (box) { box.checked = days.indexOf(box.value) >= 0; });
      preset.form.querySelectorAll("button[data-days]").forEach(function (other) { other.setAttribute("aria-pressed", String(other === preset)); });
      preset.form.dispatchEvent(new Event("change", { bubbles: true }));
      return;
    }
    var zone = event.target.closest && event.target.closest("button[data-local-timezone]");
    if (zone && zone.form) {
      zone.form.elements[zone.getAttribute("data-local-timezone")].value = localTimezone;
      zone.form.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });
  function timezones(root) {
    (root || document).querySelectorAll("input[data-default-local-timezone]").forEach(function (input) {
      if (input.value) return;
      input.value = localTimezone;
      if (input.form) input.form.dispatchEvent(new Event("change", { bubbles: true }));
    });
    (root || document).querySelectorAll("button[data-local-timezone]").forEach(function (button) {
      button.textContent = "Use my time zone (" + localTimezone + ")";
    });
    (root || document).querySelectorAll("datalist[data-timezones]").forEach(function (list) {
      if (list.options.length || !Intl.supportedValuesOf) return;
      var zones = Intl.supportedValuesOf("timeZone");
      if (zones.indexOf("UTC") < 0) zones.push("UTC");
      zones.sort().forEach(function (zone) {
        var option = document.createElement("option");
        option.value = zone;
        list.appendChild(option);
      });
    });
  }

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

  // History dates: a datetime-local field (local time) edits the hidden
  // Unix-millisecond field it names. An unchanged field keeps the exact
  // instant, which matters when a local time repeats as daylight saving ends.
  function localInput(ms) {
    var date = new Date(ms);
    var local = new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, -1);
    return local.replace(/\.000$/, "").replace(/:00$/, "");
  }
  function msFields(root) {
    (root || document).querySelectorAll("input[data-ms-field]").forEach(function (input) {
      var hidden = input.form && input.form.elements[input.getAttribute("data-ms-field")];
      var ms = hidden && Number(hidden.value);
      if (hidden && hidden.value && isFinite(ms)) {
        input.value = localInput(ms);
        input.step = input.value.indexOf(".") >= 0 ? "0.001" : input.value.length > 16 ? "1" : "60";
      }
    });
  }
  document.addEventListener("change", function (event) {
    var input = event.target;
    if (!input.matches || !input.matches("input[data-ms-field]") || !input.form) return;
    var hidden = input.form.elements[input.getAttribute("data-ms-field")];
    var ms = new Date(input.value).getTime();
    hidden.value = input.value && isFinite(ms) ? String(ms) : "";
  });
  document.addEventListener("submit", function (event) {
    var form = event.target;
    var from = form.elements && form.elements.from, to = form.elements && form.elements.to;
    if (!from || !to || !from.value || !to.value || Number(from.value) <= Number(to.value)) return;
    event.preventDefault();
    var error = form.querySelector("[data-range-error]");
    if (!error) {
      error = document.createElement("p");
      error.className = "alert alert--error";
      error.setAttribute("role", "alert");
      error.setAttribute("data-range-error", "");
      form.querySelector(".inline-actions").before(error);
    }
    error.textContent = "From must be before or equal to To.";
  }, true);

  // GET forms with data-compact-query leave empty fields out of the URL.
  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (event.defaultPrevented || !form.matches || !form.matches("form[data-compact-query]")) return;
    Array.prototype.forEach.call(form.elements, function (field) {
      if (field.name && !field.value) {
        field.disabled = true;
        setTimeout(function () { field.disabled = false; }, 0);
      }
    });
  });

  // Ludock logs: follow the newest entry, or keep the entry being read in
  // place while the buffer refreshes.
  var logAnchor = null;
  document.addEventListener("htmx:beforeSwap", function (event) {
    var output = document.querySelector("[data-log-output]");
    if (!output || !event.detail.target || event.detail.target.id !== "application-logs") return;
    logAnchor = null;
    var view = output.getBoundingClientRect();
    var rows = output.querySelectorAll("[data-log-id]");
    for (var index = 0; index < rows.length; index++) {
      var bounds = rows[index].getBoundingClientRect();
      if (bounds.bottom > view.top && bounds.top < view.bottom) {
        logAnchor = { id: rows[index].getAttribute("data-log-id"), offset: bounds.top - view.top };
        break;
      }
    }
  });
  function followLogs() {
    var output = document.querySelector("[data-log-output]");
    var follow = document.querySelector("[data-log-follow]");
    if (!output) return;
    if (!follow || follow.checked) {
      output.scrollTop = output.scrollHeight;
      return;
    }
    var row = logAnchor && output.querySelector('[data-log-id="' + logAnchor.id + '"]');
    if (row) output.scrollTop += row.getBoundingClientRect().top - output.getBoundingClientRect().top - logAnchor.offset;
  }
  document.addEventListener("change", function (event) {
    if (event.target.matches && event.target.matches("[data-log-follow]")) followLogs();
  });

  // Server access editor: any action needs "View server", changing files
  // needs reading them, and removing view removes everything. The server
  // applies the same rules when saving.
  function grantSummary(form) {
    var count = 0;
    form.querySelectorAll("[data-grant-server]").forEach(function (fieldset) {
      var view = fieldset.querySelector('input[value="server.view"]');
      if (view && view.checked) count++;
      var backup = fieldset.querySelector('input[value="backups.create"]');
      var note = fieldset.querySelector('[data-grant-note="backups.create"]');
      if (note) note.hidden = !(backup && backup.checked);
    });
    var summary = form.querySelector("[data-grant-summary]");
    if (summary) {
      summary.textContent = (count === 0 ? "No servers selected. This user will not see any servers."
        : count + (count === 1 ? " server" : " servers") + " selected.") + " Unsaved changes.";
    }
  }
  document.addEventListener("change", function (event) {
    var box = event.target;
    var fieldset = box.closest && box.closest("[data-grants] [data-grant-server]");
    if (!fieldset || box.type !== "checkbox") return;
    var find = function (value) { return fieldset.querySelector('input[value="' + value + '"]'); };
    if (box.checked) {
      if (find("server.view")) find("server.view").checked = true;
      if (box.value === "files.write" && find("files.read")) find("files.read").checked = true;
    } else if (box.value === "server.view") {
      fieldset.querySelectorAll("input[type=checkbox]").forEach(function (other) { other.checked = false; });
    } else if (box.value === "files.read" && find("files.write")) {
      find("files.write").checked = false;
    }
    grantSummary(box.closest("[data-grants]"));
  });
  document.addEventListener("click", function (event) {
    var preset = event.target.closest && event.target.closest("[data-grant-preset]");
    if (preset) {
      var chosen = preset.getAttribute("data-grant-preset").split(",");
      preset.closest("[data-grant-server]").querySelectorAll("input[type=checkbox]").forEach(function (box) {
        box.checked = chosen.indexOf(box.value) >= 0;
      });
      grantSummary(preset.closest("[data-grants]"));
      return;
    }
    // data-fill="input-id" with data-fill-value sets that field and focuses it.
    var fill = event.target.closest && event.target.closest("[data-fill]");
    if (fill) {
      var field = document.getElementById(fill.getAttribute("data-fill"));
      if (field) {
        field.value = fill.getAttribute("data-fill-value");
        field.focus();
      }
      return;
    }
    // data-clear="id" empties a region, such as a closed editor.
    var clear = event.target.closest && event.target.closest("[data-clear]");
    if (clear) {
      var region = document.getElementById(clear.getAttribute("data-clear"));
      if (region) region.replaceChildren();
    }
  });

  // data-autosubmit submits its form on change (a GET form keeps working
  // without scripts through its submit button).
  document.addEventListener("change", function (event) {
    var field = event.target;
    if (field.matches && field.matches("[data-autosubmit]") && field.form) field.form.requestSubmit();
  });

  // Dialogs swapped in with data-open-on-load open as modals and leave the
  // page when closed.
  function openDialogs(root) {
    var dialogs = root && root.matches && root.matches("dialog[data-open-on-load]") ? [root]
      : (root || document).querySelectorAll("dialog[data-open-on-load]");
    Array.prototype.forEach.call(dialogs, function (dialog) {
      if (dialog.open || !dialog.isConnected || !dialog.showModal) return;
      dialog.addEventListener("close", function () { dialog.remove(); }, { once: true });
      dialog.showModal();
    });
  }

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
      if (mode === "date") {
        element.textContent = dateOnly.format(timestamp);
      } else if (mode === "sentence" || mode === "plain") {
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
    timezones(event.detail.elt);
    openDialogs(event.detail.elt);
    if (event.detail.elt.id === "application-logs") followLogs();
  });

  onReady(function () {
    formatTimes();
    collapseLists();
    confirmations();
    timezones();
    msFields();
    followLogs();
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
