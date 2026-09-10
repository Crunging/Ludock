import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { afterEach, describe, it, spyOn } from "bun:test";
import type Docker from "dockerode";
import { serve, type Socket, type SocketHandler } from "bun";
import {
  executeGameCommand,
  executeRustWebRcon,
  executeSourceRcon,
  executeTelnetCommand,
} from "../src/game-console-runtime.js";
import type { GameConsoleAdapter } from "../src/game-console.js";

const closers: Array<() => Promise<void>> = [];

afterEach(async () => {
  await Promise.all(closers.splice(0).map((close) => close()));
});

describe("Source RCON transport", () => {
  it("does not send credentials when access changes while connecting", async () => {
    let accessChecks = 0;
    const received: Buffer[] = [];
    const port = listen({ data(_socket, chunk) { received.push(Buffer.from(chunk)); } });

    await assert.rejects(
      executeSourceRcon("127.0.0.1", port, "credential", "stop", () => {
        if (++accessChecks > 1) throw new Error("Access revoked");
      }),
      /Console access changed/,
    );
    assert.equal(accessChecks, 2);
    assert.deepEqual(received, []);
  });

  it("does not send a command after access is revoked during authentication", async () => {
    let allowed = true;
    const received: string[] = [];
    const port = listenRcon((socket, packet) => {
      received.push(packet.body.toString());
      allowed = false;
      socket.write(encodePacket(packet.id, 2, ""));
    });
    await assert.rejects(executeSourceRcon("127.0.0.1", port, "credential", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    }), /Console access changed/);
    assert.deepEqual(received, ["credential"]);
  });

  it("does not report success when the connection closes before authentication", async () => {
    const port = listenRcon((socket) => { socket.end(); });
    await assert.rejects(executeSourceRcon("127.0.0.1", port, "credential", "stop"), /authentication did not complete/);
  });
  it("authenticates and returns a native command response", async () => {
    const received: string[] = [];
    const port = listenRcon((socket, { id, type, body }) => {
      received.push(body.toString());
      socket.write(
        encodePacket(id, type === 3 ? 2 : 0, type === 3 ? "" : "3 players"),
      );
    });

    const response = await executeSourceRcon(
      "127.0.0.1",
      port,
      "correct horse",
      "status"
    );

    assert.deepEqual(received, ["correct horse", "status"]);
    assert.equal(response, "3 players");
  });

  it("reports rejected credentials without exposing the password", async () => {
    const port = listenRcon((socket) => socket.write(encodePacket(-1, 2, "")));

    await assert.rejects(
      executeSourceRcon("127.0.0.1", port, "do-not-leak", "status"),
      (error: Error) => {
        assert.match(error.message, /authentication failed/i);
        assert.doesNotMatch(error.message, /do-not-leak/);
        return true;
      }
    );
  });

  it("reassembles fragmented packets and UTF-8 split between response packets", async () => {
    const expected = "Players: José 🐉";
    const bytes = Buffer.from(expected);
    const split = bytes.indexOf(Buffer.from("🐉")) + 2;
    const port = listenRcon((socket, { id, type }) => {
      if (type === 3) {
        const packet = encodePacket(id, 2, "");
        sendFragments(socket, [packet.subarray(0, 2), packet.subarray(2, 7), packet.subarray(7)]);
      } else {
        const packets = Buffer.concat([
          encodePacket(id + 1, 0, "Unrelated reply"),
          encodePacket(id, 0, bytes.subarray(0, split)),
          encodePacket(id, 0, bytes.subarray(split)),
        ]);
        sendFragments(socket, [packets.subarray(0, 3), packets.subarray(3, 17), packets.subarray(17)]);
      }
    });

    assert.equal(await executeSourceRcon("127.0.0.1", port, "credential", "status"), expected);
  });

  it("rejects an invalid packet length without exposing received content", async () => {
    const port = listenRcon((socket) => {
      const packet = Buffer.from("do-not-leak");
      packet.writeInt32LE(4 * 1024 * 1024 + 1);
      socket.write(packet);
    });
    await assert.rejects(executeSourceRcon("127.0.0.1", port, "credential", "status"), (error: Error) => {
      assert.match(error.message, /invalid packet/);
      assert.doesNotMatch(error.message, /do-not-leak|credential/);
      return true;
    });
  });

  it("rejects an incomplete packet after a complete response instead of returning partial success", async () => {
    const port = listenRcon((socket, { id, type }) => {
      if (type === 3) socket.write(encodePacket(id, 2, ""));
      else {
        socket.write(Buffer.concat([
          encodePacket(id, 0, "Partial response"),
          encodePacket(id, 0, "unfinished").subarray(0, 15),
        ]));
        const timer = setTimeout(() => socket.end(), 180);
        closers.push(async () => { clearTimeout(timer); });
      }
    });
    await assert.rejects(executeSourceRcon("127.0.0.1", port, "credential", "status"), /incomplete packet/);
  });

  it("reports refused connections without exposing the password", async () => {
    const port = closedPort();
    await assert.rejects(executeSourceRcon("127.0.0.1", port, "do-not-leak", "status"), (error: Error) => {
      assert.match(error.message, /Could not connect/);
      assert.doesNotMatch(error.message, /do-not-leak/);
      return true;
    });
  });
});

