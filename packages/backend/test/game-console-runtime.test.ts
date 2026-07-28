import assert from "node:assert/strict";
import net from "node:net";
import { afterEach, describe, it } from "node:test";
import { WebSocketServer } from "ws";
import {
  executeRustWebRcon,
  executeSourceRcon,
  executeTelnetCommand,
} from "../src/game-console-runtime.js";

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
        assert.equal(message.Name, "Game Panel");
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
