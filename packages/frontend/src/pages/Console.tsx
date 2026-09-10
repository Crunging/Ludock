import { serverResponseSchema, consoleMessageSchema } from "@ludock/shared";
import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "@xterm/xterm/css/xterm.css";
import { useWebSocket } from "../hooks/useWebSocket";
import type { ManagedContainer } from "../types";
import { ApiRequestError, apiJson, authenticatedWebSocketUrl } from "../api";
import { useAuth, type AuthUser } from "../auth-context";
import { can } from "../permissions";
import { NavLink } from "../navigation";
import { lifecycleActionForState, lifecycleStateGuidance } from "../server-lifecycle";
import "./console-recovery.css";

type ConsoleMode = "logs" | "game" | "shell";
type ServerLoadState = "loading" | "loaded" | "error";
const MODE_LABELS: Record<ConsoleMode, string> = {
  logs: "Docker Logs",
  game: "Game Console",
  shell: "Container Shell",
};

function allowedModes(user: AuthUser | null, server: ManagedContainer | null): ConsoleMode[] {
  return [
    ...(can(user, server, "logs.read") ? ["logs" as const] : []),
    ...(can(user, server, "console.execute") && server?.gameConsole ? ["game" as const] : []),
    ...(can(user, server, "console.shell") ? ["shell" as const] : []),
  ];
}

function initialMode(modes: ConsoleMode[]): ConsoleMode {
  return modes.includes("game") ? "game" : modes.includes("logs") ? "logs" : "shell";
}

export default function Console({ containerId }: { containerId: string }) {
  const { user } = useAuth();
  return (
    <ConsoleSession
      key={`${containerId}:${user?.id}:${user?.role}`}
      containerId={containerId}
      user={user}
    />
  );
}

