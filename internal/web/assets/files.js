// File uploads: raw PUTs of up to 100 files or 16 MiB each, one after
// another, into the folder shown or, for a chosen or dropped folder, its
// subfolders; a larger file goes alone. The server checks the size limit,
// each name, and access while it writes.
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
  var folderInput = document.querySelector("[data-upload-folder-input]");
  var chooseFolder = document.querySelector("[data-choose-folder]");
  var progress = document.querySelector("[data-upload-progress]");
  var status = document.querySelector("[data-upload-status]");
  var cancel = document.querySelector("[data-upload-cancel]");
  var feedback = document.getElementById("file-feedback");
  var csrf = document.querySelector('meta[name="csrf-token"]');
  var maxUpload = Number(page.getAttribute("data-max-upload")) || 0;
  var batchFiles = 100, batchBytes = 16 * 1024 * 1024;
  // busy holds the uploader from reading what was chosen to the end of the
  // upload; controller cancels the upload.
  var busy = false, controller = null;
  if (chooseFolder && folderInput && "webkitdirectory" in folderInput) chooseFolder.hidden = false;

  function disabled() { return zone.getAttribute("aria-disabled") === "true" || busy; }
  function buttons(off) {
    [choose, chooseFolder].forEach(function (button) { if (button) button.disabled = off; });
  }
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

  // An upload is a list of items: a file and the subfolder of the shown
  // folder it goes in, "" for the folder itself.
  function label(item) { return item.folder ? item.folder + "/" + item.file.name : item.file.name; }
  function inside(folder, name) { return folder ? folder + "/" + name : name; }
  var collator = new Intl.Collator(undefined, { numeric: true });
  // Folders go outermost first and each folder's files together, by name, so
  // a batch is one folder's and its files read in order.
  function ordered(items) {
    return items.slice().sort(function (a, b) {
      if (a.folder !== b.folder) return a.folder < b.folder ? -1 : 1;
      return collator.compare(a.file.name, b.file.name);
    });
  }
  function picked(list, folders) {
    return Array.from(list || []).map(function (file) {
      var path = folders ? file.webkitRelativePath || "" : "";
      return { file: file, folder: path.split("/").slice(0, -1).join("/") };
    });
  }
  function promised(call) { return new Promise(call); }
  // dropped reads what was dropped, walking into folders. A directory reader
  // returns its entries a chunk at a time until one comes back empty.
  async function dropped(entries) {
    var items = [];
    async function walk(entry, folder) {
      if (entry instanceof File) {
        items.push({ file: entry, folder: folder });
        return;
      }
      try {
        if (entry.isFile) {
          items.push({ file: await promised(function (resolve, reject) { entry.file(resolve, reject); }), folder: folder });
          return;
        }
        if (!entry.isDirectory) return;
        var reader = entry.createReader(), children = [];
        for (;;) {
          var chunk = await promised(function (resolve, reject) { reader.readEntries(resolve, reject); });
          if (chunk.length === 0) break;
          children = children.concat(chunk);
        }
        for (var index = 0; index < children.length; index++) await walk(children[index], inside(folder, entry.name));
      } catch (error) {
        throw new Error("Nothing was uploaded: the browser couldn’t read “" + inside(folder, entry.name) + "” from what you dropped.");
      }
    }
    for (var index = 0; index < entries.length; index++) await walk(entries[index], "");
    return items;
  }
  // Without entries a dropped folder looks like a file that can't be read,
  // so a drop with one uploads nothing rather than part of it.
  async function droppedFiles(list) {
    var files = Array.from(list || []);
    for (var index = 0; index < files.length; index++) {
      try {
        await files[index].slice(0, 1).arrayBuffer();
      } catch (error) {
        throw new Error("Nothing was uploaded: this browser can’t upload a dropped folder. Use Choose folder, or drop the files inside it.");
      }
    }
    return picked(files, false);
  }

  function quoted(names) {
    var shown = names.slice(0, 3).map(function (name) { return "“" + name + "”"; });
    if (names.length > 3) return shown.join(", ") + ", and " + (names.length - 3) + " more";
    if (shown.length < 3) return shown.join(" and ");
    return shown[0] + ", " + shown[1] + ", and " + shown[2];
  }
  // listedClashes are the names of chosen files or folders that the shown
  // folder had when last listed. The server refuses to replace a file unless
  // asked to, so this only lets the page ask before sending.
  function listedClashes(items) {
    var list = document.getElementById("file-list"), names = [];
    try {
      names = JSON.parse((list && list.getAttribute("data-names")) || "[]");
    } catch (error) {
      names = [];
    }
    var present = new Set(names), clashes = [];
    items.forEach(function (item) {
      var name = item.folder ? item.folder.split("/")[0] : item.file.name;
      if (present.has(name) && clashes.indexOf(name) < 0) clashes.push(name);
    });
    return clashes;
  }
  // askReplace asks once whether to replace what detail names.
  function askReplace(detail) {
    var dialog = document.getElementById("confirm-dialog");
    if (!dialog || !dialog.showModal) return Promise.resolve(window.confirm(detail));
    dialog.querySelector("h2").textContent = "Replace what’s already here?";
    dialog.querySelector("#confirm-dialog-detail").textContent = detail;
    dialog.querySelector("#confirm-dialog-detail").hidden = false;
    var options = dialog.querySelector("[data-confirm-options]");
    options.replaceChildren();
    options.hidden = true;
    var button = dialog.querySelector("[data-confirm-button]");
    button.textContent = "Upload and replace";
    button.className = "secondary-btn secondary-btn--danger";
    dialog.returnValue = "";
    return new Promise(function (resolve) {
      dialog.addEventListener("close", function () { resolve(dialog.returnValue === "confirm"); }, { once: true });
      dialog.showModal();
    });
  }

  function batches(items) {
    var all = [], batch = [], bytes = 0;
    items.forEach(function (item) {
      if (batch.length > 0 && (batch.length >= batchFiles || bytes + item.file.size > batchBytes || item.folder !== batch[0].folder)) {
        all.push(batch);
        batch = [];
        bytes = 0;
      }
      batch.push(item);
      bytes += item.file.size;
    });
    if (batch.length > 0) all.push(batch);
    return all;
  }

  // send uploads one batch, calling progress with the file being sent. A
  // folder not yet made in this upload is sent as into, which the server
  // creates. fetch can't report upload progress, so this uses XMLHttpRequest.
  function send(batch, made, replace, progress) {
    return new Promise(function (resolve, reject) {
      var manifest = JSON.stringify({ files: batch.map(function (item) { return { name: item.file.name, size: item.file.size }; }) }) + "\n";
      var head = new Blob([manifest]).size;
      var shown = page.getAttribute("data-files-path"), folder = batch[0].folder;
      var query = new URLSearchParams({ root: page.getAttribute("data-files-root"), path: made.has(folder) ? inside(shown, folder) : shown });
      if (folder && !made.has(folder)) query.set("into", folder);
      if (replace) query.set("replace", "1");
      var request = new XMLHttpRequest();
      request.open("PUT", "/files/" + encodeURIComponent(page.getAttribute("data-files")) + "/upload?" + query);
      request.setRequestHeader("Content-Type", "application/octet-stream");
      request.setRequestHeader("X-CSRF-Token", csrf ? csrf.content : "");
      request.setRequestHeader("HX-Request", "true");
      request.upload.addEventListener("progress", function (event) {
        var sent = event.loaded - head, index = 0;
        while (index < batch.length - 1 && sent >= batch[index].file.size) {
          sent -= batch[index].file.size;
          index++;
        }
        progress(index);
      });
      request.addEventListener("load", function () { resolve({ status: request.status, body: request.responseText }); });
      request.addEventListener("error", function () { reject(new TypeError("The connection was lost.")); });
      request.addEventListener("abort", function () { reject(new DOMException("Aborted", "AbortError")); });
      controller.signal.addEventListener("abort", function () { request.abort(); }, { once: true });
      request.send(new Blob([manifest].concat(batch.map(function (item) { return item.file; })), { type: "application/octet-stream" }));
    });
  }

  // cancelled says what a cancelled upload left: the files confirmed, and
  // the batch in flight, any of which the server may have written.
  function cancelled(completed, total, inflight, current) {
    if (total === 1 && inflight) {
      return "Upload cancelled. “" + current + "” may have been written, and cancelling doesn’t undo it. Check the folder before trying again.";
    }
    var message = "Upload cancelled. " + completed + " of " + total + " files are confirmed uploaded";
    if (inflight && inflight.length === 1) {
      message += ", and “" + label(inflight[0]) + "”, which was being sent, may have been written too.";
    } else if (inflight) {
      message += ", and any of the " + inflight.length + " being sent, “" + label(inflight[0]) + "” to “" + label(inflight[inflight.length - 1]) +
        "”, may have been written too.";
    } else {
      message += ".";
    }
    return message + " Cancelling doesn’t undo them; check the folder before trying again.";
  }

  // upload takes a function that reads what was chosen, so nothing else
  // starts while it does.
  async function upload(read) {
    if (disabled()) return;
    busy = true;
    buttons(true);
    var chosen = [], message = "", replace = false;
    try {
      chosen = ordered(await read());
      if (chosen.length === 0) message = "Nothing was uploaded: there were no files in what you chose.";
    } catch (error) {
      message = error.message;
    }
    var clashes = message ? [] : listedClashes(chosen);
    if (clashes.length > 0) {
      replace = await askReplace((clashes.length === 1 ? "1 name you chose is already in this folder: " :
        clashes.length + " names you chose are already in this folder: ") + quoted(clashes) +
        ". Files with the same names are replaced, including inside folders, and that can’t be undone.");
    }
    if (message || (clashes.length > 0 && !replace)) {
      if (message) showNotice("error", message);
      if (input) input.value = "";
      if (folderInput) folderInput.value = "";
      busy = false;
      buttons(false);
      return;
    }
    controller = new AbortController();
    var completed = 0, current = label(chosen[0]), inflight = null, made = new Set();
    // lines has one entry per file that didn't upload; stop says why the
    // last pass's files after its first left untried weren't tried, and left
    // counts those. existing are files the server didn't replace unasked.
    var lines = [], stop = "", left = 0, existing = [];
    zone.setAttribute("aria-disabled", "true");
    buttons(true);
    progress.hidden = false;
    cancel.disabled = false;
    if (feedback) feedback.replaceChildren();
    var files = chosen.filter(function (item) {
      if (!maxUpload || item.file.size <= maxUpload) return true;
      lines.push("Could not upload “" + label(item) + "”: File exceeds the upload size limit of " + page.getAttribute("data-max-upload-text"));
      return false;
    });
    // run sends items in batches, numbering them in the status as the last
    // of total, until a batch's results can't be relied on. It returns false
    // once the session has ended.
    async function run(items, total, verb) {
      var queue = batches(items), offset = 0;
      function report(index) {
        current = label(items[offset + index]);
        status.textContent = verb + " " + (total - items.length + offset + index + 1) + " of " + total + ": " + current;
      }
      try {
        for (var position = 0; position < queue.length; position++) {
          var batch = queue[position];
          if (controller.signal.aborted) throw new DOMException("Aborted", "AbortError");
          report(0);
          inflight = batch;
          var response = await send(batch, made, replace, report);
          inflight = null;
          if (response.status === 401) {
            window.location.assign("/login");
            return false;
          }
          // A bare 413 is a body limit, a proxy's or a batch's, not a refused
          // file, which comes in the results: its files go again one at a time.
          if (response.status === 413) {
            if (batch.length > 1) {
              queue.splice(position + 1, 0, ...batch.map(function (item) { return [item]; }));
            } else {
              lines.push("Could not upload “" + label(batch[0]) + "”: " + errorText(response.status, response.body));
              offset += 1;
            }
            continue;
          }
          // A proxy's error or timeout can come after the server wrote some of
          // the batch.
          if (response.status >= 500 || response.status === 408) {
            stop = "The results for " + (batch.length === 1 ? "“" + label(batch[0]) + "”" : batch.length + " files from “" + label(batch[0]) + "”") +
              " could not be confirmed: " + errorText(response.status, response.body) + " Review the folder before uploading again.";
            offset += batch.length;
            break;
          }
          if (response.status !== 200) {
            stop = "Could not upload “" + label(batch[0]) + "”: " + errorText(response.status, response.body);
            offset += 1;
            break;
          }
          var answer = JSON.parse(response.body), unsure = false;
          answer.results.forEach(function (result, index) {
            if (result.status === "uploaded") {
              completed += 1;
              // A file landed, so its folders exist.
              for (var folder = batch[0].folder; folder; folder = folder.split("/").slice(0, -1).join("/")) made.add(folder);
            } else if (result.status === "exists") {
              existing.push(batch[index]);
            } else {
              unsure = unsure || result.status === "unconfirmed";
              lines.push(result.status === "unconfirmed" ? result.message : "Could not upload “" + label(batch[index]) + "”: " + result.message);
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
          offset = items.length;
        }
      }
      left = items.length - offset;
      return true;
    }
    try {
      if (!(await run(files, chosen.length, "Uploading"))) return;
      // The server found files the folder's listing didn't show, such as
      // ones inside a folder or made since: it asks once more for those.
      if (existing.length > 0 && !stop && !controller.signal.aborted) {
        var again = existing;
        if (await askReplace((again.length === 1 ? "1 file you chose is already here: " : again.length + " files you chose are already here: ") +
          quoted(again.map(label)) + (again.length === 1 ? ". Replacing it can’t be undone; otherwise it stays as it is." :
          ". Replacing them can’t be undone; otherwise they stay as they are."))) {
          existing = [];
          replace = true;
          if (!(await run(again, again.length, "Replacing"))) return;
        }
      }
    } finally {
      var noun = chosen.length === 1 ? " file" : " files";
      var kind = "error", message = "Uploaded " + completed + " of " + chosen.length + noun + ".";
      if (existing.length > 0) {
        lines.push(existing.length === 1 ? "“" + label(existing[0]) + "” wasn’t uploaded: a file with that name is already here." :
          existing.length + " files weren’t uploaded: files with the same names are already here.");
      }
      if (controller.signal.aborted) {
        message = cancelled(completed, chosen.length, inflight, current);
        kind = lines.length > 0 ? "error" : "success";
      } else {
        if (stop) lines.push(stop);
        if (left > 0) lines.push(left === 1 ? "The remaining file wasn’t uploaded." : "The remaining " + left + " files weren’t uploaded.");
        if (completed === chosen.length) {
          kind = "success";
          message = completed + " file" + (completed === 1 ? "" : "s") + " uploaded.";
        } else if (lines.length === 1) {
          // One line reads as one sentence.
          message += " " + lines.pop();
        }
      }
      status.textContent = "Refreshing folder…";
      cancel.disabled = true;
      await refreshFolder();
      showNotice(kind, message, lines);
      document.dispatchEvent(new CustomEvent("files-updated"));
      controller = null;
      busy = false;
      progress.hidden = true;
      zone.setAttribute("aria-disabled", "false");
      buttons(false);
      if (input) input.value = "";
      if (folderInput) folderInput.value = "";
    }
  }

  if (choose && input) {
    choose.addEventListener("click", function () { if (!disabled()) input.click(); });
    input.addEventListener("change", function () { upload(function () { return picked(input.files, false); }); });
  }
  if (chooseFolder && folderInput) {
    chooseFolder.addEventListener("click", function () { if (!disabled()) folderInput.click(); });
    folderInput.addEventListener("change", function () { upload(function () { return picked(folderInput.files, true); }); });
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
    if (disabled()) return;
    // Entries can only be taken while the drop event runs. An item without
    // one, such as a file another app offers, goes as its file.
    var transfer = event.dataTransfer, items = Array.from(transfer.items || []);
    if (items.length > 0 && typeof items[0].webkitGetAsEntry === "function") {
      var entries = items.filter(function (item) { return item.kind === "file"; }).map(function (item) {
        return item.webkitGetAsEntry() || (item.getAsFile && item.getAsFile());
      });
      upload(function () {
        if (entries.some(function (entry) { return !entry; })) throw new Error("Nothing was uploaded: the browser couldn’t read everything you dropped.");
        return dropped(entries);
      });
    } else {
      upload(function () { return droppedFiles(transfer.files); });
    }
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