describe("Rust WebRCON transport", () => {
  it("rejects malformed responses without exposing connection credentials", async () => {
    const server = serve({
      hostname: "127.0.0.1", port: 0,
      fetch(request, server) {
        return server.upgrade(request) ? undefined : new Response(null, { status: 400 });
      },
      websocket: { message(socket) { socket.send("invalid credential-response"); } },
    });
    closers.push(() => server.stop(true));
    await assert.rejects(executeRustWebRcon("127.0.0.1", server.port!, "do-not-leak", "status"), (error: Error) => {
      assert.match(error.message, /invalid response/);
      assert.doesNotMatch(error.message, /do-not-leak|credential-response/);
      return true;
    });
  });

  it("does not send a command after access changes during the WebSocket handshake", async () => {
    let allowed = true;
    let sent = false;
    const server = serve({
      hostname: "127.0.0.1", port: 0,
      fetch(request, server) {
        return server.upgrade(request) ? undefined : new Response(null, { status: 400 });
      },
      websocket: {
        open() { allowed = false; },
        message() { sent = true; },
      },
    });
    closers.push(() => server.stop(true));
    const port = server.port!;
    await assert.rejects(executeRustWebRcon("127.0.0.1", port, "credential", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    }), /Console access changed/);
    assert.equal(sent, false);
  });
  it("uses the WebRCON request envelope and matches its response", async () => {
    let requestPath = "";
    const server = serve({
      hostname: "127.0.0.1", port: 0,
      fetch(request, server) {
        requestPath = new URL(request.url).pathname;
        return server.upgrade(request) ? undefined : new Response(null, { status: 400 });
      },
      websocket: {
        message(socket, raw) {
          const message = JSON.parse(raw.toString()) as {
            Identifier: number;
            Message: string;
            Name: string;
          };
          assert.equal(message.Message, "server.save");
          assert.equal(message.Name, "Ludock");
          socket.send(JSON.stringify({ Identifier: message.Identifier + 1, Message: "Unrelated event" }));
          socket.send(new TextEncoder().encode(JSON.stringify({ Identifier: message.Identifier, Message: "Saved", Name: "WebRcon" })));
        },
      },
    });
    closers.push(() => server.stop(true));
    const port = server.port!;

    const response = await executeRustWebRcon(
      "127.0.0.1",
      port,
      "a password",
      "server.save"
    );

    assert.equal(response, "Saved");
    assert.equal(requestPath, "/a%20password");
  });
});

