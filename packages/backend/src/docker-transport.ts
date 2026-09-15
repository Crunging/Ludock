import { Duplex, Readable } from "node:stream";

/** Daemon diagnostics can contain credentials, commands, and host paths. Keep
 * only the HTTP status; existing callers use it for absence/conflict handling. */
export class DockerApiError extends Error {
  constructor(readonly statusCode: number) {
    super(`Docker API request failed (HTTP ${statusCode})`);
    this.name = "DockerApiError";
  }
}

export class DockerTransport {
  constructor(readonly socketPath: string) {}

  async request(path: string, method = "GET", body?: unknown): Promise<Response> {
    const response = await fetch(`http://localhost${path}`, {
      unix: this.socketPath,
      proxy: "", // Ambient HTTP proxy settings must never redirect daemon traffic.
      method,
      headers: body === undefined ? undefined : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
      redirect: "manual",
      // Logs, events, pulls, and stopped-state waits may be silent indefinitely.
      // Operation owners control deadlines and cleanup; never retry mutations.
      timeout: false,
    });
    if (!response.ok) {
      await response.body?.cancel();
      throw new DockerApiError(response.status);
    }
    return response;
  }

  async json<T>(path: string, method = "GET", body?: unknown): Promise<T> {
    const response = await this.request(path, method, body);
    try { return await response.json() as T; }
    catch { throw new Error("Docker returned invalid JSON"); }
  }

  async empty(path: string, method: string): Promise<void> {
    const response = await this.request(path, method);
    await response.body?.cancel();
  }

  async stream(path: string): Promise<Readable> {
    const response = await this.request(path);
    if (!response.body) throw new Error("Docker returned an empty stream");
    // This adapter preserves the existing file/console stream boundaries.
    // Destroy cancels the fetch body; reads retain backpressure to the socket.
    return Readable.fromWeb(response.body);
  }

  hijack(path: string, body?: unknown): Promise<Duplex> {
    return openDockerStream(this.socketPath, path, body);
  }
}

/** Bun owns the socket; Duplex supplies bounded buffers to the existing file
 * pipelines. In particular, end() must send FIN only on stdin and keep stdout
 * open until a helper reports its result and finishes cleanup. */
function openDockerStream(socketPath: string, path: string, body?: unknown): Promise<Duplex> {
  return new Promise((resolve, reject) => {
    let socket: Bun.Socket | undefined;
    let headers = Buffer.alloc(0);
    let upgraded = false;
    let ended = false;
    let pending: { data: Buffer; offset: number; callback: (error?: Error | null) => void } | undefined;
    const stream = new Duplex({
      allowHalfOpen: true,
      read() { socket?.resume(); },
      write(data: Buffer, _encoding, callback) {
        pending = { data, offset: 0, callback };
        flush();
      },
      final(callback) {
        // Bun 1.4.2 shutdown() sends FIN on the write side; shutdown(true)
        // shuts down reads, and end() closes the socket. The upload socket
        // tests exercise the runtime behavior rather than relying on typings.
        // Bun uses the same call in src/js/node/net.ts's endNT implementation.
        socket?.shutdown();
        callback();
      },
      destroy(error, callback) {
        socket?.terminate();
        const write = pending;
        pending = undefined;
        write?.callback(error || new Error("Docker stream closed"));
        if (!upgraded) reject(error || new Error("Docker upgrade did not complete"));
        callback(error);
      },
    });
    // A failed handshake can precede delivery to the caller. Keep its error
    // handled, including streams that finish before a consumer subscribes.
    stream.on("error", () => {});
    stream.once("end", () => stream.destroy());
    const fail = (error = new Error("Docker stream connection failed")) => stream.destroy(error);
    const flush = () => {
      if (!socket || !pending || stream.destroyed) return;
      const write = pending;
      try {
        const count = socket.write(write.data, write.offset, write.data.length - write.offset);
        if (count < 0) { fail(); return; }
        write.offset += count;
        if (write.offset === write.data.length) {
          pending = undefined;
          write.callback();
        }
        // A short write is resumed from this exact offset by Bun's drain event.
      } catch { fail(); }
    };
    const payload = body === undefined ? "" : JSON.stringify(body);
    const request = Buffer.from(
      `POST ${path} HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n` +
      `Content-Type: application/json\r\nContent-Length: ${Buffer.byteLength(payload)}\r\n\r\n${payload}`,
    );
    void Bun.connect({
      unix: socketPath,
      allowHalfOpen: true,
      socket: {
        open(connected) {
          socket = connected;
          if (stream.destroyed) { connected.terminate(); return; }
          stream.write(request);
        },
        data(connected, data) {
          if (stream.destroyed) return;
          let chunk = Buffer.from(data);
          if (!upgraded) {
            headers = Buffer.concat([headers, chunk]);
            const boundary = headers.indexOf("\r\n\r\n");
            if ((boundary === -1 ? headers.length : boundary) > 16_384) {
              fail(new Error("Docker upgrade headers exceeded their limit"));
              return;
            }
            if (boundary === -1) return;
            const lines = headers.subarray(0, boundary).toString("latin1").split("\r\n");
            const status = /^HTTP\/1\.[01] (\d{3})(?: |$)/.exec(lines.shift() || "");
            if (!status) { fail(new Error("Invalid Docker upgrade response")); return; }
            const statusCode = Number(status[1]);
            if (statusCode !== 101 && statusCode !== 200) { fail(new DockerApiError(statusCode)); return; }
            const fields = new Headers();
            for (const line of lines) {
              const colon = line.indexOf(":");
              if (colon <= 0) { fail(new Error("Invalid Docker upgrade headers")); return; }
              try { fields.append(line.slice(0, colon), line.slice(colon + 1).trim()); }
              catch { fail(new Error("Invalid Docker upgrade headers")); return; }
            }
            // Docker also documents a legacy 200 raw-stream response. It is
            // already hijacked; HTTP chunk framing must never reach stdin/stdout.
            if (fields.has("transfer-encoding") ||
                (statusCode === 101 && (fields.get("upgrade")?.toLowerCase() !== "tcp" ||
                  !fields.get("connection")?.toLowerCase().split(/\s*,\s*/).includes("upgrade"))) ||
                (statusCode === 200 && !/^application\/vnd\.docker\.(?:raw|multiplexed)-stream(?:;|$)/i.test(fields.get("content-type") || ""))) {
              fail(new Error("Invalid Docker stream upgrade"));
              return;
            }
            chunk = headers.subarray(boundary + 4);
            headers = Buffer.alloc(0);
            upgraded = true;
            resolve(stream);
          }
          if (chunk.length && !stream.push(chunk)) connected.pause();
        },
        drain() { flush(); },
        end(connected) {
          ended = true;
          if (!upgraded) fail(new Error("Docker closed before completing the upgrade"));
          else stream.push(null);
          connected.terminate();
        },
        close(_connected, error) {
          if (ended || stream.destroyed) return;
          if (error || !upgraded) { fail(); return; }
          // After local shutdown Bun can deliver graceful EOF as close only.
          ended = true;
          stream.push(null);
        },
        error() { fail(); },
        connectError() { fail(); },
      },
    }).then((connected) => {
      if (stream.destroyed) connected.terminate();
    }, () => { fail(); });
  });
}
