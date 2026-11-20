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

  // Morphing a live region keeps what only the page knows: a server's open
  // menu, and times already written in local form.
  if (window.htmx) {
    htmx.registerExtension("ludock", {
      htmx_before_morph_attr: function (elt, detail) {
        if (detail.attrName === "hidden" && elt.matches(".server-row .action-menu__content, .player-menu .action-menu__content")) return false;
        if (detail.attrName === "aria-expanded" && elt.matches(".server-row [data-action-menu] > button, .player-menu > button")) return false;
      },
      htmx_before_morph_node: function (elt, detail) {
        var incoming = detail.newNode;
        if (elt.tagName === "TIME" && elt.getAttribute("datetime") === incoming.getAttribute("datetime") &&
          elt.getAttribute("data-relative") === incoming.getAttribute("data-relative")) return false;
      },
    });
  }

  // hx-confirm asks in the page's own dialog instead of the browser's. htmx
  // has built the request by then, so a ticked option joins its body.
  var confirming = null;
  document.addEventListener("htmx:confirm", function (event) {
    var dialog = document.getElementById("confirm-dialog");
    var ctx = event.detail.ctx, elt = ctx.sourceElement;
    if (!dialog || !dialog.showModal) return;
    event.preventDefault();
    var detail = elt.getAttribute("data-confirm-detail") || "";
    var button = dialog.querySelector("[data-confirm-button]");
    dialog.querySelector("h2").textContent = ctx.confirm;
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
      // htmx holds the element's request queue until it hears either answer.
      if (dialog.returnValue !== "confirm") {
        event.detail.dropRequest();
        return;
      }
      if (optionName && checkbox.checked) ctx.request.body.set(optionName, "true");
      event.detail.issueRequest();
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
  // disabled while the request runs. The request remembers its button, which
  // a refresh may replace meanwhile.
  function busyButton(elt) {
    if (!elt || !elt.matches) return null;
    if (elt.matches("button[data-busy-label]")) return elt;
    return elt.matches("form") ? elt.querySelector("button[type=submit][data-busy-label]") : null;
  }
  document.addEventListener("htmx:before:request", function (event) {
    var ctx = event.detail.ctx, button = busyButton(ctx.sourceElement);
    if (!button || button.hasAttribute("data-idle-label")) return;
    button.setAttribute("data-idle-label", button.textContent);
    button.textContent = button.getAttribute("data-busy-label");
    button.disabled = true;
    ctx.busyButton = button;
  });
  // Restore busy buttons, and close a dialog after its htmx form succeeds.
  document.addEventListener("htmx:finally:request", function (event) {
    var ctx = event.detail.ctx, button = ctx.busyButton;
    if (button && button.hasAttribute("data-idle-label")) {
      button.textContent = button.getAttribute("data-idle-label");
      button.removeAttribute("data-idle-label");
      button.disabled = false;
    }
    var form = ctx.request.form;
    if (ctx.response && ctx.response.status < 400 && form && form.matches("dialog form[data-close-on-success]")) {
      form.closest("dialog").close();
    }
  });

  // Timed polls wait while the tab is hidden, as the event stream does.
  // Showing the tab again refreshes the live server views that missed a poll.
  function timed(ctx) {
    return !!ctx.sourceEvent && ctx.sourceEvent.type === "every";
  }
  var missedPoll = false;
  document.addEventListener("htmx:config:request", function (event) {
    if (document.hidden && timed(event.detail.ctx)) {
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
  document.addEventListener("htmx:response:error", function (event) {
    if (event.detail.ctx.response.status === 401) window.location.assign("/login");
  });

  // data-shown-by="checkbox-id" shows an element while the box is checked and
  // data-hidden-by hides it; data-shown-when="field=value" shows it while the
  // form's field has that value, or one of "a|b". A radio's siblings change
  // too, without their own events.
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
      element.hidden = !field || rule[1].split("|").indexOf(field.value) < 0;
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
    }
  });
  function timezones(root) {
    (root || document).querySelectorAll("input[data-default-local-timezone]").forEach(function (input) {
      // A zone other than the browser's is shown, not tucked away.
      var disclosure = input.closest("details");
      if (input.value && input.value !== localTimezone && disclosure) disclosure.open = true;
      if (input.value) return;
      input.value = localTimezone;
      if (input.form) input.form.dispatchEvent(new Event("change", { bubbles: true }));
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
    error.textContent = "The start date must be before the end date.";
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

  // Custom access: any choice includes seeing the server, changing files
  // includes reading them, and unticking seeing the server clears the rest.
  // The server applies the same rules when saving.
  document.addEventListener("change", function (event) {
    var box = event.target;
    var fieldset = box.closest && box.closest("[data-custom-access]");
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
  });
  document.addEventListener("click", function (event) {
    // data-fill="input-id" with data-fill-value sets that field and focuses it.
    var fill = event.target.closest && event.target.closest("[data-fill]");
    if (fill) {
      var field = document.getElementById(fill.getAttribute("data-fill"));
      if (field) {
        field.value = fill.getAttribute("data-fill-value");
        field.focus();
      }
    }
  });

  // A live refresh never replaces an open dialog, or the element whose
  // confirmation is open (such as a stop in the server list); it refreshes
  // once the dialog closes. A response's other partials leave them alone.
  document.addEventListener("htmx:before:swap", function (event) {
    var detail = event.detail, source = detail.ctx.sourceElement;
    for (var index = detail.tasks.length - 1; index >= 0; index--) {
      var task = detail.tasks[index], target = task.target;
      if (!target || target.nodeType !== 1 || task.swapSpec.style === "none") continue;
      var open = target.querySelector("dialog[open]");
      if (!open && confirming && target.contains(confirming)) open = document.getElementById("confirm-dialog");
      if (!open || (source.closest && source.closest("dialog") === open)) continue;
      if (task.type !== "main") {
        detail.tasks.splice(index, 1);
        continue;
      }
      event.preventDefault();
      if (open.hasAttribute("data-refresh-pending")) return;
      open.setAttribute("data-refresh-pending", "");
      open.addEventListener("close", function () {
        open.removeAttribute("data-refresh-pending");
        if (window.htmx) window.htmx.trigger(document.body, "servers-changed");
      }, { once: true });
      return;
    }
  });

  // A refresh never replaces a select whose choice it didn't see, such as a
  // filter changed while the list was loading.
  document.addEventListener("htmx:before:swap", function (event) {
    var tasks = event.detail.tasks;
    for (var index = tasks.length - 1; index >= 0; index--) {
      var live = tasks[index].target, incoming = tasks[index].fragment.querySelector("select");
      if (tasks[index].type === "main" || !live || live.tagName !== "SELECT" || !incoming || incoming.id !== live.id) continue;
      var chosen = incoming.querySelector("option[selected]") || incoming.querySelector("option");
      if (chosen && chosen.value !== live.value) tasks.splice(index, 1);
    }
  });

  // In-page navigation on a server's page (tabs and links that swap
  // #detail-body): only the newest navigation swaps, so a slow earlier response
  // can't replace a later click, and a failed one loads the address as a full
  // page so its error shows. An action follows its work with HX-Location only
  // if nothing navigated since it was sent.
  var navigation = 0;
  function navigates(ctx) {
    return !!ctx.target && ctx.target.id === "detail-body";
  }
  document.addEventListener("htmx:before:request", function (event) {
    var ctx = event.detail.ctx;
    // A request queued for a view that is gone has nothing left to update.
    if (ctx.target && ctx.target.isConnected === false) {
      event.preventDefault();
      return;
    }
    ctx.navigation = navigates(ctx) ? ++navigation : navigation;
  });
  document.addEventListener("htmx:after:request", function (event) {
    var ctx = event.detail.ctx;
    if (ctx.request.method !== "GET" && ctx.hx.location && ctx.navigation !== navigation) delete ctx.hx.location;
  });
  document.addEventListener("htmx:before:swap", function (event) {
    var ctx = event.detail.ctx;
    if (!navigates(ctx)) return;
    if (ctx.navigation !== navigation) {
      event.preventDefault();
    } else if (ctx.response.status >= 400) {
      event.preventDefault();
      window.location.assign(ctx.request.action);
    }
  });

  // A modified click opens a link's own address in a new tab or window; htmx
  // 4.0.2 cancels it on links it handles, so the click never reaches htmx.
  document.addEventListener("click", function (event) {
    if (!(event.ctrlKey || event.metaKey || event.shiftKey) || !event.target.closest) return;
    if (event.target.closest("a[href][data-htmx-powered]")) event.stopPropagation();
  }, true);

  // Live regions send the ETag of the fragment they show, and the server
  // answers an unchanged fragment with a bodiless 304. Any other swap into or
  // over a region forgets its tag; that handler comes after the others, so it
  // skips swaps they cancelled. A hung timed refresh gives up after 30
  // seconds.
  var shown = {};
  function forget(target) {
    Object.keys(shown).forEach(function (id) {
      var node = shown[id].node;
      if (!node.isConnected || node === target || node.contains(target) || target.contains(node)) delete shown[id];
    });
  }
  document.addEventListener("htmx:config:request", function (event) {
    var ctx = event.detail.ctx;
    if (timed(ctx)) ctx.request.timeout = 30000;
    if (ctx.request.method !== "GET" || ctx.sourceElement !== ctx.target || !ctx.target.id) return;
    // The swap may replace the element, so the request keeps the region's id.
    ctx.region = ctx.target.id;
  });
  // A request may wait in a queue behind one that changes the region, so it
  // names the tag shown when it is sent.
  document.addEventListener("htmx:before:request", function (event) {
    var ctx = event.detail.ctx, last = ctx.region && shown[ctx.region];
    if (last && last.node === ctx.target) ctx.request.headers["If-None-Match"] = last.tag;
  });
  // The elements a swap is about to change. A 304 changes none.
  function swapTargets(detail) {
    return detail.tasks.filter(function (task) {
      return task.target && task.target.nodeType === 1 && task.swapSpec.style !== "none";
    }).map(function (task) { return task.target; });
  }
  document.addEventListener("htmx:before:swap", function (event) {
    if (!event.defaultPrevented) swapTargets(event.detail).forEach(forget);
  });
  document.addEventListener("htmx:after:swap", function (event) {
    var ctx = event.detail.ctx, node = ctx.region && document.getElementById(ctx.region);
    var tag = ctx.response && ctx.response.status === 200 && ctx.response.headers.get("ETag");
    if (tag && node) shown[ctx.region] = { node: node, tag: tag };
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
  function stream(elt) {
    return !!elt && !!elt.hasAttribute && elt.hasAttribute("hx-sse:connect");
  }
  function failed(ctx) {
    var refresh = !!ctx.region || timed(ctx);
    if (ctx.request.method !== "GET" && !refresh) {
      notice("change", "Ludock didn’t answer, so your change may not have been made. Check your connection, then check the result before trying again.");
    } else if (!refresh) {
      if (unreachable !== "change") notice("refresh", "Can’t reach Ludock. Check your connection and try again.");
    } else if (!unreachable && !document.querySelector("[data-live-status]:not([hidden])")) {
      // A page whose own banner says live updates are disconnected says enough.
      notice("refresh", "Can’t reach Ludock. Showing the last known state until the connection is back.");
    }
  }
  function answered(ctx) {
    if (!unreachable || (unreachable === "change" && (ctx.request.method === "GET" || timed(ctx)))) return;
    var missed = unreachable === "refresh";
    notice(null);
    if (missed) catchUp();
  }
  // htmx reports these on the document when a refresh has replaced the
  // element that sent the request. An error with no response never arrived;
  // the event stream reports its own state.
  document.addEventListener("htmx:after:request", function (event) { answered(event.detail.ctx); });
  document.addEventListener("htmx:error", function (event) {
    var ctx = event.detail.ctx;
    if (ctx && !ctx.response && !stream(ctx.sourceElement)) failed(ctx);
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
  // the browser paints or an observer sees it.
  document.addEventListener("htmx:before:settle", function () { formatTimes(); });

  // A link to part of a page lands there even when regions above it load
  // afterwards and push it down, which browsers without scroll anchoring,
  // such as Safari, leave alone. It stops once the person scrolls or types.
  (function () {
    var target = null;
    try { target = document.getElementById(decodeURIComponent(location.hash.slice(1))); } catch (error) {}
    if (!target) return;
    var follow = true;
    function stop() { follow = false; }
    ["wheel", "touchstart", "keydown", "mousedown"].forEach(function (name) {
      window.addEventListener(name, stop, { once: true, passive: true });
    });
    setTimeout(stop, 10000);
    document.addEventListener("htmx:after:settle", function () {
      if (follow && target.isConnected) target.scrollIntoView({ block: "start" });
    });
  })();

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

  // Live updates arrive over an event stream, which htmx closes while the tab
  // is hidden and reopens when it shows. A stream that reconnects may have
  // missed events, so live views refresh. htmx retries a stream that drops,
  // but not one that never opened.
  function liveStatus(connected) {
    document.querySelectorAll("[data-live-status]").forEach(function (element) { element.hidden = connected; });
  }
  var streamed = false;
  document.addEventListener("htmx:sse:after:connection", function () {
    liveStatus(true);
    if (streamed) catchUp();
    streamed = true;
  });
  document.addEventListener("htmx:sse:error", function (event) {
    if (event.detail.status === 401) window.location.assign("/login");
    else liveStatus(false);
  });
  document.addEventListener("htmx:sse:before:message", function (event) {
    if (event.detail.message.event === "expired") window.location.assign("/login");
  });
  document.addEventListener("htmx:finally:request", function (event) {
    var elt = event.detail.ctx.sourceElement;
    if (!stream(elt) || (elt._htmx && elt._htmx.sse)) return;
    liveStatus(false);
    streamed = true;
    setTimeout(function () { htmx.trigger(elt, "live-retry"); }, 5000);
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

  // htmx processes each element it adds or morphs.
  document.addEventListener("htmx:after:process", function (event) {
    var root = event.target;
    collapseLists(root);
    confirmations(root);
    timezones(root);
    openDialogs(root);
  });

  onReady(function () {
    formatTimes();
    collapseLists();
    confirmations();
    timezones();
    msFields();
  });
})();