function ConsoleSession({ containerId, user }: {
  containerId: string;
  user: AuthUser | null;
}) {
  const [mode, setMode] = useState<ConsoleMode>("logs");
  const modeRef = useRef<ConsoleMode>("logs");
  const [paused, setPaused] = useState(false);
  const termRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const pausedMessagesRef = useRef<string[]>([]);
  const pausedMessageCharsRef = useRef(0);
  const pausedOutputDroppedRef = useRef(false);
  const detailsRequestRef = useRef(0);
  const hasLoadedRef = useRef(false);
  const tabButtonsRef = useRef(new Map<ConsoleMode, HTMLButtonElement>());
  const tabsId = useId();
  // Drafts are local to this mounted console and never written to storage.
  const [drafts, setDrafts] = useState({ game: "", shell: "" });
  const [commandFeedback, setCommandFeedback] = useState<{
    error: boolean;
    message: string;
  } | null>(null);
  const [serverInfo, setServerInfo] = useState<ManagedContainer | null>(null);
  const [serverState, setServerState] = useState<ServerLoadState>("loading");
  const [verifiedUrl, setVerifiedUrl] = useState<string | null>(null);
  const [accessBlocked, setAccessBlocked] = useState(false);
  const modes = allowedModes(user, serverInfo);
  const canReadLogs = modes.includes("logs");
  const canSendGameCommand = modes.includes("game");
  const canSendShellCommand = modes.includes("shell");
  const canSendCommand = mode === "game" ? canSendGameCommand
    : mode === "shell" ? canSendShellCommand : false;
  const command = mode === "logs" ? "" : drafts[mode];
  const isRunning = serverInfo?.state === "running";
  const canStart = can(user, serverInfo, "server.start") &&
    Boolean(serverInfo && lifecycleActionForState(serverInfo.state) === "start");
  const stateGuidance = serverInfo && lifecycleStateGuidance(serverInfo.state);
  const bindingActive = serverInfo?.bindingStatus === "active";
  const detailsPath = `/servers/${encodeURIComponent(containerId)}`;

  const resetOutput = useCallback(() => {
    terminalRef.current?.reset();
    pausedMessagesRef.current = [];
    pausedMessageCharsRef.current = 0;
    pausedOutputDroppedRef.current = false;
    setPaused(false);
    setCommandFeedback(null);
  }, [setCommandFeedback]);

  const selectMode = useCallback((nextMode: ConsoleMode, announce = true) => {
    if (modeRef.current === nextMode) return;
    modeRef.current = nextMode;
    setMode(nextMode);
    setVerifiedUrl(null);
    resetOutput();
    if (announce)
      terminalRef.current?.write(`\x1b[36m[system] Switched to ${MODE_LABELS[nextMode]}\x1b[0m\r\n`);
  }, [resetOutput]);

  const denyAccess = useCallback(() => {
    detailsRequestRef.current += 1;
    setAccessBlocked(true);
    resetOutput();
  }, [resetOutput]);

  const loadDetails = useCallback(async (preserveMode: boolean) => {
    if (!preserveMode) hasLoadedRef.current = false;
    const request = ++detailsRequestRef.current;
    setServerState("loading");
    setServerInfo(null);
    setVerifiedUrl(null);
    try {
      const { server } = await apiJson(
        `/servers/${encodeURIComponent(containerId)}`, serverResponseSchema,
      );
      if (request !== detailsRequestRef.current) return;
      const nextModes = allowedModes(user, server);
      const keepCurrentMode = preserveMode && hasLoadedRef.current;
      selectMode(keepCurrentMode && nextModes.includes(modeRef.current)
        ? modeRef.current : initialMode(nextModes), hasLoadedRef.current);
      hasLoadedRef.current = true;
      setServerInfo(server);
      setServerState("loaded");
      setAccessBlocked(false);
      if (nextModes.length === 0) resetOutput();
    } catch (error) {
      if (request !== detailsRequestRef.current) return;
      if (error instanceof ApiRequestError && [401, 403, 404].includes(error.status)) denyAccess();
      setServerInfo(null);
      setServerState("error");
    }
  }, [containerId, user, denyAccess, resetOutput, selectMode]);

  useEffect(() => {
    setDrafts({ game: "", shell: "" });
    resetOutput();
    void loadDetails(false);
    return () => { detailsRequestRef.current += 1; };
  }, [loadDetails, resetOutput]);

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
    terminal.loadAddon(fitAddon);
    terminal.loadAddon(new WebLinksAddon());
    terminal.open(termRef.current);
    terminalRef.current = terminal;
    const fit = () => fitAddon.fit();
    const initialFit = requestAnimationFrame(fit);
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(fit);
    observer?.observe(termRef.current);
    window.addEventListener("resize", fit);

    return () => {
      cancelAnimationFrame(initialFit);
      observer?.disconnect();
      window.removeEventListener("resize", fit);
      if (terminalRef.current === terminal) terminalRef.current = null;
      terminal.dispose();
    };
  }, []);

  const writeMessage = useCallback((raw: string) => {
    try {
      const msg = consoleMessageSchema.parse(JSON.parse(raw));
      const terminal = terminalRef.current;
      if (!terminal) return;
      switch (msg.type) {
        case "stdout": terminal.write(msg.data); break;
        case "stderr": terminal.write(`\x1b[31m${msg.data}\x1b[0m`); break;
        case "system": terminal.write(`\x1b[36m[system] ${msg.data}\x1b[0m\r\n`); break;
        case "error": terminal.write(`\x1b[31;1m[error] ${msg.data}\x1b[0m\r\n`); break;
      }
    } catch {
      terminalRef.current?.write(
        "\x1b[31;1m[error] Received an invalid console message\x1b[0m\r\n",
      );
    }
  }, []);

  const handleMessage = useCallback((raw: string) => {
    if (mode === "logs" && paused) {
      pausedMessagesRef.current.push(raw);
      pausedMessageCharsRef.current += raw.length;
      while (pausedMessagesRef.current.length > 500 || pausedMessageCharsRef.current > 1_000_000) {
        pausedMessageCharsRef.current -= pausedMessagesRef.current.shift()?.length || 0;
        pausedOutputDroppedRef.current = true;
      }
      return;
    }
    writeMessage(raw);
  }, [mode, paused, writeMessage]);

  const togglePaused = useCallback(() => {
    if (paused) {
      if (pausedOutputDroppedRef.current) {
        terminalRef.current?.write(
          "\x1b[33m[system] Some paused output was discarded to limit memory use\x1b[0m\r\n",
        );
      }
      for (const raw of pausedMessagesRef.current.splice(0)) writeMessage(raw);
      pausedMessageCharsRef.current = 0;
      pausedOutputDroppedRef.current = false;
    }
    setPaused(!paused);
  }, [paused, writeMessage]);

  const wsUrl = serverState !== "loaded" || serverInfo?.id !== containerId || !bindingActive ||
    (mode === "logs" ? !canReadLogs : !canSendCommand || !isRunning)
    ? ""
    : authenticatedWebSocketUrl(
      mode === "logs" ? `/ws/v1/logs/${encodeURIComponent(containerId)}`
        : mode === "game" ? `/ws/v1/game-console/${encodeURIComponent(containerId)}`
          : `/ws/v1/shell/${encodeURIComponent(containerId)}`,
    );

  const verifyConnection = useCallback(() => {
    const request = ++detailsRequestRef.current;
    setVerifiedUrl(null);
    // Socket authorization remains authoritative. This fresh read also keeps
    // controls honest when grants or running state changed while disconnected.
    void apiJson(`/servers/${encodeURIComponent(containerId)}`, serverResponseSchema)
      .then(({ server }) => {
        if (request !== detailsRequestRef.current) return;
        const nextModes = allowedModes(user, server);
        setServerInfo(server);
        selectMode(nextModes.includes(modeRef.current) ? modeRef.current : initialMode(nextModes));
        if (nextModes.length === 0) resetOutput();
        setVerifiedUrl(wsUrl);
      })
      .catch((error) => {
        if (request !== detailsRequestRef.current) return;
        if (error instanceof ApiRequestError && [401, 403, 404].includes(error.status)) denyAccess();
        setVerifiedUrl(null);
        setServerInfo(null);
        setServerState("error");
      });
  }, [containerId, user, wsUrl, denyAccess, resetOutput, selectMode]);

  const { status, send, retry, canRetry, accessDenied, error: connectionError } = useWebSocket({
    url: wsUrl,
    onMessage: handleMessage,
    onOpen: verifyConnection,
  });
  const accessUnavailable = accessDenied || accessBlocked;
  const outputUnavailable = accessUnavailable || (serverState === "loaded" && !modes.includes(mode));

  useEffect(() => {
    if (!accessDenied) return;
    denyAccess();
  }, [accessDenied, denyAccess]);

  const readyToSend = status === "connected" && verifiedUrl === wsUrl &&
    Boolean(wsUrl) && !accessUnavailable && canSendCommand && isRunning && bindingActive;

  const sendCommand = () => {
    const cmd = command.trim();
    if (!cmd || !readyToSend || mode === "logs") return;
    if (!send(JSON.stringify({ type: "input", data: cmd }))) {
      setCommandFeedback({
        error: true,
        message: "The connection could not confirm sending. Your draft is still here. Check the output before trying again.",
      });
      return;
    }
    terminalRef.current?.write(`\x1b[33m> ${cmd}\x1b[0m\r\n`);
    setDrafts((current) => ({ ...current, [mode]: current[mode] === command ? "" : current[mode] }));
    setCommandFeedback({ error: false, message: "Command sent. Check the output for its result." });
  };

  const switchMode = (nextMode: ConsoleMode) => {
    if (mode === nextMode) return;
    detailsRequestRef.current += 1;
    selectMode(nextMode);
  };

  const statusLabel = serverState === "loading" ? "Loading…"
    : serverState === "error" ? "Unavailable"
      : accessUnavailable ? "Access unavailable"
      : !wsUrl ? "Unavailable"
        : status === "connected" ? (verifiedUrl === wsUrl ? "Connected" : "Checking access…")
          : status === "connecting" ? "Connecting…" : "Disconnected";

  return (
    <div className="console-wrapper">
      <div className="console-header">
        <div className="console-header__title">
          <NavLink
            className="console-header__back"
            to={detailsPath}
            title="Back to server details"
            aria-label="Back to server details"
            id="btn-console-back"
          >
            <span aria-hidden="true">←</span>
          </NavLink>
          <div>
            <div className="console-header__name">
              {accessUnavailable ? "Console unavailable" : serverInfo?.displayName || containerId.substring(0, 12)}
            </div>
            <div className="console-header__status">
              {accessUnavailable ? "Reload details to check your access"
                : mode === "logs" ? "Docker stdout and stderr"
                : mode === "game" ? serverInfo?.gameConsole?.name || "Game console"
                  : "Administrator container shell"}
              {!accessUnavailable && serverInfo?.image && ` · ${serverInfo.image}`}
            </div>
          </div>
        </div>
        {!accessUnavailable && serverState === "loaded" && modes.length > 0 && (
          <div className="console-mode-tabs" role="tablist" aria-label="Console mode">
            {modes.map((tabMode, index) => (
              <button
                className={mode === tabMode ? "console-mode-tab--active" : ""}
                key={tabMode}
                id={`${tabsId}-tab-${tabMode}`}
                role="tab"
                aria-selected={mode === tabMode}
                aria-controls={`${tabsId}-panel`}
                tabIndex={mode === tabMode ? 0 : -1}
                ref={(element) => {
                  if (element) tabButtonsRef.current.set(tabMode, element);
                  else tabButtonsRef.current.delete(tabMode);
                }}
                onClick={() => switchMode(tabMode)}
                onKeyDown={(event) => {
                  const nextIndex = event.key === "ArrowRight" ? (index + 1) % modes.length
                    : event.key === "ArrowLeft" ? (index + modes.length - 1) % modes.length
                      : event.key === "Home" ? 0 : event.key === "End" ? modes.length - 1 : null;
                  if (nextIndex === null) return;
                  event.preventDefault();
                  const nextMode = modes[nextIndex];
                  switchMode(nextMode);
                  tabButtonsRef.current.get(nextMode)?.focus();
                }}
              >
                {MODE_LABELS[tabMode]}
              </button>
            ))}
          </div>
        )}
        <div className="console-header__controls">
          {!accessUnavailable && mode === "logs" && canReadLogs && (
            <button
              className={`console-pause ${paused ? "console-pause--active" : ""}`}
              onClick={togglePaused}
              aria-pressed={paused}
              type="button"
            >
              {paused ? "Resume" : "Pause"}
            </button>
          )}
          <button
            className="console-retry"
            onClick={() => void loadDetails(true)}
            disabled={serverState === "loading"}
          >
            Reload details
          </button>
          {!accessUnavailable && canRetry && (
            <button className="console-retry" onClick={() => { setVerifiedUrl(null); retry(); }}>Retry connection</button>
          )}
          <div className={`connection-status connection-status--${status}`} role="status">
            <span className="status-dot" aria-hidden="true" />
            {statusLabel}
          </div>
        </div>
      </div>

      <div
        className={`console-terminal ${outputUnavailable ? "console-terminal--blocked" : ""}`}
        ref={termRef}
        id={`${tabsId}-panel`}
        role="tabpanel"
        aria-labelledby={!accessUnavailable && modes.includes(mode) ? `${tabsId}-tab-${mode}` : undefined}
        aria-label={!accessUnavailable && modes.includes(mode) ? undefined : "Console output"}
        aria-hidden={outputUnavailable || undefined}
        tabIndex={outputUnavailable ? -1 : 0}
      />

      {connectionError && !accessUnavailable && (
        <div className="console-recovery console-recovery--error" role="alert">
          {connectionError}
        </div>
      )}
      {serverState === "loading" ? (
        <div className="console-warning">Loading server details…</div>
      ) : serverState === "error" ? (
        <div className="console-recovery console-recovery--error" role="alert">
          Server details could not be loaded. Reload details to check your access and reconnect.
        </div>
      ) : accessUnavailable ? (
        <div className="console-recovery console-recovery--error" role="alert">
          Connection access is unavailable. Reload details to check your access before reconnecting.
        </div>
      ) : modes.length === 0 ? (
        <div className="console-warning" role="alert">
          You do not have console or log access to this server.
        </div>
      ) : !bindingActive ? (
        <div className="console-warning" role="alert">
          Server identity requires administrator review.{" "}
          <NavLink className="text-link" to={detailsPath}>
            Open server details
          </NavLink>
        </div>
      ) : mode === "logs" ? (
        <div className="console-warning">
          Read-only Docker output · latest 500 lines · timestamps enabled
          {paused ? " · live display paused" : ""}
        </div>
      ) : (
        <>
          <div className="console-warning">
            {!isRunning
              ? `Server state: ${serverInfo?.state}. Commands require a running server.`
              : mode === "game"
                ? `Enter commands exactly as ${serverInfo?.gameConsole?.name || "the game console"} expects them.`
                : "Advanced access: commands run as a new process inside the container."}
            {" "}Commands are never resent automatically.
            {!isRunning && (
              <>
                {stateGuidance && (
                  <p className="console-warning__guidance">
                    {stateGuidance} Reload details after the state changes.
                  </p>
                )}
                <div className="console-warning__actions">
                  {canReadLogs && (
                    <button
                      className="console-retry"
                      onClick={() => {
                        switchMode("logs");
                        tabButtonsRef.current.get("logs")?.focus();
                      }}
                    >
                      View logs
                    </button>
                  )}
                  <NavLink className="text-link" to={detailsPath}>
                    {canStart ? "Open server controls" : "Open server details"}
                  </NavLink>
                </div>
              </>
            )}
          </div>
          {commandFeedback && (
            <div
              className={`console-recovery ${commandFeedback.error ? "console-recovery--error" : ""}`}
              role={commandFeedback.error ? "alert" : "status"}
            >
              {commandFeedback.message}
            </div>
          )}
          <form className="console-input" onSubmit={(event) => { event.preventDefault(); sendCommand(); }}>
            <span className="console-input__prompt" aria-hidden="true">{">"}</span>
            <input
              className="console-input__field"
              type="text"
              value={command}
              onChange={(event) => {
                const value = event.target.value;
                setDrafts((current) => ({ ...current, [mode]: value }));
                setCommandFeedback(null);
              }}
              aria-label={mode === "game" ? "Game command" : "Shell command"}
              placeholder={mode === "game"
                ? serverInfo?.gameConsole?.commandPlaceholder || "Enter a game command…"
                : "Run a container shell command…"}
              disabled={!canSendCommand}
              maxLength={mode === "game" ? 1024 : 4096}
              autoComplete="off"
              spellCheck={false}
              id="console-input"
            />
            <button
              className="console-input__send"
              disabled={!readyToSend || !command.trim()}
              id="btn-console-send"
              type="submit"
            >
              Send
            </button>
          </form>
        </>
      )}
    </div>
  );
}
