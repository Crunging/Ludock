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

  // hx-confirm asks in the page's own dialog instead of the browser's.
  var confirming = null;
  document.addEventListener("htmx:confirm", function (event) {
    var dialog = document.getElementById("confirm-dialog");
    var elt = event.detail.elt;
    if (!event.detail.question || !dialog || !dialog.showModal) return;
    event.preventDefault();
    var detail = elt.getAttribute("data-confirm-detail") || "";
    var button = dialog.querySelector("[data-confirm-button]");
    dialog.querySelector("h2").textContent = event.detail.question;
    dialog.querySelector("#confirm-dialog-detail").textContent = detail;
    dialog.querySelector("#confirm-dialog-detail").hidden = !detail;
    var optionName = elt.getAttribute("data-confirm-option");
    var option = dialog.querySelector("[data-confirm-option]");
    var checkbox = option.querySelector("input");
    option.hidden = !optionName;
    checkbox.checked = false;
    option.querySelector("span").textContent = elt.getAttribute("data-confirm-option-label") || "";
    option.querySelector("p").textContent = elt.getAttribute("data-confirm-option-help") || "";
    button.textContent = elt.getAttribute("data-confirm-action") || "Confirm";
    button.className = elt.hasAttribute("data-confirm-danger") ? "secondary-btn secondary-btn--danger" : "primary-btn";
    dialog.returnValue = "";
    confirming = elt;
    dialog.addEventListener("close", function () {
      confirming = null;
      // An action in a menu closed the menu; focus returns to its button.
      var menu = elt.closest("[data-action-menu]");
      if (menu && elt.closest("[hidden]")) menu.querySelector(":scope > button").focus();
      if (dialog.returnValue !== "confirm") return;
      // htmx reads hx-vals as it builds the request.
      if (optionName) {
        var values = {};
        if (checkbox.checked) values[optionName] = "true";
        elt.setAttribute("hx-vals", JSON.stringify(values));
      }
      elt.setAttribute("data-confirmed-sending", "");
      elt.addEventListener("htmx:afterRequest", function () { elt.removeAttribute("data-confirmed-sending"); }, { once: true });
      event.detail.issueRequest(true);
    }, { once: true });
    dialog.showModal();
  });

  // data-dialog-close closes its native dialog.
  document.addEventListener("click", function (event) {
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
    // htmx leaves "successful" unset when the response moves the page with
    // HX-Location, which follows queued work.
    var moved = event.detail.xhr && event.detail.xhr.status < 400 && event.detail.xhr.getResponseHeader("HX-Location");
    if ((event.detail.successful || moved) && form && form.matches && form.matches("dialog form[data-close-on-success]")) {
      form.closest("dialog").close();
    }
  });

  // Timed polls wait while the tab is hidden; events still refresh. Showing
  // the tab again refreshes the live server views that missed a poll.
  function polls(elt) {
    var parts = (elt.getAttribute && elt.getAttribute("hx-trigger") || "").split(",").map(function (part) { return part.trim(); });
    return parts.some(function (part) { return part.indexOf("every ") === 0; }) &&
      !parts.some(function (part) { return part === "load" || part.indexOf("load ") === 0; });
  }
  var missedPoll = false;
  document.addEventListener("htmx:beforeRequest", function (event) {
    if (document.hidden && !event.detail.requestConfig.triggeringEvent && polls(event.detail.elt)) {
      missedPoll = true;
      event.preventDefault();
    }
  });
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden && missedPoll && window.htmx) {
      missedPoll = false;
      htmx.trigger(document.body, "servers-changed");
    }
  });

  // Session expiry or revocation sends the browser to sign in again.
  document.addEventListener("htmx:responseError", function (event) {
    if (event.detail.xhr && event.detail.xhr.status === 401) window.location.assign("/login");
  });

  // data-shown-by="checkbox-id" shows an element while the box is checked and
  // data-hidden-by hides it; data-shown-when="field=value" shows it while the
  // form's field has that value. A radio's siblings change too, without their
  // own events.
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

  // A live refresh never replaces an open dialog (such as a stop
  // confirmation in the server list), or the element a confirmation will
  // send; it refreshes once the dialog closes.
  document.addEventListener("htmx:beforeSwap", function (event) {
    var target = event.detail.target;
    var open = target && target.querySelector && target.querySelector("dialog[open]");
    if (!open && confirming && target && target.contains && target.contains(confirming)) open = document.getElementById("confirm-dialog");
    if (!open || event.detail.requestConfig.elt.closest("dialog") === open) return;
    event.detail.shouldSwap = false;
    if (open.hasAttribute("data-refresh-pending")) return;
    open.setAttribute("data-refresh-pending", "");
    open.addEventListener("close", function () {
      open.removeAttribute("data-refresh-pending");
      if (window.htmx) window.htmx.trigger(document.body, "servers-changed");
    }, { once: true });
  });

  // A live server view's refresh waits while a confirmed request from inside it
  // is on its way, since that response's events only reach the page while the
  // sending element is there. The view reloads once it answers.
  document.addEventListener("htmx:beforeSwap", function (event) {
    var target = event.detail.target;
    if (!target || !target.querySelector || event.detail.requestConfig.verb !== "get" ||
      (target.getAttribute("hx-trigger") || "").indexOf("servers-changed") === -1) return;
    var sending = target.querySelector("[data-confirmed-sending]");
    if (!sending || sending === event.detail.requestConfig.elt) return;
    event.detail.shouldSwap = false;
    if (sending.hasAttribute("data-refresh-pending")) return;
    sending.setAttribute("data-refresh-pending", "");
    sending.addEventListener("htmx:afterRequest", function () {
      sending.removeAttribute("data-refresh-pending");
      if (window.htmx) window.htmx.trigger(document.body, "servers-changed");
    }, { once: true });
  });

  // In-page navigation on a server's page (tabs and links that swap
  // #detail-body): only the newest navigation swaps, so a slow earlier response
  // can't replace a later click, and a failed one loads the address as a full
  // page so its error shows. Each request guards its own target, since a newer
  // response may already have replaced it. An action follows its work with
  // HX-Location only if nothing navigated since it was sent; the check listens
  // on the action's button, which a navigation may have replaced.
  var navigation = 0, followStale = false;
  document.addEventListener("htmx:beforeRequest", function (event) {
    var xhr = event.detail.xhr, config = event.detail.requestConfig, elt = event.target, sent = navigation;
    if (!xhr || !config || config.verb === "get") return;
    elt.addEventListener("htmx:beforeOnLoad", function check(load) {
      if (load.detail.xhr !== xhr) return;
      elt.removeEventListener("htmx:beforeOnLoad", check);
      followStale = sent !== navigation && !!xhr.getResponseHeader("HX-Location");
    });
  });
  document.addEventListener("htmx:beforeRequest", function (event) {
    var target = event.detail.target, xhr = event.detail.xhr;
    if (!target || !xhr || !target.closest || !target.closest("#detail-body")) return;
    // A request queued for a view that is gone has nothing left to update.
    if (!target.isConnected) {
      event.preventDefault();
      return;
    }
    var navigates = target.id === "detail-body";
    if (navigates && followStale && event.target === document.body) {
      followStale = false;
      event.preventDefault();
      return;
    }
    var view = navigates ? ++navigation : navigation;
    target.addEventListener("htmx:beforeSwap", function guard(swap) {
      if (swap.detail.xhr !== xhr) return;
      target.removeEventListener("htmx:beforeSwap", guard);
      if (view !== navigation) {
        swap.detail.shouldSwap = false;
      } else if (navigates && xhr.status >= 400) {
        swap.detail.shouldSwap = false;
        window.location.assign(swap.detail.pathInfo.finalRequestPath || swap.detail.pathInfo.requestPath);
      }
    });
  });
  // A second click on a link whose request is still pending starts its own
  // request, since htmx drops a queued one once a newer view replaces the link.
  document.addEventListener("click", function (event) {
    var link = event.target.closest && event.target.closest("#detail-body a[hx-boost]");
    if (!link || !link.classList.contains("htmx-request") || !window.htmx) return;
    if (event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    var source = document.createElement("span");
    source.hidden = true;
    document.body.appendChild(source);
    window.htmx.ajax("GET", link.getAttribute("href"), { source: source, target: "#detail-body", select: "#detail-body", swap: "outerHTML show:none", push: "true" })
      .finally(function () { source.remove(); });
  }, true);

  // An out-of-band swap never replaces the element a confirmation will send.
  document.addEventListener("htmx:oobBeforeSwap", function (event) {
    var target = event.detail.target;
    if (confirming && target && (target === confirming || target.contains(confirming))) event.detail.shouldSwap = false;
  });

  // A refresh never replaces a select whose choice it didn't see, such as a
  // filter changed while the list was loading: the form's queued request
  // reads the select when it runs, so the choice must still be there.
  document.addEventListener("htmx:oobBeforeSwap", function (event) {
    var live = event.detail.target;
    var incoming = event.detail.fragment && event.detail.fragment.querySelector && event.detail.fragment.querySelector("select");
    if (!live || live.tagName !== "SELECT" || !incoming || incoming.id !== live.id) return;
    var chosen = incoming.querySelector("option[selected]") || incoming.querySelector("option");
    if (chosen && chosen.value !== live.value) event.detail.shouldSwap = false;
  });

  // Live regions send the ETag of the fragment they show, and the server
  // answers an unchanged fragment with a bodiless 304. Any other swap into or
  // over a region forgets its tag. These handlers come after the others so they
  // see swaps the others cancelled. A hung timed refresh gives up after 30
  // seconds.
  var shown = {};
  function refreshesItself(config) {
    return config && config.verb === "get" && config.elt === config.target && !config.boosted && !!config.elt.id;
  }
  function forget(target) {
    if (!target) return;
    Object.keys(shown).forEach(function (id) {
      var node = shown[id].node;
      if (!node.isConnected || node === target || node.contains(target) || (target.contains && target.contains(node))) delete shown[id];
    });
  }
  function timed(elt) {
    return !!elt && !!elt.getAttribute && (elt.getAttribute("hx-trigger") || "").indexOf("every ") !== -1;
  }
  document.addEventListener("htmx:configRequest", function (event) {
    var config = event.detail;
    if (timed(config.elt)) config.timeout = 30000;
    if (!refreshesItself(config)) return;
    var last = shown[config.elt.id];
    if (last && last.node === config.elt) config.headers["If-None-Match"] = last.tag;
  });
  document.addEventListener("htmx:beforeSwap", function (event) { if (event.detail.shouldSwap) forget(event.detail.target); });
  document.addEventListener("htmx:oobBeforeSwap", function (event) { if (event.detail.shouldSwap) forget(event.detail.target); });
  document.addEventListener("htmx:afterSwap", function (event) {
    var detail = event.detail, xhr = detail.xhr, config = detail.requestConfig;
    if (!xhr || xhr.status !== 200 || !refreshesItself(config)) return;
    var tag = xhr.getResponseHeader("ETag"), node = document.getElementById(config.elt.id);
    if (tag && node) shown[node.id] = { node: node, tag: tag };
  });

  // A request that can't reach Ludock shows a notice at the bottom of the page.
  // Refresh notices clear once anything answers; a notice for a change the user
  // sent stays until the next change answers or it is dismissed. When the
  // connection returns, live views catch up.
  var unreachable = null;
  function notice(kind, message) {
    var region = document.querySelector("[data-connection-notice]");
    if (!region) return;
    region.textContent = "";
    unreachable = kind;
    if (!kind) return;
    var alert = document.createElement("div");
    alert.className = "alert alert--" + (kind === "change" ? "error" : "warning");
    var text = document.createElement("p");
    text.textContent = message;
    alert.appendChild(text);
    if (kind === "change") {
      var dismiss = document.createElement("button");
      dismiss.type = "button";
      dismiss.setAttribute("aria-label", "Dismiss");
      dismiss.textContent = "×";
      dismiss.addEventListener("click", function () { notice(null); });
      alert.appendChild(dismiss);
    }
    region.appendChild(alert);
  }
  function catchUp() {
    if (window.htmx) htmx.trigger(document.body, "servers-changed");
  }
  function failed(config) {
    var refresh = refreshesItself(config) || timed(config.elt);
    if (config.verb !== "get" && !refresh) {
      notice("change", "Ludock didn’t answer, so your change may not have been made. Check your connection, then check the result before trying again.");
    } else if (!refresh) {
      if (unreachable !== "change") notice("refresh", "Can’t reach Ludock. Check your connection and try again.");
    } else if (!unreachable && !document.querySelector("[data-live-status]:not([hidden])")) {
      // A page whose own banner says live updates are disconnected says enough.
      notice("refresh", "Can’t reach Ludock. Showing the last known state until the connection is back.");
    }
  }
  function answered(config) {
    if (!unreachable || (unreachable === "change" && (config.verb === "get" || timed(config.elt)))) return;
    var missed = unreachable === "refresh";
    notice(null);
    if (missed) catchUp();
  }
  // htmx reports a request's end on the element that sent it, which a
  // refresh may have replaced by then, so these listen on the request.
  document.addEventListener("htmx:beforeRequest", function (event) {
    var xhr = event.detail.xhr, config = event.detail.requestConfig;
    if (!xhr || !config) return;
    xhr.addEventListener("load", function () { answered(config); });
    xhr.addEventListener("error", function () { failed(config); });
    xhr.addEventListener("timeout", function () { failed(config); });
  });
  window.addEventListener("online", catchUp);

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

  // Copy buttons: data-copy="text". The button's own markup, such as an
  // icon, comes back after the confirmation, even after repeated clicks.
  var copyIdle = new WeakMap();
  document.addEventListener("click", function (event) {
    var button = event.target.closest && event.target.closest("[data-copy]");
    if (!button || !navigator.clipboard) return;
    event.preventDefault();
    navigator.clipboard.writeText(button.getAttribute("data-copy")).then(function () {
      if (!copyIdle.has(button)) copyIdle.set(button, { html: button.innerHTML });
      var idle = copyIdle.get(button);
      clearTimeout(idle.timer);
      button.textContent = button.getAttribute("data-copied-label") || "Copied";
      idle.timer = setTimeout(function () {
        button.innerHTML = idle.html;
        copyIdle.delete(button);
      }, 1500);
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
  // Swapped content is formatted in the same task that inserts it, before
  // the browser paints; htmx:load comes only after settling, a frame later.
  document.addEventListener("htmx:afterSwap", function () { formatTimes(); });

  // Resource charts: a crosshair follows the pointer, or the arrow keys on a
  // focused chart, and reads out the nearest sample in every chart of the
  // section, so CPU and memory show the same minute.
  var clock = new Intl.DateTimeFormat(undefined, { timeStyle: "short" });
  function chartPoints(chart) {
    if (!chart.resourcePoints) chart.resourcePoints = JSON.parse(chart.getAttribute("data-points") || "[]");
    return chart.resourcePoints;
  }
  function nearestIndex(points, at) {
    var best = -1;
    points.forEach(function (point, index) {
      if (best < 0 || Math.abs(point[0] - at) < Math.abs(points[best][0] - at)) best = index;
    });
    return best;
  }
  function readOut(section, at) {
    section.querySelectorAll(".resource-chart").forEach(function (chart) {
      var points = chartPoints(chart), step = Number(chart.dataset.step);
      var start = Number(chart.dataset.start), span = Number(chart.dataset.span);
      var cursor = chart.querySelector(".resource-chart__cursor"), readout = chart.querySelector(".resource-chart__readout");
      var index = at === null ? -1 : nearestIndex(points, at);
      // The pointer is in a gap: show nothing rather than a distant sample.
      if (index < 0 || Math.abs(points[index][0] - at) > 1.5 * step) {
        cursor.hidden = readout.hidden = true;
        return;
      }
      var point = points[index], left = (point[0] - start) / span * 100;
      var value = document.createElement("strong");
      value.textContent = point[1];
      var time = document.createElement("span");
      time.textContent = step > 60000 && clock.formatRange ? clock.formatRange(point[0], point[0] + step) : clock.format(point[0]);
      readout.replaceChildren(value, time);
      if (point[2]) {
        var peak = document.createElement("span");
        peak.textContent = point[2];
        readout.append(peak);
      }
      cursor.style.left = readout.style.left = left + "%";
      readout.classList.toggle("resource-chart__readout--flip", left > 55);
      cursor.hidden = readout.hidden = false;
    });
  }
  function pointerAt(plot, event) {
    var chart = plot.closest(".resource-chart"), box = plot.getBoundingClientRect();
    var share = Math.min(1, Math.max(0, (event.clientX - box.left) / box.width));
    return Number(chart.dataset.start) + share * Number(chart.dataset.span);
  }
  var readingSection = null;
  function followPointer(event) {
    var plot = event.target.closest && event.target.closest(".resource-chart__plot");
    var section = plot && plot.closest(".resource-history");
    if (readingSection && readingSection !== section) readOut(readingSection, null);
    readingSection = section;
    if (section) readOut(section, pointerAt(plot, event));
  }
  document.addEventListener("pointermove", followPointer);
  document.addEventListener("pointerdown", followPointer);
  document.addEventListener("focusin", function (event) {
    var plot = event.target.closest && event.target.closest(".resource-chart__plot");
    if (!plot) return;
    var points = chartPoints(plot.closest(".resource-chart"));
    plot.readingAt = points.length ? points[points.length - 1][0] : null;
    readOut(plot.closest(".resource-history"), plot.readingAt);
  });
  document.addEventListener("focusout", function (event) {
    var plot = event.target.closest && event.target.closest(".resource-chart__plot");
    if (plot) readOut(plot.closest(".resource-history"), null);
  });
  document.addEventListener("keydown", function (event) {
    var plot = event.target.closest && event.target.closest(".resource-chart__plot");
    var moves = { ArrowLeft: -1, ArrowRight: 1, Home: -Infinity, End: Infinity };
    if (!plot || !(event.key in moves) || plot.readingAt === null) return;
    event.preventDefault();
    var points = chartPoints(plot.closest(".resource-chart"));
    var index = Math.min(points.length - 1, Math.max(0, nearestIndex(points, plot.readingAt) + moves[event.key]));
    plot.readingAt = points[index][0];
    readOut(plot.closest(".resource-history"), plot.readingAt);
  });

  // data-clear-filters="form-id" empties a filter form and applies it.
  document.addEventListener("click", function (event) {
    var clear = event.target.closest && event.target.closest("[data-clear-filters]");
    var form = clear && document.getElementById(clear.getAttribute("data-clear-filters"));
    if (!form) return;
    Array.prototype.forEach.call(form.elements, function (field) {
      if (field.tagName === "SELECT") field.selectedIndex = 0;
      else if (field.type === "search" || field.type === "text") field.value = "";
    });
    form.requestSubmit();
    var first = form.querySelector("input[type=search], input[type=text]");
    if (first) first.focus();
  });

  // Disclosure menus: [data-action-menu] > button + content.
  document.addEventListener("click", function (event) {
    var trigger = event.target.closest && event.target.closest("[data-action-menu] > button");
    document.querySelectorAll("[data-action-menu]").forEach(function (menu) {
      var button = menu.querySelector(":scope > button");
      var content = menu.querySelector(".action-menu__content");
      var open = trigger && trigger === button ? button.getAttribute("aria-expanded") !== "true" : false;
      if (!trigger && menu.contains(event.target) && !event.target.closest("[hx-confirm], a")) return;
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

  // Phones fold the navigation into a menu; without this script it wraps.
  document.querySelectorAll("[data-harbor]").forEach(function (harbor) { harbor.classList.add("harbor--menu"); });
  function harborMenu(open) {
    document.querySelectorAll("[data-harbor]").forEach(function (harbor) {
      harbor.classList.toggle("harbor--open", open);
      var button = harbor.querySelector("[data-harbor-menu]");
      if (button) button.setAttribute("aria-expanded", open ? "true" : "false");
    });
  }
  document.addEventListener("click", function (event) {
    if (event.target.closest && event.target.closest("[data-harbor-menu]")) {
      harborMenu(event.target.closest("[data-harbor-menu]").getAttribute("aria-expanded") !== "true");
    }
  });
  document.addEventListener("keydown", function (event) {
    var button = document.querySelector("[data-harbor-menu][aria-expanded=true]");
    if (event.key !== "Escape" || !button) return;
    harborMenu(false);
    button.focus();
  });

  // Live update connection status and session expiry over SSE.
  function liveStatus(connected) {
    document.querySelectorAll("[data-live-status]").forEach(function (element) { element.hidden = connected; });
  }
  // A stream that reconnects may have missed events, so live views refresh.
  var streamLost = false;
  document.addEventListener("htmx:sseOpen", function () {
    liveStatus(true);
    if (streamLost) catchUp();
    streamLost = false;
  });
  document.addEventListener("htmx:sseError", function () {
    liveStatus(false);
    streamLost = true;
  });
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
    // Error and success responses both render into their targets; a 304
    // keeps the region a refresh found unchanged.
    if (window.htmx) {
      htmx.config.responseHandling = [
        { code: "204", swap: false },
        { code: "304", swap: false },
        { code: "[23]..", swap: true },
        { code: "401", swap: false, error: true },
        { code: "[45]..", swap: true, error: false },
      ];
    }
  });
})();