describe("Telnet console transport", () => {
  it("does not send credentials when access changes before the password prompt", async () => {
    let accessChecks = 0;
    const received: Buffer[] = [];
    const port = listen({
      open(socket) { socket.write("Password: "); },
      data(_socket, chunk) { received.push(Buffer.from(chunk)); },
    });
    await assert.rejects(executeTelnetCommand("127.0.0.1", port, "credential", "stop", () => {
      if (++accessChecks > 1) throw new Error("Access revoked");
    }), /Console access changed/);
    assert.equal(accessChecks, 2);
    assert.deepEqual(received, []);
  });

  it("does not send a command after access changes during password authentication", async () => {
    let allowed = true;
    const received: string[] = [];
    const port = listen({
      open(socket) { socket.write("Password: "); },
      data(socket, chunk) {
        received.push(chunk.toString().trim());
        allowed = false;
        socket.write("Logged in\n");
      },
    });
    await assert.rejects(executeTelnetCommand("127.0.0.1", port, "credential", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    }), /Console access changed/);
    assert.deepEqual(received, ["credential"]);
  });
  it("authenticates and sends a command after the password prompt", async () => {
    const received: string[] = [];
    const port = listenTelnet((socket, value) => {
      received.push(value);
      socket.write(value === "telnet-secret" ? "Log on successful.\r\n" : "PlayerOne, PlayerTwo\r\n");
    });

    const response = await executeTelnetCommand(
      "127.0.0.1",
      port,
      "telnet-secret",
      "listplayers"
    );

    assert.deepEqual(received, ["telnet-secret", "listplayers"]);
    assert.match(response, /PlayerOne, PlayerTwo/);
    assert.doesNotMatch(response, /telnet-secret/);
  });

  it("preserves UTF-8 characters split between TCP chunks", async () => {
    const expected = "José 🐉";
    const bytes = Buffer.from(expected);
    const port = listenTelnet((socket, line) => {
      if (line === "credential") socket.write("Logged in\r\n");
      else sendFragments(socket, [bytes.subarray(0, 4), bytes.subarray(4, 8), bytes.subarray(8)]);
    });

    const response = await executeTelnetCommand("127.0.0.1", port, "credential", "status");
    assert.match(response, /José 🐉/);
    assert.doesNotMatch(response, /�/);
  });

  it("handles fragmented Telnet negotiation before a fragmented password prompt", async () => {
    const received: Buffer[] = [];
    let input = Buffer.alloc(0);
    const port = listen({
      open(socket) {
        sendFragments(socket, [
          Buffer.from([255]), Buffer.from([251]), Buffer.from([1]),
          Buffer.from("Pass"), Buffer.from("word: "),
        ]);
      },
      data(socket, chunk) {
        received.push(Buffer.from(chunk));
        input = Buffer.concat([input, chunk]);
        if (input.includes(Buffer.from("status\n"))) socket.end("Ready\r\n");
      },
    });
    assert.equal(await executeTelnetCommand("127.0.0.1", port, "credential", "status"), "Ready");
    assert.deepEqual(Buffer.concat(received), Buffer.concat([
      Buffer.from([255, 254, 1]), Buffer.from("credential\nstatus\n"),
    ]));
  });

  it("reports rejected credentials and never sends the command", async () => {
    const received: string[] = [];
    const port = listenTelnet((socket, line) => {
      received.push(line);
      socket.write("Authentication failed\r\n");
    });
    await assert.rejects(executeTelnetCommand("127.0.0.1", port, "do-not-leak", "stop"), (error: Error) => {
      assert.match(error.message, /authentication failed/);
      assert.doesNotMatch(error.message, /do-not-leak/);
      return true;
    });
    assert.deepEqual(received, ["do-not-leak"]);
  });

  it("rejects a disconnect before the delayed command and cancels its write", async () => {
    const received: string[] = [];
    const port = listenTelnet((socket, line) => {
      received.push(line);
      socket.end();
    });
    await assert.rejects(executeTelnetCommand("127.0.0.1", port, "credential", "stop"), /closed before the command was sent/);
    await Bun.sleep(70);
    assert.deepEqual(received, ["credential"]);
  });

  it("reports a connection failure without exposing the password", async () => {
    const port = closedPort();
    await assert.rejects(executeTelnetCommand("127.0.0.1", port, "do-not-leak", "status"), (error: Error) => {
      assert.match(error.message, /Could not connect/);
      assert.doesNotMatch(error.message, /do-not-leak/);
      return true;
    });
  });
});

