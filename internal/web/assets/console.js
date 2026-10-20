// Console page: an xterm view over one WebSocket for Docker logs, a game
// console, or an administrator shell. The server authorizes the socket and
// rechecks access before every frame; this script only displays output and
// sends commands the user typed. Commands are never resent automatically.
"use strict";
(function () {
  var root = document.querySelector("[data-console-terminal]");
  var wrapper = document.querySelector(".console-wrapper");
  if (!root || !wrapper || !window.Terminal) return;

  var socketPath = wrapper.getAttribute("data-console");
  var mode = wrapper.getAttribute("data-console-mode");
  var running = wrapper.hasAttribute("data-running");
  var statusBox = document.querySelector("[data-console-status]");
  var errorBox = document.querySelector("[data-console-error]");
  var retryButton = document.querySelector("[data-console-retry]");
  var pauseButton = document.querySelector("[data-console-pause]");
  var pausedNote = document.querySelector("[data-console-paused-note]");
  var form = document.querySelector("[data-console-form]");
  var input = document.getElementById("console-input");
  var sendButton = document.getElementById("btn-console-send");
  var feedback = document.querySelector("[data-console-feedback]");
  var quickButtons = document.querySelectorAll("[data-console-command]");

  // Terminal, themed from the page's CSS variables.
  var colors = getComputedStyle(document.documentElement);
  var css = function (name, fallback) { return colors.getPropertyValue(name).trim() || fallback; };
  var background = css("--terminal", "#0f1a28");
  var fontFamily = css("--mono", "monospace");
  var terminal = new window.Terminal({
    theme: {
      background: background, foreground: css("--terminal-fg", "#dbe4e8"), cursor: css("--caution", "#e3b20b"),
      cursorAccent: background, selectionBackground: "rgba(227, 178, 11, 0.28)",
      black: background, red: "#f39b92", green: "#92dba0", yellow: "#e8c46b", blue: "#38bdf8", magenta: "#c084fc",
      cyan: "#7cc8e0", white: "#d6d6d6", brightBlack: "#767676", brightRed: "#fca5a5", brightGreen: "#6ee7b7",
      brightYellow: "#fde68a", brightBlue: "#7dd3fc", brightMagenta: "#d8b4fe", brightCyan: "#67e8f9", brightWhite: "#ededed",
    },
    // xterm measures glyph width once, so start on a font that is already present.
    fontFamily: "ui-monospace, monospace",
    fontSize: 14, lineHeight: 1.4, cursorBlink: true, cursorStyle: "bar", scrollback: 5000, convertEol: true, disableStdin: true,
  });
  var fitAddon = new window.FitAddon.FitAddon();
  terminal.loadAddon(fitAddon);
  if (window.WebLinksAddon) terminal.loadAddon(new window.WebLinksAddon.WebLinksAddon());
  terminal.open(root);
  var fit = function () { try { fitAddon.fit(); } catch (error) { /* Hidden terminals cannot be measured. */ } };
  requestAnimationFrame(fit);
  if (window.ResizeObserver) new ResizeObserver(fit).observe(root);
  window.addEventListener("resize", fit);
  if (document.fonts) {
    document.fonts.load("14px " + fontFamily).then(function () {
      terminal.options.fontFamily = fontFamily;
      fit();
    }, function () {});
  }

  function write(raw) {
    var message;
    try { message = JSON.parse(raw); } catch (error) { message = null; }
    if (!message || typeof message.data !== "string") {
      terminal.write("\x1b[31;1m[error] Received an invalid console message\x1b[0m\r\n");
      return;
    }
    switch (message.type) {
      case "stdout": terminal.write(message.data); break;
      case "stderr": terminal.write("\x1b[31m" + message.data + "\x1b[0m"); break;
      case "system": terminal.write("\x1b[36m[system] " + message.data + "\x1b[0m\r\n"); break;
      case "error": terminal.write("\x1b[31;1m[error] " + message.data + "\x1b[0m\r\n"); break;
      default: terminal.write("\x1b[31;1m[error] Received an invalid console message\x1b[0m\r\n");
    }
  }

  // Pausing logs holds output in a bounded buffer.
  var paused = false, held = [], heldChars = 0, dropped = false;
  if (pauseButton) {
    pauseButton.addEventListener("click", function () {
      if (paused) {
        if (dropped) terminal.write("\x1b[33m[system] Some paused output was discarded to limit memory use\x1b[0m\r\n");
        held.splice(0).forEach(write);
        heldChars = 0;
        dropped = false;
      }
      paused = !paused;
      pauseButton.textContent = paused ? "Resume" : "Pause";
      pauseButton.setAttribute("aria-pressed", String(paused));
      pauseButton.classList.toggle("console-pause--active", paused);
      if (pausedNote) pausedNote.hidden = !paused;
    });
  }
  function receive(raw) {
    if (mode === "logs" && paused) {
      held.push(raw);
      heldChars += raw.length;
      while (held.length > 500 || heldChars > 1000000) {
        heldChars -= (held.shift() || "").length;
        dropped = true;
      }
      return;
    }
    write(raw);
  }

  // Connection status and readiness. A socket counts as verified once the
  // server's first message arrives: it is sent only after authorization.
  var state = "disconnected", verified = false, accessDenied = false;
  function showStatus(label, tone) {
    if (!statusBox) return;
    statusBox.className = "connection-status connection-status--" + state;
    var pip = statusBox.querySelector(".status-pip");
    if (pip) pip.className = "status-pip status-pip--" + tone;
    statusBox.querySelector("span:last-child").textContent = label;
  }
  function showError(message) {
    if (!errorBox) return;
    errorBox.textContent = message || "";
    errorBox.hidden = !message;
  }
  function ready() { return state === "connected" && verified && !accessDenied && running && mode !== "logs"; }
  function updateControls() {
    if (sendButton) sendButton.disabled = !ready() || !input || !input.value.trim();
    quickButtons.forEach(function (button) {
      var needsInput = / $/.test(button.getAttribute("data-console-command"));
      button.disabled = needsInput ? !running || accessDenied : !ready();
    });
  }

  var ACCESS_DENIED = [1008, 4001, 4003, 4401, 4403];
  var socket = null, retries = 0, maxRetries = 10, reconnectTimer = null, stableTimer = null;
  function disconnected(code) {
    clearTimeout(reconnectTimer);
    clearTimeout(stableTimer);
    state = "disconnected";
    verified = false;
    if (code !== undefined && ACCESS_DENIED.indexOf(code) >= 0) {
      accessDenied = true;
      showStatus("Access unavailable", "attention");
      showError("Connection access is unavailable. Reload details to check your access before reconnecting.");
      if (retryButton) retryButton.hidden = true;
      root.classList.add("console-terminal--blocked");
    } else if (retries < maxRetries) {
      retries += 1;
      showStatus("Disconnected", "attention");
      showError("Connection lost. Reconnecting…");
      reconnectTimer = setTimeout(connect, 2000);
    } else {
      showStatus("Disconnected", "attention");
      showError("Connection lost. Automatic retries have stopped.");
      if (retryButton) retryButton.hidden = false;
    }
    updateControls();
  }
  function connect() {
    if (!socketPath || accessDenied) return;
    if (retryButton) retryButton.hidden = true;
    state = "connecting";
    verified = false;
    showStatus("Connecting…", "active");
    var url = (location.protocol === "https:" ? "wss://" : "ws://") + location.host + socketPath;
    var ws;
    try { ws = new WebSocket(url); } catch (error) { disconnected(); return; }
    socket = ws;
    ws.onopen = function () {
      if (socket !== ws) return;
      state = "connected";
      showStatus("Checking access…", "active");
      showError("");
      // Only a sustained connection starts a fresh retry budget.
      stableTimer = setTimeout(function () { if (socket === ws) retries = 0; }, 30000);
      updateControls();
    };
    ws.onmessage = function (event) {
      if (socket !== ws || typeof event.data !== "string") return;
      if (!verified) {
        verified = true;
        showStatus("Connected", "ok");
        updateControls();
      }
      receive(event.data);
    };
    ws.onclose = function (event) {
      if (socket !== ws) return;
      socket = null;
      disconnected(event.code);
    };
    ws.onerror = function () {
      // A close event follows; keep its code so access denials never retry.
      try { ws.close(); } catch (error) { if (socket === ws) { socket = null; disconnected(); } }
    };
  }
  if (retryButton) {
    retryButton.addEventListener("click", function () {
      retries = 0;
      connect();
    });
  }
  window.addEventListener("pagehide", function () {
    var ws = socket;
    socket = null;
    if (ws) ws.close(1000);
  });

  // Commands and per-page history; nothing is written to storage.
  var history = [], cursor = null;
  function setFeedback(message, error) {
    if (!feedback) return;
    feedback.textContent = message || "";
    feedback.hidden = !message;
    feedback.classList.toggle("console-recovery--error", Boolean(error));
    feedback.setAttribute("role", error ? "alert" : "status");
  }
  function sendCommand(text, fromDraft) {
    var command = text.trim();
    if (!command || !ready()) return;
    var ws = socket;
    try {
      if (!ws || ws.readyState !== WebSocket.OPEN) throw new Error("closed");
      ws.send(JSON.stringify({ type: "input", data: command }));
      if (ws.readyState !== WebSocket.OPEN) throw new Error("closed");
    } catch (error) {
      setFeedback("The connection could not confirm sending. Your draft is still here. Check the output before trying again.", true);
      return;
    }
    terminal.write("\x1b[33m> " + command + "\x1b[0m\r\n");
    if (history[history.length - 1] !== command) history.push(command);
    if (history.length > 50) history.shift();
    cursor = null;
    if (fromDraft && input) input.value = "";
    setFeedback("Command sent. Check the output for its result.", false);
    updateControls();
  }
  if (form && input) {
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      sendCommand(input.value, true);
    });
    input.addEventListener("input", function () {
      cursor = null;
      setFeedback("");
      updateControls();
    });
    // Up and Down step through commands sent on this page, keeping the draft.
    input.addEventListener("keydown", function (event) {
      if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
      var direction = event.key === "ArrowUp" ? -1 : 1;
      if (history.length === 0 || (!cursor && direction === 1)) return;
      var index = cursor ? cursor.index + direction : history.length - 1;
      event.preventDefault();
      if (index < 0) return;
      var draft = cursor ? cursor.draft : input.value;
      input.value = index >= history.length ? draft : history[index];
      cursor = index >= history.length ? null : { index: index, draft: draft };
      updateControls();
    });
  }
  quickButtons.forEach(function (button) {
    button.addEventListener("click", function () {
      var command = button.getAttribute("data-console-command");
      // A trailing space means the command needs more input, such as a message.
      if (/ $/.test(command) && input) {
        input.value = command;
        cursor = null;
        input.focus();
        input.setSelectionRange(command.length, command.length);
        updateControls();
        return;
      }
      sendCommand(command, false);
    });
  });

  if (socketPath) connect();
  else showStatus("Unavailable", "attention");
  updateControls();
})();
