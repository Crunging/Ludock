// File uploads: one raw PUT per file, in order, into the folder shown.
// The server checks the size limit, the name, and access while it writes.
"use strict";
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
  var controller = null;

  function disabled() { return zone.getAttribute("aria-disabled") === "true" || controller !== null; }
  function show(kind, message) {
    if (!feedback) return;
    var alert = document.createElement("div");
    alert.className = "alert alert--" + kind;
    alert.setAttribute("role", kind === "error" ? "alert" : "status");
    alert.textContent = message;
    feedback.replaceChildren(alert);
  }
  // Errors come back as an alert fragment; show only its text.
  function errorText(response, body) {
    var template = document.createElement("template");
    template.innerHTML = body;
    var text = template.content.textContent.trim();
    return text || "Upload failed (HTTP " + response.status + ").";
  }

  async function upload(files) {
    if (disabled() || files.length === 0) return;
    controller = new AbortController();
    var completed = 0, current = files[0].name;
    zone.setAttribute("aria-disabled", "true");
    if (choose) choose.disabled = true;
    progress.hidden = false;
    cancel.disabled = false;
    if (feedback) feedback.replaceChildren();
    try {
      for (var index = 0; index < files.length; index++) {
        var file = files[index];
        current = file.name;
        if (controller.signal.aborted) throw new DOMException("Aborted", "AbortError");
        status.textContent = "Uploading " + (completed + 1) + " of " + files.length + ": " + file.name;
        if (maxUpload && file.size > maxUpload) {
          throw new Error("File exceeds the upload size limit of " + page.getAttribute("data-max-upload-text"));
        }
        var query = new URLSearchParams({ root: page.getAttribute("data-files-root"), path: page.getAttribute("data-files-path"), name: file.name });
        var response = await fetch("/files/" + encodeURIComponent(page.getAttribute("data-files")) + "/upload?" + query, {
          method: "PUT",
          headers: { "Content-Type": "application/octet-stream", "X-CSRF-Token": csrf ? csrf.content : "", "HX-Request": "true" },
          body: file,
          signal: controller.signal,
          credentials: "same-origin",
        });
        if (response.status === 401) {
          window.location.assign("/login");
          return;
        }
        if (!response.ok) throw new Error(errorText(response, await response.text()));
        completed += 1;
      }
      show("success", completed + " file" + (completed === 1 ? "" : "s") + " uploaded.");
    } catch (error) {
      if (controller.signal.aborted) {
        show("success", "Upload canceled. " + completed + " of " + files.length + " files confirmed complete. Check “" + current +
          "” before retrying; canceling does not undo data already written.");
      } else {
        show("error", "Uploaded " + completed + " of " + files.length + " files. Could not upload “" + current + "”: " +
          (error && error.message ? error.message : "Upload failed."));
      }
    } finally {
      status.textContent = "Refreshing folder…";
      cancel.disabled = true;
      if (window.htmx) window.htmx.trigger(document.body, "files-changed");
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
    status.textContent = "Canceling upload…";
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
