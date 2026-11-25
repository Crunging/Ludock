// File uploads: raw PUTs of up to 100 files or 16 MiB each, one after
// another, into the folder shown; a larger file goes alone. The server
// checks the size limit, each name, and access while it writes.
"use strict";
// The file editor saves raw text with a PUT, since form bodies are capped
// far below the preview limit; the response replaces the dialog's content.
(function () {
  var csrf = document.querySelector('meta[name="csrf-token"]');
  function editor(dialog) { return dialog.querySelector("form[data-file-editor] textarea"); }
  function unsaved(text) { return text && (text.hasAttribute("data-unsaved") || text.value !== text.defaultValue); }
  // Escape would close the dialog and drop edits.
  document.addEventListener("cancel", function (event) {
    if (!event.target.matches || !event.target.matches("dialog")) return;
    var text = editor(event.target);
    if (unsaved(text) || (text && text.readOnly)) event.preventDefault();
  }, true);
  // Saving and canceling each replace the dialog, so while either is pending
  // the editor and its buttons are locked.
  function lock(form, on) {
    form.querySelector("textarea").readOnly = on;
    form.querySelectorAll("button").forEach(function (control) { control.disabled = on; });
  }
  function editorForm(ctx) { return ctx.sourceElement && ctx.sourceElement.closest && ctx.sourceElement.closest("form[data-file-editor]"); }
  document.addEventListener("htmx:before:request", function (event) {
    var form = editorForm(event.detail.ctx);
    if (form) lock(form, true);
  });
  document.addEventListener("htmx:finally:request", function (event) {
    var form = editorForm(event.detail.ctx);
    if (form && form.isConnected) lock(form, false);
  });
  document.addEventListener("submit", async function (event) {
    var form = event.target.closest && event.target.closest("form[data-file-editor]");
    if (!form) return;
    event.preventDefault();
    var dialog = form.closest("dialog");
    var button = form.querySelector('button[type="submit"]');
    var text = form.querySelector("textarea");
    if (text.readOnly) return;
    function busy(on) {
      lock(form, on);
      button.textContent = on ? "Saving…" : "Save";
    }
    busy(true);
    var body;
    try {
      var response = await fetch(form.getAttribute("data-file-editor"), {
        method: "PUT",
        headers: { "Content-Type": "application/octet-stream", "X-CSRF-Token": csrf ? csrf.content : "", "HX-Request": "true" },
        body: text.value,
        credentials: "same-origin",
      });
      if (response.status === 401) {
        window.location.assign("/login");
        return;
      }
      body = await response.text();
      if (!/file-preview-title/.test(body)) {
        // Other failures come back as an alert fragment; show only its text.
        var template = document.createElement("template");
        template.innerHTML = body;
        throw new Error(template.content.textContent.trim() || "The file could not be saved (HTTP " + response.status + ").");
      }
    } catch (error) {
      var alert = document.createElement("div");
      alert.className = "alert alert--error";
      alert.setAttribute("role", "alert");
      alert.textContent = error instanceof TypeError ? "The file could not be saved. Check your connection and try again." : error.message;
      form.querySelector("[data-file-editor-error]").replaceChildren(alert);
      busy(false);
      return;
    }
    dialog.innerHTML = body;
    if (window.htmx) window.htmx.process(dialog);
    var focus = dialog.querySelector("textarea, [autofocus]");
    if (focus) focus.focus();
    if (!dialog.querySelector("form[data-file-editor]") && window.htmx) window.htmx.trigger(document.body, "files-changed");
  });
})();

