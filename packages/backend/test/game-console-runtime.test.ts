import assert from "node:assert/strict";
import net from "node:net";
import { PassThrough } from "node:stream";
import { afterEach, describe, it } from "node:test";
import type Docker from "dockerode";
import { WebSocketServer } from "ws";
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
  it("authenticates and returns a native command response", async () => {
    const received: string[] = [];
    const server = net.createServer((socket) => {
      let buffer = Buffer.alloc(0);
      socket.on("data", (chunk) => {
        buffer = Buffer.concat([buffer, chunk]);
        while (buffer.length >= 4 && buffer.length >= buffer.readInt32LE(0) + 4) {
          const size = buffer.readInt32LE(0);
          const packet = buffer.subarray(0, size + 4);
          buffer = buffer.subarray(size + 4);
          const id = packet.readInt32LE(4);
          const type = packet.readInt32LE(8);
          const body = packet.subarray(12, packet.length - 2).toString();
          received.push(body);
          socket.write(
            encodePacket(id, type === 3 ? 2 : 0, type === 3 ? "" : "3 players")
          );
        }
      });
    });
    const port = await listen(server);

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
    const server = net.createServer((socket) => {
      socket.once("data", () => socket.write(encodePacket(-1, 2, "")));
    });
    const port = await listen(server);

    await assert.rejects(
      executeSourceRcon("127.0.0.1", port, "do-not-leak", "status"),
      (error: Error) => {
        assert.match(error.message, /authentication failed/i);
        assert.doesNotMatch(error.message, /do-not-leak/);
        return true;
      }
    );
  });
});

describe("Rust WebRCON transport", () => {
  it("uses the WebRCON request envelope and matches its response", async () => {
    let requestPath = "";
    const server = new WebSocketServer({ host: "127.0.0.1", port: 0 });
    server.on("connection", (socket, request) => {
      requestPath = request.url || "";
      socket.once("message", (raw) => {
        const message = JSON.parse(raw.toString()) as {
          Identifier: number;
          Message: string;
          Name: string;
        };
        assert.equal(message.Message, "server.save");
        assert.equal(message.Name, "Ludock");
        socket.send(
          JSON.stringify({
            Identifier: message.Identifier,
            Message: "Saved",
            Name: "WebRcon",
          })
        );
      });
    });
    const port = await websocketPort(server);

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
  it("authenticates and sends a command after the password prompt", async () => {
    const received: string[] = [];
    const server = net.createServer((socket) => {
      socket.write("Please enter password: ");
      socket.on("data", (chunk) => {
        for (const line of chunk.toString().split("\n")) {
          const value = line.trim();
          if (!value) continue;
          received.push(value);
          if (value === "telnet-secret") {
            socket.write("Log on successful.\r\n");
          } else {
            socket.write("PlayerOne, PlayerTwo\r\n");
          }
        }
      });
    });
    const port = await listen(server);

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
});

describe("Container stdin transport", () => {
  const adapter: GameConsoleAdapter = {
    id: "stdin-console",
    name: "Server console",
    transport: "container-stdin",
    commandPlaceholder: "help",
  };

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

async function listen(server: net.Server): Promise<number> {
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  closers.push(
    () =>
      new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
      })
  );
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  return address.port;
}

async function websocketPort(server: WebSocketServer): Promise<number> {
  if (!server.address()) {
    await new Promise<void>((resolve, reject) => {
      server.once("listening", resolve);
      server.once("error", reject);
    });
  }
  closers.push(
    () =>
      new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
      })
  );
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  return address.port;
}

function encodePacket(id: number, type: number, body: string): Buffer {
  const payload = Buffer.from(body);
  const packet = Buffer.alloc(payload.length + 14);
  packet.writeInt32LE(payload.length + 10, 0);
  packet.writeInt32LE(id, 4);
  packet.writeInt32LE(type, 8);
  payload.copy(packet, 12);
  return packet;
}