describe("Native TCP connection lifetime", () => {
  it("retries partial and zero-byte writes without dropping or duplicating RCON bytes", async () => {
    const connection = controlledConnection([5, 0]);
    const pending = executeSourceRcon("127.0.0.1", 12345, "credential", "status");
    connection.open();
    connection.drain();
    connection.drain();
    const auth = Buffer.concat(connection.accepted);
    assert.deepEqual(auth, encodePacket(auth.readInt32LE(4), 3, "credential"));

    connection.accepted.length = 0;
    connection.writeSizes.push(3, 0);
    connection.receive(encodePacket(auth.readInt32LE(4), 2, ""));
    connection.drain();
    connection.drain();
    const command = Buffer.concat(connection.accepted);
    assert.deepEqual(command, encodePacket(command.readInt32LE(4), 2, "status"));
    connection.receive(encodePacket(command.readInt32LE(4), 0, "Ready"));
    connection.end();
    assert.equal(await pending, "Ready");
    assert.equal(connection.terminated, true);
  });

  it("discards queued credential bytes when access changes before drain", async () => {
    let allowed = true;
    const connection = controlledConnection([12, 0]);
    const pending = executeSourceRcon("127.0.0.1", 12345, "do-not-leak", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    });
    connection.open();
    const sentBeforeRevocation = Buffer.concat(connection.accepted);
    assert.equal(sentBeforeRevocation.length, 12);
    allowed = false;
    connection.drain();
    await assert.rejects(pending, /Console access changed/);
    connection.drain();
    assert.deepEqual(Buffer.concat(connection.accepted), sentBeforeRevocation);
    assert.equal(connection.terminated, true);
  });

  it("keeps Telnet negotiation and credentials in order across partial writes", async () => {
    const connection = controlledConnection([1, 0]);
    const pending = executeTelnetCommand("127.0.0.1", 12345, "credential", "status");
    connection.open();
    connection.receive(Buffer.concat([Buffer.from([255, 251, 1]), Buffer.from("Password: ")]));
    connection.drain();
    connection.drain();
    await Bun.sleep(70);
    assert.deepEqual(Buffer.concat(connection.accepted), Buffer.concat([
      Buffer.from([255, 254, 1]), Buffer.from("credential\nstatus\n"),
    ]));
    connection.receive(Buffer.from("Ready\r\n"));
    connection.end();
    assert.equal(await pending, "Ready");
  });

  it("closes a connection that opens after its deadline without sending credentials", async () => {
    const connection = controlledConnection();
    const expireConnection = captureDeadline(5_000);
    const pending = executeSourceRcon("127.0.0.1", 12345, "do-not-leak", "stop");
    expireConnection();
    await assert.rejects(pending, /connection timed out/);
    connection.open();
    await Promise.resolve();
    assert.equal(connection.terminated, true);
    assert.deepEqual(connection.accepted, []);
  });

  it("enforces the overall deadline after a stalled connection opens", async () => {
    const connection = controlledConnection();
    const expireCommand = captureDeadline(10_000);
    const pending = executeTelnetCommand("127.0.0.1", 12345, "credential", "stop");
    connection.open();
    expireCommand();
    await assert.rejects(pending, /command timed out/);
    connection.receive(Buffer.from("Password: "));
    assert.equal(connection.terminated, true);
    assert.deepEqual(connection.accepted, []);
  });

  it("rejects excessive incoming data before retaining or exposing its contents", async () => {
    const connection = controlledConnection();
    const pending = executeTelnetCommand("127.0.0.1", 12345, "credential", "stop");
    connection.open();
    connection.receive(Buffer.alloc(4 * 1024 * 1024 + 64 * 1024 + 1, "do-not-leak"));
    await assert.rejects(pending, (error: Error) => {
      assert.match(error.message, /response is too large/);
      assert.doesNotMatch(error.message, /do-not-leak/);
      return true;
    });
    assert.equal(connection.terminated, true);
    assert.deepEqual(connection.accepted, []);
  });

  it("sanitizes socket errors and ignores late data after failure", async () => {
    const connection = controlledConnection();
    const pending = executeTelnetCommand("127.0.0.1", 12345, "do-not-leak", "stop");
    connection.open();
    connection.error(new Error("untrusted do-not-leak connection detail"));
    await assert.rejects(pending, (error: Error) => {
      assert.match(error.message, /Could not connect/);
      assert.doesNotMatch(error.message, /do-not-leak|untrusted/);
      return true;
    });
    connection.receive(Buffer.from("Password: "));
    connection.drain();
    assert.equal(connection.terminated, true);
    assert.deepEqual(connection.accepted, []);
  });

  it("limits the combined RCON response across individually valid packets", async () => {
    const connection = controlledConnection();
    const pending = executeSourceRcon("127.0.0.1", 12345, "credential", "status");
    connection.open();
    const auth = Buffer.concat(connection.accepted);
    connection.accepted.length = 0;
    connection.receive(encodePacket(auth.readInt32LE(4), 2, ""));
    const command = Buffer.concat(connection.accepted);
    const id = command.readInt32LE(4);
    const part = Buffer.alloc(2 * 1024 * 1024, "x");
    connection.receive(encodePacket(id, 0, part));
    connection.receive(encodePacket(id, 0, part));
    connection.receive(encodePacket(id, 0, "x"));
    await assert.rejects(pending, /response is too large/);
    assert.equal(connection.terminated, true);
  });
});