(function () {
  var page = document.querySelector("[data-files]");
  var zone = document.querySelector("[data-dropzone]");
  if (!page || !zone) return;
  var input = document.querySelector("[data-upload-input]");
  var choose = document.querySelector("[data-choose-files]");
  var progress = document.querySelector("[data-upload-progress]");
  var status = document.querySelector("[data-upload-status]");
  var cancel = document.querySelector("[data-upload-cancel]");
  var feedback = document.getElementById("file-feedback");
  var csrf = document.querySelector('meta[name="csrf-token"]');
  var maxUpload = Number(page.getAttribute("data-max-upload")) || 0;
  var batchFiles = 100, batchBytes = 16 * 1024 * 1024;
  var controller = null;

  function disabled() { return zone.getAttribute("aria-disabled") === "true" || controller !== null; }
  // A notice is a sentence and, for files that didn't upload, one line each.
  function showNotice(kind, message, lines) {
    if (!feedback) return;
    var alert = document.createElement("div");
    alert.className = "alert alert--" + kind;
    alert.setAttribute("role", kind === "error" ? "alert" : "status");
    if (!lines || lines.length === 0) {
      alert.textContent = message;
    } else {
      var body = document.createElement("div");
      var summary = document.createElement("p");
      summary.textContent = message;
      var list = document.createElement("ul");
      lines.forEach(function (line) {
        var item = document.createElement("li");
        item.textContent = line;
        list.append(item);
      });
      body.append(summary, list);
      alert.append(body);
    }
    feedback.replaceChildren(alert);
  }
  // Errors come back as an alert fragment; show only its text.
  function errorText(status, body) {
    var template = document.createElement("template");
    template.innerHTML = body;
    var text = template.content.textContent.trim();
    return text || "Upload failed (HTTP " + status + ").";
  }

  // The notice waits for the folder, so no row from before the upload is on
  // screen once it says the upload finished.
  async function refreshFolder() {
    var list = document.getElementById("file-list");
    if (!list || !window.htmx) return;
    try {
      await window.htmx.ajax("GET", list.getAttribute("hx-get"), { source: list, target: list, swap: "outerMorph" });
    } catch (error) {
      // The notice still shows; the folder's own error replaces the rows.
    }
  }

  function batches(files) {
    var all = [], batch = [], bytes = 0;
    files.forEach(function (file) {
      if (batch.length > 0 && (batch.length >= batchFiles || bytes + file.size > batchBytes)) {
        all.push(batch);
        batch = [];
        bytes = 0;
      }
      batch.push(file);
      bytes += file.size;
    });
    if (batch.length > 0) all.push(batch);
    return all;
  }

  // send uploads one batch, calling progress with the file being sent.
  // fetch can't report upload progress, so this uses XMLHttpRequest.
  function send(batch, progress) {
    return new Promise(function (resolve, reject) {
      var manifest = JSON.stringify({ files: batch.map(function (file) { return { name: file.name, size: file.size }; }) }) + "\n";
      var head = new Blob([manifest]).size;
      var query = new URLSearchParams({ root: page.getAttribute("data-files-root"), path: page.getAttribute("data-files-path") });
      var request = new XMLHttpRequest();
      request.open("PUT", "/files/" + encodeURIComponent(page.getAttribute("data-files")) + "/upload?" + query);
      request.setRequestHeader("Content-Type", "application/octet-stream");
      request.setRequestHeader("X-CSRF-Token", csrf ? csrf.content : "");
      request.setRequestHeader("HX-Request", "true");
      request.upload.addEventListener("progress", function (event) {
        var sent = event.loaded - head, index = 0;
        while (index < batch.length - 1 && sent >= batch[index].size) {
          sent -= batch[index].size;
          index++;
        }
        progress(index);
      });
      request.addEventListener("load", function () { resolve({ status: request.status, body: request.responseText }); });
      request.addEventListener("error", function () { reject(new TypeError("The connection was lost.")); });
      request.addEventListener("abort", function () { reject(new DOMException("Aborted", "AbortError")); });
      controller.signal.addEventListener("abort", function () { request.abort(); }, { once: true });
      request.send(new Blob([manifest].concat(batch), { type: "application/octet-stream" }));
    });
  }

  async function upload(chosen) {
    if (disabled() || chosen.length === 0) return;
    controller = new AbortController();
    var completed = 0, offset = 0, current = chosen[0].name;
    // lines has one entry per file that didn't upload; stop says why the
    // files after offset weren't tried.
    var lines = [], stop = "";
    zone.setAttribute("aria-disabled", "true");
    if (choose) choose.disabled = true;
    progress.hidden = false;
    cancel.disabled = false;
    if (feedback) feedback.replaceChildren();
    var files = chosen.filter(function (file) {
      if (!maxUpload || file.size <= maxUpload) return true;
      lines.push("Could not upload “" + file.name + "”: File exceeds the upload size limit of " + page.getAttribute("data-max-upload-text"));
      return false;
    });
    function report(index) {
      current = files[offset + index].name;
      status.textContent = "Uploading " + (chosen.length - files.length + offset + index + 1) + " of " + chosen.length + ": " + current;
    }
    try {
      var queue = batches(files);
      for (var position = 0; position < queue.length; position++) {
        var batch = queue[position];
        if (controller.signal.aborted) throw new DOMException("Aborted", "AbortError");
        report(0);
        var response = await send(batch, report);
        if (response.status === 401) {
          window.location.assign("/login");
          return;
        }
        // A bare 413 is a body limit, a proxy's or a batch's, not a refused
        // file, which comes in the results: its files go again one at a time.
        if (response.status === 413) {
          if (batch.length > 1) {
            queue.splice(position + 1, 0, ...batch.map(function (file) { return [file]; }));
          } else {
            lines.push("Could not upload “" + batch[0].name + "”: " + errorText(response.status, response.body));
            offset += 1;
          }
          continue;
        }
        // A proxy's error or timeout can come after the server wrote some of
        // the batch.
        if (response.status >= 500 || response.status === 408) {
          stop = "The results for " + (batch.length === 1 ? "“" + batch[0].name + "”" : batch.length + " files from “" + batch[0].name + "”") +
            " could not be confirmed: " + errorText(response.status, response.body) + " Review the folder before uploading again.";
          offset += batch.length;
          break;
        }
        if (response.status !== 200) {
          stop = "Could not upload “" + batch[0].name + "”: " + errorText(response.status, response.body);
          offset += 1;
          break;
        }
        var answer = JSON.parse(response.body), unsure = false;
        answer.results.forEach(function (result) {
          if (result.status === "uploaded") {
            completed += 1;
          } else {
            unsure = unsure || result.status === "unconfirmed";
            lines.push(result.status === "unconfirmed" ? result.message : "Could not upload “" + result.name + "”: " + result.message);
          }
        });
        offset += batch.length;
        // After an unknown result, the rest waits until someone checks the folder.
        if (answer.stop || unsure) break;
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        // Files sent before the one cut off may have been written.
        stop = "The upload stopped at “" + current + "”: " + (error && error.message ? error.message : "Upload failed.") +
          " Review the folder before uploading again.";
        offset = files.length;
      }
    } finally {
      var noun = chosen.length === 1 ? " file" : " files";
      var kind = "error", message = "Uploaded " + completed + " of " + chosen.length + noun + ".";
      if (stop) lines.push(stop);
      var left = files.length - offset;
      if (left > 0) lines.push(left === 1 ? "The remaining file wasn’t uploaded." : "The remaining " + left + " files weren’t uploaded.");
      if (controller.signal.aborted) {
        kind = "success";
        message = chosen.length === 1
          ? "Upload cancelled. “" + current + "” may have been written, and cancelling doesn’t undo it. Check the folder before trying again."
          : "Upload cancelled. " + completed + " of " + chosen.length + " files finished. Check the folder before trying again; " +
            "files sent with “" + current + "” may have been written, and cancelling doesn’t undo them.";
        lines = [];
      } else if (completed === chosen.length) {
        kind = "success";
        message = completed + " file" + (completed === 1 ? "" : "s") + " uploaded.";
      } else if (lines.length === 1) {
        // One line reads as one sentence.
        message += " " + lines.pop();
      }
      status.textContent = "Refreshing folder…";
      cancel.disabled = true;
      await refreshFolder();
      showNotice(kind, message, lines);
      document.dispatchEvent(new CustomEvent("files-updated"));
      controller = null;
      progress.hidden = true;
      zone.setAttribute("aria-disabled", "false");
      if (choose) choose.disabled = false;
      if (input) input.value = "";
    }
  }

  if (choose && input) {
    choose.addEventListener("click", function () { if (!disabled()) input.click(); });
    input.addEventListener("change", function () { upload(Array.from(input.files || [])); });
  }
  cancel.addEventListener("click", function () {
    if (!controller) return;
    status.textContent = "Cancelling upload…";
    cancel.disabled = true;
    controller.abort();
  });
  zone.addEventListener("dragenter", function (event) {
    event.preventDefault();
    if (!disabled()) zone.classList.add("file-dropzone--active");
  });
  zone.addEventListener("dragover", function (event) { event.preventDefault(); });
  zone.addEventListener("dragleave", function (event) {
    if (event.target === zone) zone.classList.remove("file-dropzone--active");
  });
  zone.addEventListener("drop", function (event) {
    event.preventDefault();
    zone.classList.remove("file-dropzone--active");
    upload(Array.from(event.dataTransfer.files || []));
  });
  // Leaving mid-upload would cancel it silently.
  window.addEventListener("beforeunload", function (event) {
    if (controller) event.preventDefault();
  });
})();

