// Direct connections and the helper use the same checked protocol implementation.
export interface NetworkConsoleControl {
  signal?: AbortSignal;
  /** Ask the backend again before a credential/command write in a helper. */
  authorize?: () => Promise<void>;
}

export interface NetworkConsoleRequest {
  transport: "source-rcon" | "rust-webrcon" | "telnet";
  port: number;
  password: string;
  command: string;
}

/** This factory has no captured values: its JavaScript source also runs in a
 * disposable helper, without depending on files in the selected runtime image. */
export function createNetworkConsoleRuntime() {
  const CONNECT_TIMEOUT_MS = 5_000;
  const COMMAND_TIMEOUT_MS = 10_000;
  const MAX_NETWORK_CONSOLE_BYTES = 4 * 1024 * 1024;
  const MAX_RCON_PACKET_SIZE = MAX_NETWORK_CONSOLE_BYTES;
  const MAX_TCP_RECEIVED_BYTES = MAX_RCON_PACKET_SIZE + 64 * 1024;
  const encodeText = (value: string) => new TextEncoder().encode(value);
  const concatBytes = (chunks: readonly Uint8Array[]) => new Uint8Array(Bun.concatArrayBuffers([...chunks]));
  const byteView = (bytes: Uint8Array) => new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const socketMessageText = (raw: string | ArrayBuffer) => typeof raw === "string" ? raw : new TextDecoder().decode(raw);

  /** No request data is put in Docker configuration or diagnostics. The parent
   * authorizes each network write over this private, attached stdin channel. */
  async function runNetworkConsoleHelper(): Promise<void> {
    // Independent of the parent connection, including a crash before stdin opens.
    setTimeout(() => process.exit(1), 15_000);
    const reader = Bun.stdin.stream().getReader();
    let pending: Uint8Array = new Uint8Array(0);
    const readRecord = async (): Promise<unknown> => {
      while (true) {
        const newline = pending.indexOf(10);
        if (newline !== -1) {
          const line = pending.subarray(0, newline);
          pending = pending.subarray(newline + 1);
          return JSON.parse(new TextDecoder().decode(line)) as unknown;
        }
        const { value, done } = await reader.read();
        if (done) throw new Error("Console parent disconnected");
        pending = concatBytes([pending, value]);
        if (pending.length > 6 * MAX_NETWORK_CONSOLE_BYTES + 65_536)
          throw new Error("Console input exceeded its limit");
      }
    };
    const send = (record: object) => Bun.write(Bun.stdout, JSON.stringify(record) + "\n");
    try {
      const request = await readRecord() as NetworkConsoleRequest;
      if (!request || !["source-rcon", "rust-webrcon", "telnet"].includes(request.transport) ||
          !Number.isInteger(request.port) || request.port < 1 || request.port > 65_535 ||
          typeof request.password !== "string" || typeof request.command !== "string")
        throw new Error("Invalid console request");
      const authorize = async () => {
        await send({ type: "authorize" });
        const reply = await readRecord() as { type?: unknown } | null;
        if (reply?.type !== "authorized") throw new Error("Console access changed");
      };
      await authorize(); // WebRCON sends its credential during the handshake.
      const execute = request.transport === "source-rcon" ? executeSourceRcon
        : request.transport === "rust-webrcon" ? executeRustWebRcon : executeTelnetCommand;
      const response = await execute("127.0.0.1", request.port, request.password, request.command,
        undefined, { authorize });
      await send({ type: "result", response });
      process.exit(0);
    } catch {
      // Never print exceptions: WebSocket diagnostics can contain the password URL.
      await send({ type: "error" });
      process.exit(1);
    }
  }

  async function executeSourceRcon(
    host: string,
    port: number,
    password: string,
    command: string,
    assertAccess?: () => void,
    control: NetworkConsoleControl = {},
  ): Promise<string> {
    assertAccess?.();
    const authId = randomRequestId();
    const commandId = randomRequestId();
    let buffer: Uint8Array = new Uint8Array(0);
    let authenticated = false;
    let commandSent = false;
    const decoder = new TextDecoder("utf-8", { ignoreBOM: true });
    let response = "";
    let responseBytes = 0;
    let receivedResponse = false;
    let responseTimer: ReturnType<typeof setTimeout> | undefined;

    return runConsoleTcp({
      host, port, assertAccess, control,
      connectTimeoutMessage: "RCON connection timed out",
      commandTimeoutMessage: "RCON connection timed out",
      connectionErrorMessage: "Could not connect to the server's RCON endpoint",
      responseLimitMessage: "RCON response is too large",
      open(tcp) {
        tcp.write(encodeRconPacket(authId, 3, password));
      },
      data(tcp, chunk) {
        buffer = concatBytes([buffer, chunk]);
        const decoded = decodeRconPackets(buffer);
        buffer = decoded.remaining;
        for (const packet of decoded.packets) {
          if (tcp.settled) return;
          if (!authenticated) {
            if (packet.id === -1) {
              tcp.finish(new Error("RCON authentication failed"));
              return;
            }
            if (packet.id === authId && packet.type === 2) {
              authenticated = true;
              tcp.write(encodeRconPacket(commandId, 2, command), () => { commandSent = true; });
            }
            continue;
          }
          if (!commandSent || packet.id !== commandId) continue;
          receivedResponse = true;
          responseBytes += packet.body.length;
          if (responseBytes > MAX_RCON_PACKET_SIZE) {
            tcp.finish(new Error("RCON response is too large"));
            return;
          }
          response += decoder.decode(packet.body, { stream: true });
        }
        if (responseTimer) clearTimeout(responseTimer);
        if (receivedResponse && buffer.length === 0)
          responseTimer = setTimeout(() => tcp.finish(undefined, response + decoder.decode()), 150);
      },
      end(tcp) {
        if (!authenticated) tcp.finish(new Error("RCON authentication did not complete"));
        else if (!commandSent) tcp.finish(new Error("RCON connection closed before the command was sent"));
        else if (buffer.length) tcp.finish(new Error("The RCON server returned an incomplete packet"));
        else tcp.finish(undefined, response + decoder.decode());
      },
      cleanup() { if (responseTimer) clearTimeout(responseTimer); },
    });
  }

  async function executeRustWebRcon(
    host: string,
    port: number,
    password: string,
    command: string,
    assertAccess?: () => void,
    control: NetworkConsoleControl = {},
  ): Promise<string> {
    assertAccess?.();
    return new Promise<string>((resolve, reject) => {
      const identifier = randomRequestId();
      const url = `ws://${formatHost(host)}:${port}/${encodeURIComponent(password)}`;
      const socket = new globalThis.WebSocket(url);
      socket.binaryType = "arraybuffer";
      let settled = false;
      const timeout = setTimeout(() => {
        finish(new Error("WebRCON command timed out"));
      }, COMMAND_TIMEOUT_MS);
      const handshakeTimeout = setTimeout(() => {
        finish(new Error("WebRCON connection timed out"));
      }, CONNECT_TIMEOUT_MS);

      const finish = (error?: Error, response = "") => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        clearTimeout(handshakeTimeout);
        control.signal?.removeEventListener("abort", onAbort);
        if (error) socket.terminate();
        else socket.close();
        if (error) reject(error);
        else resolve(response);
      };

      const onAbort = () => finish(new Error("Console command cancelled"));
      control.signal?.addEventListener("abort", onAbort, { once: true });
      if (control.signal?.aborted) { onAbort(); return; }
      socket.addEventListener("open", () => {
        clearTimeout(handshakeTimeout);
        if (settled) return;
        void (async () => {
          try {
            if (control.authorize) await control.authorize();
            if (settled) return;
            assertAccess?.();
            socket.send(
              JSON.stringify({
                Identifier: identifier,
                Message: command,
                Name: "Ludock",
              }),
            );
          } catch {
            finish(new Error("Console access changed"));
          }
        })();
      });
      socket.addEventListener("message", (event) => {
        if (settled) return;
        try {
          const raw: unknown = event.data;
          if (typeof raw !== "string" && !(raw instanceof ArrayBuffer))
            throw new Error("Unsupported WebRCON message");
          if ((typeof raw === "string" ? encodeText(raw).byteLength : raw.byteLength) > MAX_RCON_PACKET_SIZE)
            throw new Error("WebRCON response is too large");
          const message = JSON.parse(socketMessageText(raw)) as {
            Identifier?: unknown;
            Message?: unknown;
          };
          if (message.Identifier !== identifier) return;
          finish(
            undefined,
            typeof message.Message === "string"
              ? message.Message
              : JSON.stringify(message.Message ?? "")
          );
        } catch {
          finish(new Error("The WebRCON server returned an invalid response"));
        }
      });
      socket.addEventListener("error", () => {
        finish(new Error("Could not connect or authenticate with Rust WebRCON"));
      });
      socket.addEventListener("close", () => finish(
        new Error("WebRCON connection closed before the command response"),
      ));
    });
  }

  async function executeTelnetCommand(
    host: string,
    port: number,
    password: string,
    command: string,
    assertAccess?: () => void,
    control: NetworkConsoleControl = {},
  ): Promise<string> {
    assertAccess?.();
    let output = "";
    const decoder = new TextDecoder("utf-8", { ignoreBOM: true });
    let passwordSent = false;
    let commandSent = false;
    let quietTimer: ReturnType<typeof setTimeout> | undefined;
    let commandTimer: ReturnType<typeof setTimeout> | undefined;
    const telnet = createTelnetDecoder();
    const finishResponse = (tcp: ConsoleTcpSession) => {
      tcp.finish(undefined, cleanTelnetOutput(output + decoder.decode(), command));
    };

    return runConsoleTcp({
      host, port, assertAccess, control,
      connectTimeoutMessage: "Telnet console connection timed out",
      commandTimeoutMessage: "Telnet console command timed out",
      connectionErrorMessage: "Could not connect to the server's Telnet console",
      responseLimitMessage: "Telnet console response is too large",
      data(tcp, chunk) {
        output += decoder.decode(telnet(chunk, tcp), { stream: true });
        if (tcp.settled) return;
        if (!passwordSent && /password\s*[:>]?/i.test(output)) {
          passwordSent = true;
          tcp.write(encodeText(`${password}\n`), () => {
            commandTimer = setTimeout(() => {
              if (tcp.settled) return;
              tcp.write(encodeText(`${command}\n`), () => {
                commandSent = true;
                quietTimer = setTimeout(() => finishResponse(tcp), 250);
              });
            }, 50);
          });
          return;
        }
        if (passwordSent) {
          if (/incorrect|invalid password|authentication failed/i.test(output)) {
            tcp.finish(new Error("Telnet console authentication failed"));
            return;
          }
          if (commandSent) {
            if (quietTimer) clearTimeout(quietTimer);
            quietTimer = setTimeout(() => finishResponse(tcp), 250);
          }
        }
      },
      end(tcp) {
        if (commandSent) finishResponse(tcp);
        else tcp.finish(new Error("Telnet console closed before the command was sent"));
      },
      cleanup() {
        if (quietTimer) clearTimeout(quietTimer);
        if (commandTimer) clearTimeout(commandTimer);
      },
    });
  }

  interface ConsoleTcpSession {
    readonly settled: boolean;
    write(data: Uint8Array, written?: () => void): void;
    finish(error?: Error, response?: string): void;
  }

  function runConsoleTcp(options: {
    host: string;
    port: number;
    assertAccess?: () => void;
    control: NetworkConsoleControl;
    connectTimeoutMessage: string;
    commandTimeoutMessage: string;
    connectionErrorMessage: string;
    responseLimitMessage: string;
    open?(tcp: ConsoleTcpSession): void;
    data(tcp: ConsoleTcpSession, chunk: Uint8Array): void;
    end(tcp: ConsoleTcpSession): void;
    cleanup(): void;
  }): Promise<string> {
    return new Promise((resolve, reject) => {
      let socket: Bun.Socket | undefined;
      let settled = false;
      let receivedBytes = 0;
      let queuedBytes = 0;
      let draining = false;
      let blocked = false;
      const writes: Array<{ data: Uint8Array; offset: number; written?: () => void }> = [];
      const connectTimer = setTimeout(() => {
        tcp.finish(new Error(options.connectTimeoutMessage));
      }, CONNECT_TIMEOUT_MS);
      const commandTimer = setTimeout(() => {
        tcp.finish(new Error(options.commandTimeoutMessage));
      }, COMMAND_TIMEOUT_MS);
      const tcp: ConsoleTcpSession = {
        get settled() { return settled; },
        finish(error, response = "") {
          if (settled) return;
          settled = true;
          clearTimeout(connectTimer);
          clearTimeout(commandTimer);
          options.control.signal?.removeEventListener("abort", onAbort);
          options.cleanup();
          writes.length = 0;
          queuedBytes = 0;
          socket?.terminate();
          if (error) reject(error);
          else resolve(response);
        },
        write(data, written) {
          if (settled) return;
          if (queuedBytes + data.length > MAX_RCON_PACKET_SIZE + 4) {
            tcp.finish(new Error("Console command or credential is too large"));
            return;
          }
          writes.push({ data, offset: 0, written });
          queuedBytes += data.length;
          void drainWrites();
        },
      };
      const invoke = (callback: () => void) => {
        if (settled) return;
        try { callback(); }
        catch (error) {
          tcp.finish(error instanceof Error ? error : new Error("The console server returned an invalid response"));
        }
      };
      const drainWrites = async () => {
        if (!socket || settled || draining || blocked) return;
        draining = true;
        try {
          while (writes.length && !settled) {
            const next = writes[0];
            try {
              if (options.control.authorize) await options.control.authorize();
              if (settled) return;
              options.assertAccess?.();
            }
            catch {
              tcp.finish(new Error("Console access changed"));
              return;
            }
            let count: number;
            try { count = socket.write(next.data, next.offset, next.data.length - next.offset); }
            catch {
              tcp.finish(new Error(options.connectionErrorMessage));
              return;
            }
            if (count < 0) {
              tcp.finish(new Error(options.connectionErrorMessage));
              return;
            }
            next.offset += count;
            queuedBytes -= count;
            // Bun does not queue the remaining bytes; drain resumes this exact frame.
            if (next.offset < next.data.length) {
              blocked = true;
              return;
            }
            writes.shift();
            if (next.written) invoke(next.written);
          }
        } finally { draining = false; }
      };
      const onAbort = () => tcp.finish(new Error("Console command cancelled"));
      options.control.signal?.addEventListener("abort", onAbort, { once: true });
      if (options.control.signal?.aborted) { onAbort(); return; }
      try {
        void Bun.connect({
          hostname: options.host,
          port: options.port,
          socket: {
            binaryType: "uint8array",
            open(connected) {
              socket = connected;
              if (settled) { connected.terminate(); return; }
              clearTimeout(connectTimer);
              connected.setNoDelay(true);
              invoke(() => options.open?.(tcp));
            },
            data(_socket, chunk) {
              if (settled) return;
              receivedBytes += chunk.length;
              if (receivedBytes > MAX_TCP_RECEIVED_BYTES) {
                tcp.finish(new Error(options.responseLimitMessage));
                return;
              }
              invoke(() => options.data(tcp, chunk));
            },
            drain() { blocked = false; void drainWrites(); },
            end() { invoke(() => options.end(tcp)); },
            close(_socket, error) {
              if (error) tcp.finish(new Error(options.connectionErrorMessage));
              else invoke(() => options.end(tcp));
            },
            error() { tcp.finish(new Error(options.connectionErrorMessage)); },
            connectError(failed) {
              if (settled) { failed.terminate(); return; }
              socket = failed;
              tcp.finish(new Error(options.connectionErrorMessage));
            },
          },
        }).then((connected) => {
          // A deadline may expire while DNS/connect is still pending.
          if (settled) connected.terminate();
        }, () => tcp.finish(new Error(options.connectionErrorMessage)));
      } catch {
        tcp.finish(new Error(options.connectionErrorMessage));
      }
    });
  }

  function encodeRconPacket(id: number, type: number, body: string): Uint8Array {
    const payload = encodeText(body);
    if (payload.byteLength > MAX_RCON_PACKET_SIZE - 10)
      throw new Error("RCON command or credential is too large");
    const packet = new Uint8Array(payload.length + 14);
    const header = byteView(packet);
    header.setInt32(0, payload.length + 10, true);
    header.setInt32(4, id, true);
    header.setInt32(8, type, true);
    packet.set(payload, 12);
    return packet;
  }

  function decodeRconPackets(buffer: Uint8Array): {
    packets: Array<{ id: number; type: number; body: Uint8Array }>;
    remaining: Uint8Array;
  } {
    const packets: Array<{ id: number; type: number; body: Uint8Array }> = [];
    let offset = 0;
    while (buffer.length - offset >= 4) {
      const size = byteView(buffer).getInt32(offset, true);
      if (size < 10 || size > MAX_RCON_PACKET_SIZE) {
        throw new Error("The RCON server returned an invalid packet");
      }
      if (buffer.length - offset < size + 4) break;
      const end = offset + size + 4;
      packets.push({
        id: byteView(buffer).getInt32(offset + 4, true),
        type: byteView(buffer).getInt32(offset + 8, true),
        body: buffer.subarray(offset + 12, end - 2),
      });
      offset = end;
    }
    return { packets, remaining: buffer.subarray(offset) };
  }

  function randomRequestId(): number {
    return Math.floor(Math.random() * 2_000_000_000) + 1;
  }

  function formatHost(host: string): string {
    return host.includes(":") ? `[${host}]` : host;
  }

  function createTelnetDecoder(): (chunk: Uint8Array, tcp: ConsoleTcpSession) => Uint8Array {
    let state: "text" | "command" | "option" | "subnegotiation" | "subcommand" = "text";
    let command = 0;
    return (chunk, tcp) => {
      const output = new Uint8Array(chunk.length);
      let length = 0;
      for (const byte of chunk) {
        if (tcp.settled) break;
        switch (state) {
          case "text":
            if (byte === 255) state = "command";
            else output[length++] = byte;
            break;
          case "command":
            if (byte === 255) {
              output[length++] = byte;
              state = "text";
            } else if (byte >= 251 && byte <= 254) {
              command = byte;
              state = "option";
            } else state = byte === 250 ? "subnegotiation" : "text";
            break;
          case "option":
            tcp.write(Uint8Array.from([255, command <= 252 ? 254 : 252, byte]));
            state = "text";
            break;
          case "subnegotiation":
            if (byte === 255) state = "subcommand";
            break;
          case "subcommand":
            state = byte === 240 ? "text" : "subnegotiation";
            break;
        }
      }
      return output.subarray(0, length);
    };
  }

  function cleanTelnetOutput(output: string, command: string): string {
    return output
      .replace(/.*password\s*[:>]?\s*/is, "")
      .replace(new RegExp(`^\\s*${escapeRegExp(command)}\\s*`, "i"), "")
      .trim();
  }

  function escapeRegExp(value: string): string {
    return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }

  return { executeSourceRcon, executeRustWebRcon, executeTelnetCommand, runNetworkConsoleHelper, MAX_NETWORK_CONSOLE_BYTES };
}

export const { executeSourceRcon, executeRustWebRcon, executeTelnetCommand, MAX_NETWORK_CONSOLE_BYTES } = createNetworkConsoleRuntime();