describe("Container stdin transport", () => {
  const adapter: GameConsoleAdapter = {
    id: "stdin-console",
    name: "Server console",
    transport: "container-stdin",
    commandPlaceholder: "help",
  };

  it("destroys an attachment without writing if access changes while it opens", async () => {
    let allowed = true;
    let received = "";
    const stream = new PassThrough();
    stream.on("data", (chunk: Buffer) => { received += chunk.toString(); });
    const container = {
      inspect: async () => ({ Config: { OpenStdin: true, StdinOnce: false } }),
      attach: async () => { allowed = false; return stream; },
    } as unknown as Docker.Container;
    await assert.rejects(executeGameCommand(container, { state: "running", labels: {} }, adapter, "stop", {
      stdout: () => {}, stderr: () => {}, system: () => {},
    }, () => { if (!allowed) throw new Error("Access revoked"); }), /Access revoked/);
    assert.equal(received, "");
    assert.equal(stream.destroyed, true);
  });

  it("attaches directly to the container stdin and sends a newline", async () => {
    const stream = new PassThrough();
    let received = "";
    let attachOptions: Docker.ContainerAttachOptions | undefined;
    stream.on("data", (chunk: Buffer) => {
      received += chunk.toString();
    });
    const container = {
      inspect: async () => ({
        Config: { OpenStdin: true, StdinOnce: false },
      }),
      attach: async (options: Docker.ContainerAttachOptions) => {
        attachOptions = options;
        return stream;
      },
    } as unknown as Docker.Container;
    const systemMessages: string[] = [];

    await executeGameCommand(
      container,
      { state: "running", labels: {} },
      adapter,
      "help",
      {
        stdout: () => undefined,
        stderr: () => undefined,
        system: (message) => systemMessages.push(message),
      }
    );

    assert.deepEqual(attachOptions, {
      stream: true,
      stdin: true,
      stdout: false,
      stderr: false,
      hijack: true,
    });
    assert.equal(received, "help\n");
    assert.equal(stream.destroyed, true);
    assert.deepEqual(systemMessages, ["Command sent to the server process"]);
  });

  it("explains when the container was not created with open stdin", async () => {
    let attached = false;
    const container = {
      inspect: async () => ({
        Config: { OpenStdin: false, StdinOnce: false },
      }),
      attach: async () => {
        attached = true;
        return new PassThrough();
      },
    } as unknown as Docker.Container;

    await assert.rejects(
      executeGameCommand(
        container,
        { state: "running", labels: {} },
        adapter,
        "help",
        {
          stdout: () => undefined,
          stderr: () => undefined,
          system: () => undefined,
        }
      ),
      /stdin_open: true/
    );
    assert.equal(attached, false);
  });
});

describe("Docker exec console transport", () => {
  const adapter: GameConsoleAdapter = {
    id: "minecraft-rcon", name: "Fixture", transport: "docker-exec",
    commandPlaceholder: "help", createExecOptions: (command) => ({ Cmd: ["fixture", command] }),
  };
  const output = { stdout: () => {}, stderr: () => {}, system: () => {} };
  it("does not start a prepared exec after access is revoked", async () => {
    let allowed = true;
    let started = false;
    const container = {
      exec: async () => {
        allowed = false;
        return { start: async () => { started = true; } };
      },
    } as unknown as Docker.Container;
    await assert.rejects(executeGameCommand(container, { state: "running", labels: {} }, adapter, "stop", output,
      () => { if (!allowed) throw new Error("Access revoked"); }), /Access revoked/);
    assert.equal(started, false);
  });
  it("reports a nonzero process exit instead of auditing a successful command", async () => {
    const container = {
      exec: async () => ({
        start: async () => {
          const stream = new PassThrough();
          setImmediate(() => stream.end());
          return stream;
        },
        inspect: async () => ({ Running: false, ExitCode: 1 }),
      }),
    } as unknown as Docker.Container;
    await assert.rejects(executeGameCommand(container, { state: "running", labels: {} }, adapter, "stop", output), /Game console command failed/);
  });
});