// Selecting several entries: checkboxes and the selection bar appear only
// once someone chooses Select, and leave after a change reloads the folder.
(function () {
  var manager = document.querySelector(".file-manager");
  var bar = document.querySelector("[data-selection-bar]");
  if (!manager || !bar) return;
  var count = bar.querySelector("[data-selection-count]");
  function rows() { return Array.from(manager.querySelectorAll("[data-file-select]")); }
  function update() {
    var all = rows();
    var checked = all.filter(function (box) { return box.checked; }).length;
    count.textContent = checked + " selected";
    bar.querySelectorAll("[data-selection-action]").forEach(function (button) {
      button.disabled = checked === 0 || button.hasAttribute("data-selection-blocked");
    });
    var every = manager.querySelector("[data-file-select-all]");
    if (every) {
      every.checked = all.length > 0 && checked === all.length;
      every.indeterminate = checked > 0 && checked < all.length;
    }
  }
  function selecting(on) {
    manager.toggleAttribute("data-selecting", on);
    bar.hidden = !on;
    manager.querySelectorAll("[data-file-select-mode]").forEach(function (button) {
      if (button.hasAttribute("aria-pressed")) button.setAttribute("aria-pressed", on ? "true" : "false");
    });
    if (!on) rows().forEach(function (box) { box.checked = false; });
    update();
    var focus = on ? manager.querySelector("[data-file-select]") : manager.querySelector(".file-toolbar [data-file-select-mode]");
    if (focus) focus.focus();
  }
  manager.addEventListener("click", function (event) {
    var toggle = event.target.closest && event.target.closest("[data-file-select-mode]");
    if (toggle) selecting(!manager.hasAttribute("data-selecting"));
  });
  manager.addEventListener("change", function (event) {
    if (event.target.matches("[data-file-select-all]")) {
      rows().forEach(function (box) { box.checked = event.target.checked; });
    }
    if (event.target.matches("[data-file-select], [data-file-select-all]")) update();
  });
  document.addEventListener("htmx:after:swap", update);
  ["files-changed", "files-updated"].forEach(function (name) {
    document.addEventListener(name, function () {
      if (manager.hasAttribute("data-selecting")) selecting(false);
    });
  });
})();
