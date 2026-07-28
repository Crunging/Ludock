import { useCallback, useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "@xterm/xterm/css/xterm.css";
import { useWebSocket } from "../hooks/useWebSocket";
import type { ConsoleMessage, ManagedContainer } from "../types";
import { apiFetch, authenticatedWebSocketUrl } from "../api";
import { useAuth } from "../auth-context";
import { useNavigate } from "../navigation-context";

export default function Console({ containerId }: { containerId: string }) {
  const navigate = useNavigate();
  const { user } = useAuth();
  const readOnly = user?.role === "viewer";
  const [mode, setMode] = useState<"game" | "shell">("game");
  const termRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitAddonRef = useRef<FitAddon | null>(null);
  const [command, setCommand] = useState("");
  const [serverInfo, setServerInfo] = useState<ManagedContainer | null>(null);
  const gameConsoleAvailable = Boolean(serverInfo?.gameConsole);
  const canSendGameCommand = !readOnly && gameConsoleAvailable;
  const canSendShellCommand = user?.role === "admin";
  const canSendCommand =
    mode === "game" ? canSendGameCommand : canSendShellCommand;

  useEffect(() => {
    apiFetch(`/api/servers/${encodeURIComponent(containerId)}`)
      .then(async (response) => {
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json() as Promise<{ server: ManagedContainer }>;
      })
      .then(({ server }) => setServerInfo(server))
      .catch(() => setServerInfo(null));
  }, [containerId]);

  useEffect(() => {
    if (!termRef.current) return;

    const terminal = new Terminal({
      theme: {
        background: "#0d1117",
        foreground: "#c9d1d9",
        cursor: "#58a6ff",
        cursorAccent: "#0d1117",
        selectionBackground: "rgba(56, 189, 248, 0.25)",
        black: "#0d1117",
        red: "#f87171",
        green: "#34d399",
        yellow: "#fbbf24",
        blue: "#38bdf8",
        magenta: "#c084fc",
        cyan: "#22d3ee",
        white: "#c9d1d9",
        brightBlack: "#6e7681",
        brightRed: "#fca5a5",
        brightGreen: "#6ee7b7",
        brightYellow: "#fde68a",
        brightBlue: "#7dd3fc",
        brightMagenta: "#d8b4fe",
        brightCyan: "#67e8f9",
        brightWhite: "#f1f5f9",
      },
      fontFamily: "'JetBrains Mono', 'Fira Code', monospace",
      fontSize: 14,
      lineHeight: 1.4,
      cursorBlink: true,
      cursorStyle: "bar",
      scrollback: 5000,
      convertEol: true,
      disableStdin: true,
    });

    const fitAddon = new FitAddon();
    const webLinksAddon = new WebLinksAddon();

    terminal.loadAddon(fitAddon);
    terminal.loadAddon(webLinksAddon);
    terminal.open(termRef.current);

    requestAnimationFrame(() => fitAddon.fit());

    terminalRef.current = terminal;
    fitAddonRef.current = fitAddon;

    const handleResize = () => fitAddon.fit();
    window.addEventListener("resize", handleResize);

    return () => {
      window.removeEventListener("resize", handleResize);
      terminal.dispose();
    };
  }, []);

  const handleMessage = useCallback((raw: string) => {
    try {
      const msg: ConsoleMessage = JSON.parse(raw);
      const terminal = terminalRef.current;
      if (!terminal) return;

      switch (msg.type) {
        case "stdout":
          terminal.write(msg.data);
          break;
        case "stderr":
          terminal.write(`\x1b[31m${msg.data}\x1b[0m`);
          break;
        case "system":
          terminal.write(`\x1b[36m[system] ${msg.data}\x1b[0m\r\n`);
          break;
        case "error":
          terminal.write(`\x1b[31;1m[error] ${msg.data}\x1b[0m\r\n`);
          break;
      }
    } catch {
      terminalRef.current?.write(
        "\x1b[31;1m[error] Received an invalid console message\x1b[0m\r\n"
      );
    }
  }, []);

  const wsUrl = authenticatedWebSocketUrl(
    mode === "game"
      ? `/ws/game-console/${containerId}`
      : `/ws/shell/${containerId}`
  );

  const { status, send } = useWebSocket({
    url: wsUrl,
    onMessage: handleMessage,
  });

  const sendCommand = useCallback(() => {
    const cmd = command.trim();
    if (!cmd) return;

    terminalRef.current?.write(`\x1b[33m> ${cmd}\x1b[0m\r\n`);
    send(JSON.stringify({ type: "input", data: cmd }));
    setCommand("");
  }, [command, send]);

  const switchMode = useCallback((nextMode: "game" | "shell") => {
    setMode(nextMode);
    setCommand("");
    terminalRef.current?.write(
      `\r\n\x1b[36m[system] Switched to ${
        nextMode === "game" ? "game console" : "container shell"
      }\x1b[0m\r\n`
    );
  }, []);

  const statusLabel =
    status === "connected"
      ? "Connected"
      : status === "connecting"
        ? "Connecting..."
        : "Disconnected";

  return (
    <div className="console-wrapper">
      <div className="console-header">
        <div className="console-header__title">
          <button
            className="console-header__back"
            onClick={() => navigate("/")}
            title="Back to Dashboard"
            id="btn-console-back"
          >
            ←
          </button>
          <div>
            <div className="console-header__name">
              {serverInfo?.displayName || containerId.substring(0, 12)}
            </div>
            <div className="console-header__status">
              {mode === "game"
                ? serverInfo?.gameConsole?.name || "Container logs"
                : "Administrator container shell"}{" "}
              · {serverInfo?.image || "Loading..."}
            </div>
          </div>
        </div>
        {user?.role === "admin" && (
          <div className="console-mode-tabs" role="tablist" aria-label="Console mode">
            <button
              className={mode === "game" ? "console-mode-tab--active" : ""}
              onClick={() => switchMode("game")}
              role="tab"
              aria-selected={mode === "game"}
            >
              Game Console
            </button>
            <button
              className={mode === "shell" ? "console-mode-tab--active" : ""}
              onClick={() => switchMode("shell")}
              role="tab"
              aria-selected={mode === "shell"}
            >
              Container Shell
            </button>
          </div>
        )}
        <div className={`connection-status connection-status--${status}`}>
          <span className="status-dot" />
          {statusLabel}
        </div>
      </div>

      <div className="console-terminal" ref={termRef} />

      {mode === "game" && readOnly ? (
        <div className="console-warning">Your account has read-only log access.</div>
      ) : mode === "game" && !gameConsoleAvailable ? (
        <div className="console-warning">
          No game console adapter is configured for this server. Live logs are
          still available.
        </div>
      ) : (
        <>
          <div className="console-warning">
            {mode === "game"
              ? `Commands are sent directly through ${
                  serverInfo?.gameConsole?.name || "the game console"
                }. Enter each command exactly as the game expects it.`
              : "Advanced access: commands run as a new process inside the container."}
          </div>
          <div className="console-input">
            <span className="console-input__prompt">{">"}</span>
            <input
              className="console-input__field"
              type="text"
              value={command}
              onChange={(e) => setCommand(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") sendCommand();
              }}
              placeholder={
                mode === "game"
                  ? serverInfo?.gameConsole?.commandPlaceholder ||
                    "Enter a game command..."
                  : "Run a container shell command..."
              }
              disabled={status !== "connected" || !canSendCommand}
              id="console-input"
              autoFocus
            />
            <button
              className="console-input__send"
              onClick={sendCommand}
              disabled={
                status !== "connected" || !canSendCommand || !command.trim()
              }
              id="btn-console-send"
            >
              Send
            </button>
          </div>
        </>
      )}
    </div>
  );
}