function listen(socket: SocketHandler<undefined>): number {
  const server = Bun.listen({ hostname: "127.0.0.1", port: 0, socket });
  closers.push(async () => { server.stop(true); });
  return server.port;
}

function closedPort(): number {
  const server = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  const port = server.port;
  server.stop(true);
  return port;
}

function listenRcon(receive: (socket: Socket<undefined>, packet: { id: number; type: number; body: Buffer }) => void): number {
  let buffer: Buffer<ArrayBufferLike> = Buffer.alloc(0);
  return listen({
    data(socket, chunk) {
      buffer = Buffer.concat([buffer, chunk]);
      while (buffer.length >= 4 && buffer.length >= buffer.readInt32LE(0) + 4) {
        const end = buffer.readInt32LE(0) + 4;
        const packet = buffer.subarray(0, end);
        buffer = buffer.subarray(end);
        receive(socket, {
          id: packet.readInt32LE(4),
          type: packet.readInt32LE(8),
          body: packet.subarray(12, packet.length - 2),
        });
      }
    },
  });
}

function listenTelnet(receive: (socket: Socket<undefined>, line: string) => void): number {
  let buffer = "";
  return listen({
    open(socket) { socket.write("Please enter password: "); },
    data(socket, chunk) {
      buffer += chunk.toString();
      let newline: number;
      while ((newline = buffer.indexOf("\n")) !== -1) {
        const line = buffer.slice(0, newline).trim();
        buffer = buffer.slice(newline + 1);
        if (line) receive(socket, line);
      }
    },
  });
}

function sendFragments(socket: Socket<undefined>, chunks: Buffer[]): void {
  const timers = chunks.map((chunk, index) => setTimeout(() => socket.write(chunk), index * 5));
  closers.push(async () => { for (const timer of timers) clearTimeout(timer); });
}

function controlledConnection(writeSizes: number[] = []) {
  let handlers: SocketHandler<undefined> | undefined;
  let resolveConnection!: (socket: Socket<undefined>) => void;
  const connected = new Promise<Socket<undefined>>((resolve) => { resolveConnection = resolve; });
  const accepted: Buffer[] = [];
  let terminated = false;
  const socket = {
    write(data: string | Uint8Array, offset = 0, length?: number) {
      const bytes = Buffer.from(data).subarray(offset, length === undefined ? undefined : offset + length);
      const count = Math.min(writeSizes.shift() ?? bytes.length, bytes.length);
      if (count) accepted.push(Buffer.from(bytes.subarray(0, count)));
      return count;
    },
    terminate() { terminated = true; },
    setNoDelay() { return true; },
    timeout() {},
  } as unknown as Socket<undefined>;
  const connectSpy = spyOn(Bun, "connect").mockImplementation((options) => {
    handlers = options.socket as SocketHandler<undefined>;
    return connected;
  });
  closers.push(async () => { connectSpy.mockRestore(); resolveConnection(socket); });
  const callbacks = () => {
    assert.ok(handlers, "The transport must initialize a native TCP connection");
    return handlers;
  };
  return {
    accepted,
    writeSizes,
    get terminated() { return terminated; },
    open() { callbacks().open?.(socket); resolveConnection(socket); },
    drain() { callbacks().drain?.(socket); },
    receive(data: Buffer) { callbacks().data?.(socket, data); },
    end() { callbacks().end?.(socket); },
    error(error: Error) { callbacks().error?.(socket, error); },
  };
}

function captureDeadline(delay: number): () => void {
  const originalSetTimeout = globalThis.setTimeout;
  let expire: (() => void) | undefined;
  const timerSpy = spyOn(globalThis, "setTimeout").mockImplementation((callback, milliseconds, ...args) => {
    if (milliseconds === delay) expire = () => { (callback as (...args: unknown[]) => void)(...args); };
    return originalSetTimeout(callback, milliseconds, ...args);
  });
  closers.push(async () => { timerSpy.mockRestore(); });
  return () => { assert.ok(expire, `Expected a ${delay}ms deadline`); expire(); };
}

function encodePacket(id: number, type: number, body: string | Uint8Array): Buffer {
  const payload = Buffer.from(body);
  const packet = Buffer.alloc(payload.length + 14);
  packet.writeInt32LE(payload.length + 10, 0);
  packet.writeInt32LE(id, 4);
  packet.writeInt32LE(type, 8);
  payload.copy(packet, 12);
  return packet;
}
