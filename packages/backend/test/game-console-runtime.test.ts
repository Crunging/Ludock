import { fixtureBytes, repeatedBytes } from "./fixtures/bytes.js";
import { byteView, concatBytes, decodeText, encodeText } from "../src/bytes.js";
import { rejectedBy } from "./fixtures/errors.js";
import { StreamFixture } from "./fixtures/web-streams.js";
import { existsSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { expect, afterEach, describe, it, spyOn } from "bun:test";
import type * as Docker from "../src/docker-client.js";
import type { DockerConnection } from "../src/docker-transport.js";
import { serve, type Socket, type SocketHandler } from "bun";
import {
  executeGameCommand,
  executeRustWebRcon,
  executeSourceRcon,
  executeTelnetCommand,
  MAX_DOCKER_EXEC_OUTPUT_BYTES,
} from "../src/game-console-runtime.js";
import type { GameConsoleAdapter } from "../src/game-console.js";

const closers: Array<() => Promise<void>> = [];

afterEach(async () => {
  await Promise.all(closers.splice(0).map((close) => close()));
});

describe("Source RCON transport", () => {
  it("does not send credentials when access changes while connecting", async () => {
    let accessChecks = 0;
    const received: Uint8Array[] = [];
    const port = listen({ data(_socket, chunk) { received.push(fixtureBytes(chunk)); } });

    await expect(executeSourceRcon("127.0.0.1", port, "credential", "stop", () => {
        if (++accessChecks > 1) throw new Error("Access revoked");
      })).rejects.toThrow(/Console access changed/);
    expect(accessChecks).toBe(2);
    expect(received).toStrictEqual([]);
  });

  it("does not send a command after access is revoked during authentication", async () => {
    let allowed = true;
    const received: string[] = [];
    const port = listenRcon((socket, packet) => {
      received.push(decodeText(packet.body));
      allowed = false;
      socket.write(encodePacket(packet.id, 2, ""));
    });
    await expect(executeSourceRcon("127.0.0.1", port, "credential", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    })).rejects.toThrow(/Console access changed/);
    expect(received).toStrictEqual(["credential"]);
  });

  it("does not report success when the connection closes before authentication", async () => {
    const port = listenRcon((socket) => { socket.end(); });
    await expect(executeSourceRcon("127.0.0.1", port, "credential", "stop")).rejects.toThrow(/authentication did not complete/);
  });
  it("authenticates and returns a native command response", async () => {
    const received: string[] = [];
    const port = listenRcon((socket, { id, type, body }) => {
      received.push(decodeText(body));
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

    expect(received).toStrictEqual(["correct horse", "status"]);
    expect(response).toBe("3 players");
  });

  it("reports rejected credentials without exposing the password", async () => {
    const port = listenRcon((socket) => socket.write(encodePacket(-1, 2, "")));

    await expect(await rejectedBy(executeSourceRcon("127.0.0.1", port, "do-not-leak", "status"))).toSatisfy((error: Error) => {
        expect(error.message).toMatch(/authentication failed/i);
        expect(error.message).not.toMatch(/do-not-leak/);
        return true;
      });
  });

  it("reassembles fragmented packets and UTF-8 split between response packets", async () => {
    const expected = "Players: José 🐉";
    const bytes = fixtureBytes(expected);
    // Split inside the four-byte dragon emoji.
    const split = fixtureBytes(expected.slice(0, expected.indexOf("🐉"))).length + 2;
    const port = listenRcon((socket, { id, type }) => {
      if (type === 3) {
        const packet = encodePacket(id, 2, "");
        sendFragments(socket, [packet.subarray(0, 2), packet.subarray(2, 7), packet.subarray(7)]);
      } else {
        const packets = concatBytes([
          encodePacket(id + 1, 0, "Unrelated reply"),
          encodePacket(id, 0, bytes.subarray(0, split)),
          encodePacket(id, 0, bytes.subarray(split)),
        ]);
        sendFragments(socket, [packets.subarray(0, 3), packets.subarray(3, 17), packets.subarray(17)]);
      }
    });

    expect(await executeSourceRcon("127.0.0.1", port, "credential", "status")).toBe(expected);
  });

  it("rejects an invalid packet length without exposing received content", async () => {
    const port = listenRcon((socket) => {
      const packet = fixtureBytes("do-not-leak");
      byteView(packet).setInt32(0, 4 * 1024 * 1024 + 1, true);
      socket.write(packet);
    });
    await expect(await rejectedBy(executeSourceRcon("127.0.0.1", port, "credential", "status"))).toSatisfy((error: Error) => {
      expect(error.message).toMatch(/invalid packet/);
      expect(error.message).not.toMatch(/do-not-leak|credential/);
      return true;
    });
  });

  it("rejects an incomplete packet after a complete response instead of returning partial success", async () => {
    const port = listenRcon((socket, { id, type }) => {
      if (type === 3) socket.write(encodePacket(id, 2, ""));
      else {
        socket.write(concatBytes([
          encodePacket(id, 0, "Partial response"),
          encodePacket(id, 0, "unfinished").subarray(0, 15),
        ]));
        const timer = setTimeout(() => socket.end(), 180);
        closers.push(async () => { clearTimeout(timer); });
      }
    });
    await expect(executeSourceRcon("127.0.0.1", port, "credential", "status")).rejects.toThrow(/incomplete packet/);
  });

  it("reports refused connections without exposing the password", async () => {
    const port = closedPort();
    await expect(await rejectedBy(executeSourceRcon("127.0.0.1", port, "do-not-leak", "status"))).toSatisfy((error: Error) => {
      expect(error.message).toMatch(/Could not connect/);
      expect(error.message).not.toMatch(/do-not-leak/);
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
    await expect(await rejectedBy(executeRustWebRcon("127.0.0.1", server.port!, "do-not-leak", "status"))).toSatisfy((error: Error) => {
      expect(error.message).toMatch(/invalid response/);
      expect(error.message).not.toMatch(/do-not-leak|credential-response/);
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
    await expect(executeRustWebRcon("127.0.0.1", port, "credential", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    })).rejects.toThrow(/Console access changed/);
    expect(sent).toBe(false);
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
          const message = JSON.parse((typeof raw === "string" ? raw : decodeText(raw))) as {
            Identifier: number;
            Message: string;
            Name: string;
          };
          expect(message.Message).toBe("server.save");
          expect(message.Name).toBe("Ludock");
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

    expect(response).toBe("Saved");
    expect(requestPath).toBe("/a%20password");
  });
});

describe("Telnet console transport", () => {
  it("does not send credentials when access changes before the password prompt", async () => {
    let accessChecks = 0;
    const received: Uint8Array[] = [];
    const port = listen({
      open(socket) { socket.write("Password: "); },
      data(_socket, chunk) { received.push(fixtureBytes(chunk)); },
    });
    await expect(executeTelnetCommand("127.0.0.1", port, "credential", "stop", () => {
      if (++accessChecks > 1) throw new Error("Access revoked");
    })).rejects.toThrow(/Console access changed/);
    expect(accessChecks).toBe(2);
    expect(received).toStrictEqual([]);
  });

  it("does not send a command after access changes during password authentication", async () => {
    let allowed = true;
    const received: string[] = [];
    const port = listen({
      open(socket) { socket.write("Password: "); },
      data(socket, chunk) {
        received.push(decodeText(chunk).trim());
        allowed = false;
        socket.write("Logged in\n");
      },
    });
    await expect(executeTelnetCommand("127.0.0.1", port, "credential", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    })).rejects.toThrow(/Console access changed/);
    expect(received).toStrictEqual(["credential"]);
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

    expect(received).toStrictEqual(["telnet-secret", "listplayers"]);
    expect(response).toMatch(/PlayerOne, PlayerTwo/);
    expect(response).not.toMatch(/telnet-secret/);
  });

  it("preserves UTF-8 characters split between TCP chunks", async () => {
    const expected = "José 🐉";
    const bytes = fixtureBytes(expected);
    const port = listenTelnet((socket, line) => {
      if (line === "credential") socket.write("Logged in\r\n");
      else sendFragments(socket, [bytes.subarray(0, 4), bytes.subarray(4, 8), bytes.subarray(8)]);
    });

    const response = await executeTelnetCommand("127.0.0.1", port, "credential", "status");
    expect(response).toMatch(/José 🐉/);
    expect(response).not.toMatch(/�/);
  });

  it("handles fragmented Telnet negotiation before a fragmented password prompt", async () => {
    const received: Uint8Array[] = [];
    let input = new Uint8Array(0);
    const port = listen({
      open(socket) {
        sendFragments(socket, [
          fixtureBytes([255]), fixtureBytes([251]), fixtureBytes([1]),
          fixtureBytes("Pass"), fixtureBytes("word: "),
        ]);
      },
      data(socket, chunk) {
        received.push(fixtureBytes(chunk));
        input = concatBytes([input, chunk]);
        if (decodeText(input).includes("status\n")) socket.end("Ready\r\n");
      },
    });
    expect(await executeTelnetCommand("127.0.0.1", port, "credential", "status")).toBe("Ready");
    expect(concatBytes(received)).toStrictEqual(concatBytes([
      fixtureBytes([255, 254, 1]), fixtureBytes("credential\nstatus\n"),
    ]));
  });

  it("reports rejected credentials and never sends the command", async () => {
    const received: string[] = [];
    const port = listenTelnet((socket, line) => {
      received.push(line);
      socket.write("Authentication failed\r\n");
    });
    await expect(await rejectedBy(executeTelnetCommand("127.0.0.1", port, "do-not-leak", "stop"))).toSatisfy((error: Error) => {
      expect(error.message).toMatch(/authentication failed/);
      expect(error.message).not.toMatch(/do-not-leak/);
      return true;
    });
    expect(received).toStrictEqual(["do-not-leak"]);
  });

  it("rejects a disconnect before the delayed command and cancels its write", async () => {
    const received: string[] = [];
    const port = listenTelnet((socket, line) => {
      received.push(line);
      socket.end();
    });
    await expect(executeTelnetCommand("127.0.0.1", port, "credential", "stop")).rejects.toThrow(/closed before the command was sent/);
    await Bun.sleep(70);
    expect(received).toStrictEqual(["credential"]);
  });

  it("reports a connection failure without exposing the password", async () => {
    const port = closedPort();
    await expect(await rejectedBy(executeTelnetCommand("127.0.0.1", port, "do-not-leak", "status"))).toSatisfy((error: Error) => {
      expect(error.message).toMatch(/Could not connect/);
      expect(error.message).not.toMatch(/do-not-leak/);
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
    const auth = concatBytes(connection.accepted);
    expect(auth).toStrictEqual(encodePacket(byteView(auth).getInt32(4, true), 3, "credential"));

    connection.accepted.length = 0;
    connection.writeSizes.push(3, 0);
    connection.receive(encodePacket(byteView(auth).getInt32(4, true), 2, ""));
    connection.drain();
    connection.drain();
    const command = concatBytes(connection.accepted);
    expect(command).toStrictEqual(encodePacket(byteView(command).getInt32(4, true), 2, "status"));
    connection.receive(encodePacket(byteView(command).getInt32(4, true), 0, "Ready"));
    connection.end();
    expect(await pending).toBe("Ready");
    expect(connection.terminated).toBe(true);
  });

  it("discards queued credential bytes when access changes before drain", async () => {
    let allowed = true;
    const connection = controlledConnection([12, 0]);
    const pending = executeSourceRcon("127.0.0.1", 12345, "do-not-leak", "stop", () => {
      if (!allowed) throw new Error("Access revoked");
    });
    connection.open();
    const sentBeforeRevocation = concatBytes(connection.accepted);
    expect(sentBeforeRevocation.length).toBe(12);
    allowed = false;
    connection.drain();
    await expect(pending).rejects.toThrow(/Console access changed/);
    connection.drain();
    expect(concatBytes(connection.accepted)).toStrictEqual(sentBeforeRevocation);
    expect(connection.terminated).toBe(true);
  });

  it("keeps Telnet negotiation and credentials in order across partial writes", async () => {
    const connection = controlledConnection([1, 0]);
    const pending = executeTelnetCommand("127.0.0.1", 12345, "credential", "status");
    connection.open();
    connection.receive(concatBytes([fixtureBytes([255, 251, 1]), fixtureBytes("Password: ")]));
    connection.drain();
    connection.drain();
    await Bun.sleep(70);
    expect(concatBytes(connection.accepted)).toStrictEqual(concatBytes([
      fixtureBytes([255, 254, 1]), fixtureBytes("credential\nstatus\n"),
    ]));
    connection.receive(fixtureBytes("Ready\r\n"));
    connection.end();
    expect(await pending).toBe("Ready");
  });

  it("closes a connection that opens after its deadline without sending credentials", async () => {
    const connection = controlledConnection();
    const expireConnection = captureDeadline(5_000);
    const pending = executeSourceRcon("127.0.0.1", 12345, "do-not-leak", "stop");
    expireConnection();
    await expect(pending).rejects.toThrow(/connection timed out/);
    connection.open();
    await Promise.resolve();
    expect(connection.terminated).toBe(true);
    expect(connection.accepted).toStrictEqual([]);
  });

  it("enforces the overall deadline after a stalled connection opens", async () => {
    const connection = controlledConnection();
    const expireCommand = captureDeadline(10_000);
    const pending = executeTelnetCommand("127.0.0.1", 12345, "credential", "stop");
    connection.open();
    expireCommand();
    await expect(pending).rejects.toThrow(/command timed out/);
    connection.receive(fixtureBytes("Password: "));
    expect(connection.terminated).toBe(true);
    expect(connection.accepted).toStrictEqual([]);
  });

  it("rejects excessive incoming data before retaining or exposing its contents", async () => {
    const connection = controlledConnection();
    const pending = executeTelnetCommand("127.0.0.1", 12345, "credential", "stop");
    connection.open();
    connection.receive(repeatedBytes(4 * 1024 * 1024 + 64 * 1024 + 1, "do-not-leak"));
    await expect(await rejectedBy(pending)).toSatisfy((error: Error) => {
      expect(error.message).toMatch(/response is too large/);
      expect(error.message).not.toMatch(/do-not-leak/);
      return true;
    });
    expect(connection.terminated).toBe(true);
    expect(connection.accepted).toStrictEqual([]);
  });

  it("sanitizes socket errors and ignores late data after failure", async () => {
    const connection = controlledConnection();
    const pending = executeTelnetCommand("127.0.0.1", 12345, "do-not-leak", "stop");
    connection.open();
    connection.error(new Error("untrusted do-not-leak connection detail"));
    await expect(await rejectedBy(pending)).toSatisfy((error: Error) => {
      expect(error.message).toMatch(/Could not connect/);
      expect(error.message).not.toMatch(/do-not-leak|untrusted/);
      return true;
    });
    connection.receive(fixtureBytes("Password: "));
    connection.drain();
    expect(connection.terminated).toBe(true);
    expect(connection.accepted).toStrictEqual([]);
  });

  it("limits the combined RCON response across individually valid packets", async () => {
    const connection = controlledConnection();
    const pending = executeSourceRcon("127.0.0.1", 12345, "credential", "status");
    connection.open();
    const auth = concatBytes(connection.accepted);
    connection.accepted.length = 0;
    connection.receive(encodePacket(byteView(auth).getInt32(4, true), 2, ""));
    const command = concatBytes(connection.accepted);
    const id = byteView(command).getInt32(4, true);
    const part = repeatedBytes(2 * 1024 * 1024, "x");
    connection.receive(encodePacket(id, 0, part));
    connection.receive(encodePacket(id, 0, part));
    connection.receive(encodePacket(id, 0, "x"));
    await expect(pending).rejects.toThrow(/response is too large/);
    expect(connection.terminated).toBe(true);
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
    const stream = new StreamFixture();
    stream.onInput = chunk => { received += new TextDecoder().decode(chunk); };
    const container = {
      inspect: async () => ({ Config: { OpenStdin: true, StdinOnce: false } }),
      attach: async () => { allowed = false; return stream.connection; },
    } as unknown as Docker.Container;
    await expect(executeGameCommand(container, { state: "running", labels: {} }, adapter, "stop", {
      stdout: () => {}, stderr: () => {}, system: () => {},
    }, () => { if (!allowed) throw new Error("Access revoked"); })).rejects.toThrow(/Access revoked/);
    expect(received).toBe("");
    expect(stream.closed).toBe(true);
  });

  it("attaches directly to the container stdin and sends a newline", async () => {
    const stream = new StreamFixture();
    let received = "";
    let attachOptions: Docker.ContainerAttachOptions | undefined;
    stream.onInput = chunk => { received += new TextDecoder().decode(chunk); };
    const container = {
      inspect: async () => ({
        Config: { OpenStdin: true, StdinOnce: false },
      }),
      attach: async (options: Docker.ContainerAttachOptions) => {
        attachOptions = options;
        return stream.connection;
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

    expect(attachOptions).toStrictEqual({
      stream: true,
      stdin: true,
      stdout: false,
      stderr: false,
    });
    expect(received).toBe("help\n");
    expect(stream.closed).toBe(true);
    expect(systemMessages).toStrictEqual(["Command sent to the server process"]);
  });

  it("explains when the container was not created with open stdin", async () => {
    let attached = false;
    const container = {
      inspect: async () => ({
        Config: { OpenStdin: false, StdinOnce: false },
      }),
      attach: async () => {
        attached = true;
        return new StreamFixture().connection;
      },
    } as unknown as Docker.Container;

    await expect(executeGameCommand(
        container,
        { state: "running", labels: {} },
        adapter,
        "help",
        {
          stdout: () => undefined,
          stderr: () => undefined,
          system: () => undefined,
        }
      )).rejects.toThrow(/stdin_open: true/);
    expect(attached).toBe(false);
  });
});

describe("Docker exec console transport", () => {
  const adapter: GameConsoleAdapter = {
    id: "minecraft-rcon", name: "Fixture", transport: "docker-exec",
    commandPlaceholder: "help", createExecOptions: (command) => ({ Cmd: ["fixture", command] }),
  };
  const output = { stdout: () => {}, stderr: () => {}, system: () => {} };
  it("times out exec creation without starting a late result", async () => {
    const expire = captureDeadline(5_000);
    let created!: (exec: Docker.Exec) => void;
    let starts = 0;
    const pendingCreation = new Promise<Docker.Exec>((resolve) => { created = resolve; });
    const container = { exec: () => pendingCreation } as unknown as Docker.Container;
    const pending = executeGameCommand(
      container, { state: "running", labels: {} }, adapter, "stop", output,
    );
    expire();
    await expect(pending).rejects.toThrow(/preparation timed out/);
    created({ start: async () => { starts++; return endedStream(); } } as unknown as Docker.Exec);
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(starts).toBe(0);
  });
  it("cancels pending exec creation without starting a late result", async () => {
    const controller = new AbortController();
    let created!: (exec: Docker.Exec) => void;
    let starts = 0;
    const pendingCreation = new Promise<Docker.Exec>((resolve) => { created = resolve; });
    const container = { exec: () => pendingCreation } as unknown as Docker.Container;
    const pending = executeGameCommand(
      container, { state: "running", labels: {} }, adapter, "stop", output,
      undefined, controller.signal,
    );
    controller.abort();
    await expect(pending).rejects.toThrow(/cancelled/);
    created({ start: async () => { starts++; return endedStream(); } } as unknown as Docker.Exec);
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(starts).toBe(0);
  });
  it("does not settle an indeterminate start before its late stream is cleaned up", async () => {
    const controller = new AbortController();
    const mainStream = new StreamFixture();
    let returnStream!: (stream: StreamFixture) => void;
    let starting!: () => void;
    let cancellationStarted!: () => void;
    const didStart = new Promise<void>((resolve) => { starting = resolve; });
    const didCancel = new Promise<void>((resolve) => { cancellationStarted = resolve; });
    const delayedStart = new Promise<StreamFixture>((resolve) => { returnStream = resolve; });
    let creations = 0;
    const container = {
      exec: async () => ++creations === 1
        ? { start: () => { starting(); return delayedStart.then(stream => stream.connection); } }
        : { start: async () => { cancellationStarted(); return endedStream(); } },
    } as unknown as Docker.Container;
    const pending = executeGameCommand(
      container, { state: "running", labels: {} }, adapter, "stop", output,
      undefined, controller.signal,
    );
    let settled = false;
    void pending.then(() => { settled = true; }, () => { settled = true; });
    await didStart;
    controller.abort();
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(settled, "an unknown start outcome must retain the caller's lock").toBe(false);
    returnStream(mainStream);
    await didCancel;
    expect(settled, "late execution still needs cleanup before releasing the lock").toBe(false);
    mainStream.close();
    await expect(pending).rejects.toThrow(/cancelled/);
  });

  it("keeps the command literal while adding an in-container deadline", async () => {
    const command = 'say "hello"; touch /tmp/not-executed';
    let received: Docker.ExecCreateOptions | undefined;
    const container = {
      exec: async (options: Docker.ExecCreateOptions) => {
        received = options;
        return {
          start: async () => endedStream(),
          inspect: async () => ({ Running: false, ExitCode: 0 }),
        };
      },
    } as unknown as Docker.Container;

    await executeGameCommand(
      container,
      { state: "running", labels: {} },
      adapter,
      command,
      output,
    );

    expect(received?.Cmd?.slice(-2)).toStrictEqual(["fixture", command]);
    expect(received?.Cmd?.[0]).toBe("/bin/sh");
    expect(received?.Cmd?.[1]).toBe("-c");
    expect(received?.Cmd?.[2] || "").toMatch(/"\$@"/);
    expect(received?.Cmd?.[2] || "").toMatch(/mkdir "\$control"/);
    expect((received?.Cmd?.[2]?.indexOf(': > "$ready"') ?? -1) <
      (received?.Cmd?.[2]?.indexOf('"$@" &') ?? -1)).toBeTruthy();
    expect(received?.Cmd?.[2]?.includes(command)).toBe(false);

    const controlPath = `/tmp/.ludock-console-test-${crypto.randomUUID()}`;
    const process = Bun.spawn([
      "/bin/sh",
      "-c",
      received?.Cmd?.[2] || "",
      "ludock-console-test",
      controlPath,
      "1",
      "1",
      "/bin/sh",
      "-c",
      'trap "" TERM; exec sleep 30',
    ], { stdout: "ignore", stderr: "ignore" });
    const emergency = setTimeout(() => process.kill(9), 5_000);
    try {
      expect(await process.exited).toBe(124);
    } finally {
      clearTimeout(emergency);
      rmSync(controlPath, { force: true, recursive: true });
    }
  });
  it("does not start a prepared exec after access is revoked", async () => {
    let allowed = true;
    let started = false;
    const container = {
      exec: async () => {
        allowed = false;
        return { start: async () => { started = true; } };
      },
    } as unknown as Docker.Container;
    await expect(executeGameCommand(container, { state: "running", labels: {} }, adapter, "stop", output,
      () => { if (!allowed) throw new Error("Access revoked"); })).rejects.toThrow(/Access revoked/);
    expect(started).toBe(false);
  });
  it("reports a nonzero process exit instead of auditing a successful command", async () => {
    const container = {
      exec: async () => ({
        start: async () => {
          const stream = new StreamFixture();
          setImmediate(() => stream.close());
          return stream.connection;
        },
        inspect: async () => ({ Running: false, ExitCode: 1 }),
      }),
    } as unknown as Docker.Container;
    await expect(executeGameCommand(container, { state: "running", labels: {} }, adapter, "stop", output)).rejects.toThrow(/Game console command failed/);
  });
  it("reports the watchdog deadline separately from an ordinary failure", async () => {
    const container = {
      exec: async () => ({
        start: async () => endedStream(),
        inspect: async () => ({ Running: false, ExitCode: 124 }),
      }),
    } as unknown as Docker.Container;
    await expect(executeGameCommand(
        container,
        { state: "running", labels: {} },
        adapter,
        "stop",
        output,
      )).rejects.toThrow(/timed out/);
  });
  it("bounds the stream lifetime if Docker never reports wrapper exit", async () => {
    const expire = captureDeadline(23_000);
    const mainStream = new StreamFixture();
    let executionCount = 0;
    const container = {
      exec: async () => {
        executionCount++;
        if (executionCount === 1) {
          return {
            start: async () => mainStream.connection,
            inspect: async () => ({ Running: true, ExitCode: null }),
          };
        }
        return { start: async () => endedStream() };
      },
    } as unknown as Docker.Container;
    const pending = executeGameCommand(
      container,
      { state: "running", labels: {} },
      adapter,
      "stop",
      output,
    );
    await new Promise<void>((resolve) => setImmediate(resolve));
    expire();
    await expect(pending).rejects.toThrow(/timed out/);
    expect(mainStream.closed).toBe(true);
    expect(executionCount).toBe(2);
  });
  it("bounds the status check after the exec stream closes", async () => {
    const expire = captureDeadline(5_000);
    let inspecting!: () => void;
    const didInspect = new Promise<void>((resolve) => { inspecting = resolve; });
    const container = {
      exec: async () => ({
        start: async () => endedStream(),
        inspect: () => {
          inspecting();
          return new Promise<never>(() => {});
        },
      }),
    } as unknown as Docker.Container;
    const pending = executeGameCommand(
      container,
      { state: "running", labels: {} },
      adapter,
      "stop",
      output,
    );
    await didInspect;
    expire();
    await expect(pending).rejects.toThrow(/status check timed out/);
  });
  it("cancels the real exec and does not settle until its stream ends", async () => {
    const controller = new AbortController();
    const mainStream = new StreamFixture();
    const executions: Docker.ExecCreateOptions[] = [];
    let started!: () => void;
    let cancellationStarted!: () => void;
    const didStart = new Promise<void>((resolve) => { started = resolve; });
    const didCancel = new Promise<void>((resolve) => {
      cancellationStarted = resolve;
    });
    const container = {
      exec: async (options: Docker.ExecCreateOptions) => {
        executions.push(options);
        if (executions.length === 1) {
          return {
            start: async () => {
              started();
              return mainStream.connection;
            },
            inspect: async () => ({ Running: false, ExitCode: 125 }),
          };
        }
        return {
          start: async () => {
            cancellationStarted();
            return endedStream();
          },
        };
      },
    } as unknown as Docker.Container;
    const pending = executeGameCommand(
      container,
      { state: "running", labels: {} },
      adapter,
      "stop",
      output,
      undefined,
      controller.signal,
    );
    let settled = false;
    void pending.then(() => { settled = true; }, () => { settled = true; });
    await didStart;
    controller.abort();
    await didCancel;
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(settled).toBe(false);
    expect(executions.length).toBe(2);
    expect(executions[1].Cmd?.[2] || "").toMatch(/\/cancel/);
    expect(executions[1].Cmd?.[2] || "").toMatch(/while .*ready/);
    expect(executions[1].Cmd?.at(-1)).toBe(executions[0].Cmd?.[4]);
    mainStream.close();
    await expect(pending).rejects.toThrow(/cancelled/);

    const delayedControl = `/tmp/.ludock-console-test-${crypto.randomUUID()}`;
    const cancellation = Bun.spawn([
      ...(executions[1].Cmd?.slice(0, -1) ?? []),
      delayedControl,
    ], { stdout: "ignore", stderr: "ignore" });
    try {
      await new Promise<void>((resolve) => setTimeout(resolve, 50));
      mkdirSync(delayedControl, { mode: 0o700 });
      writeFileSync(`${delayedControl}/ready`, "", { mode: 0o600 });
      expect(await cancellation.exited).toBe(0);
      expect(existsSync(`${delayedControl}/cancel`)).toBe(true);
    } finally {
      cancellation.kill(9);
      rmSync(delayedControl, { force: true, recursive: true });
    }
  });
  it("bounds combined Docker exec output and terminates the command", async () => {
    const mainStream = new StreamFixture();
    const executions: Docker.ExecCreateOptions[] = [];
    let cancellationStarted!: () => void;
    const didCancel = new Promise<void>((resolve) => {
      cancellationStarted = resolve;
    });
    const received: string[] = [];
    const container = {
      exec: async (options: Docker.ExecCreateOptions) => {
        executions.push(options);
        if (executions.length === 1) {
          return {
            start: async () => {
              setImmediate(() => {
                mainStream.enqueue(dockerFrame(
                  1,
                  new Uint8Array(MAX_DOCKER_EXEC_OUTPUT_BYTES / 2).fill(97),
                ));
                mainStream.enqueue(dockerFrame(
                  2,
                  new Uint8Array(MAX_DOCKER_EXEC_OUTPUT_BYTES / 2 + 1).fill(98),
                ));
              });
              return mainStream.connection;
            },
            inspect: async () => ({ Running: false, ExitCode: 125 }),
          };
        }
        return {
          start: async () => {
            cancellationStarted();
            mainStream.close();
            return endedStream();
          },
        };
      },
    } as unknown as Docker.Container;
    const pending = executeGameCommand(
      container,
      { state: "running", labels: {} },
      adapter,
      "list",
      {
        ...output,
        stdout: (value) => received.push(value),
        stderr: (value) => received.push(value),
      },
    );

    await didCancel;
    await expect(pending).rejects.toThrow(/output exceeded its limit/);
    expect(encodeText(received.join("")).byteLength <=
      MAX_DOCKER_EXEC_OUTPUT_BYTES).toBeTruthy();
    expect(executions.length).toBe(2);
  });
});

function endedStream(): DockerConnection {
  const stream = new StreamFixture();
  setImmediate(() => stream.close());
  return stream.connection;
}

function dockerFrame(type: 1 | 2, value: string | Uint8Array): Uint8Array {
  const payload = fixtureBytes(value);
  const header = new Uint8Array(8);
  header[0] = type;
  byteView(header).setUint32(4, payload.length);
  return concatBytes([header, payload]);
}

function listen(socket: SocketHandler<undefined, "uint8array">): number {
  // Bun's listener types only model the default Buffer binary type.
  const server = Bun.listen({
    hostname: "127.0.0.1", port: 0,
    socket: { ...socket, binaryType: "uint8array" } as unknown as SocketHandler<undefined>,
  });
  closers.push(async () => { server.stop(true); });
  return server.port;
}

function closedPort(): number {
  const server = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  const port = server.port;
  server.stop(true);
  return port;
}

function listenRcon(receive: (socket: Socket<undefined>, packet: { id: number; type: number; body: Uint8Array }) => void): number {
  let buffer: Uint8Array = new Uint8Array(0);
  return listen({
    data(socket, chunk) {
      buffer = concatBytes([buffer, chunk]);
      while (buffer.length >= 4 && buffer.length >= byteView(buffer).getInt32(0, true) + 4) {
        const end = byteView(buffer).getInt32(0, true) + 4;
        const packet = buffer.subarray(0, end);
        buffer = buffer.subarray(end);
        receive(socket, {
          id: byteView(packet).getInt32(4, true),
          type: byteView(packet).getInt32(8, true),
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
      buffer += decodeText(chunk);
      let newline: number;
      while ((newline = buffer.indexOf("\n")) !== -1) {
        const line = buffer.slice(0, newline).trim();
        buffer = buffer.slice(newline + 1);
        if (line) receive(socket, line);
      }
    },
  });
}

function sendFragments(socket: Socket<undefined>, chunks: Uint8Array[]): void {
  const timers = chunks.map((chunk, index) => setTimeout(() => socket.write(chunk), index * 5));
  closers.push(async () => { for (const timer of timers) clearTimeout(timer); });
}

function controlledConnection(writeSizes: number[] = []) {
  let handlers: SocketHandler<undefined, "uint8array"> | undefined;
  let resolveConnection!: (socket: Socket<undefined>) => void;
  const connected = new Promise<Socket<undefined>>((resolve) => { resolveConnection = resolve; });
  const accepted: Uint8Array[] = [];
  let terminated = false;
  const socket = {
    write(data: string | Uint8Array, offset = 0, length?: number) {
      const bytes = fixtureBytes(data).subarray(offset, length === undefined ? undefined : offset + length);
      const count = Math.min(writeSizes.shift() ?? bytes.length, bytes.length);
      if (count) accepted.push(fixtureBytes(bytes.subarray(0, count)));
      return count;
    },
    terminate() { terminated = true; },
    setNoDelay() { return true; },
    timeout() {},
  } as unknown as Socket<undefined>;
  const connectSpy = spyOn(Bun, "connect").mockImplementation((options) => {
    handlers = options.socket as unknown as SocketHandler<undefined, "uint8array">;
    return connected;
  });
  closers.push(async () => { connectSpy.mockRestore(); resolveConnection(socket); });
  const callbacks = () => {
    if (!handlers) throw new Error("The transport must initialize a native TCP connection");
    return handlers;
  };
  return {
    accepted,
    writeSizes,
    get terminated() { return terminated; },
    open() { callbacks().open?.(socket); resolveConnection(socket); },
    drain() { callbacks().drain?.(socket); },
    receive(data: Uint8Array<ArrayBuffer>) { callbacks().data?.(socket, data); },
    end() { callbacks().end?.(socket); },
    error(error: Error) { callbacks().error?.(socket, error); },
  };
}

function captureDeadline(delay: number): () => void {
  const originalSetTimeout = globalThis.setTimeout;
  let expire: (() => void) | undefined;
  const timerSpy = spyOn(globalThis, "setTimeout").mockImplementation(((
    callback: (...args: unknown[]) => void, milliseconds?: number, ...args: unknown[]
  ) => {
    if (milliseconds === delay) expire = () => { callback(...args); };
    return originalSetTimeout(callback, milliseconds, ...args);
  }) as typeof setTimeout);
  closers.push(async () => { timerSpy.mockRestore(); });
  return () => { expect(expire, `Expected a ${delay}ms deadline`).toBeTruthy(); expire?.(); };
}

function encodePacket(id: number, type: number, body: string | Uint8Array): Uint8Array<ArrayBuffer> {
  const payload = fixtureBytes(body);
  const packet = new Uint8Array(payload.length + 14);
  byteView(packet).setInt32(0, payload.length + 10, true);
  byteView(packet).setInt32(4, id, true);
  byteView(packet).setInt32(8, type, true);
  packet.set(payload, 12);
  return packet;
}
