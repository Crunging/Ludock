import { encodeText, concatBytes } from "./bytes.js";
export interface DockerConnection {
  readable: ReadableStream<Uint8Array>;
  writable: WritableStream<Uint8Array>;
  /** Terminate both directions. Closing writable alone sends stdin EOF. */
  abort(reason?: unknown): void;
}

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

  async stream(path: string): Promise<ReadableStream<Uint8Array>> {
    const response = await this.request(path);
    if (!response.body) throw new Error("Docker returned an empty stream");
    return response.body;
  }

  hijack(path: string, body?: unknown): Promise<DockerConnection> {
    return openDockerStream(this.socketPath, path, body);
  }
}

/** Each direction has a byte-bounded queue. Stdin EOF preserves stdout until
 * the helper reports its result and finishes cleanup. */
function openDockerStream(socketPath: string, path: string, body?: unknown): Promise<DockerConnection> {
  return new Promise((resolve, reject) => {
    let socket: Bun.Socket | undefined;
    let headers = new Uint8Array(0);
    let upgraded = false;
    let requestSent = false;
    let delivered = false;
    let ended = false;
    let aborted = false;
    let writeClosed = false;
    let input!: ReadableStreamDefaultController<Uint8Array>;
    let output!: WritableStreamDefaultController;
    let pending: { data: Uint8Array; offset: number; resolve(): void; reject(error: unknown): void } | undefined;
    const abort = (reason: unknown = new Error("Docker stream connection failed")) => {
      if (aborted) return;
      const error = reason instanceof Error ? reason : new Error("Docker stream cancelled");
      aborted = true;
      socket?.terminate();
      input.error(error);
      output.error(error);
      pending?.reject(error);
      pending = undefined;
      if (!delivered) reject(error);
    };
    const readable = new ReadableStream<Uint8Array>({
      start(controller) { input = controller; },
      pull() { if (!ended && !aborted) socket?.resume(); },
      cancel(reason) { abort(reason); },
    }, new ByteLengthQueuingStrategy({ highWaterMark: 65_536 }));
    const write = (data: Uint8Array): Promise<void> => new Promise((resolveWrite, rejectWrite) => {
      if (aborted || ended || writeClosed) { rejectWrite(new Error("Docker stream closed")); return; }
      pending = { data, offset: 0, resolve: resolveWrite, reject: rejectWrite };
      flush();
    });
    const writable = new WritableStream<Uint8Array>({
      start(controller) { output = controller; },
      write,
      close() {
        // Bun 1.4.2 shutdown() sends FIN on the write side; shutdown(true)
        // shuts down reads, and end() closes the socket. The upload socket
        // tests exercise the runtime behavior rather than relying on typings.
        // Bun uses the same call in src/js/node/net.ts's endNT implementation.
        socket?.shutdown();
        writeClosed = true;
      },
      abort,
    }, new ByteLengthQueuingStrategy({ highWaterMark: 65_536 }));
    const deliver = () => {
      if (!aborted && upgraded && requestSent && !delivered) {
        delivered = true;
        resolve({ readable, writable, abort });
      }
    };
    const fail = (error = new Error("Docker stream connection failed")) => abort(error);
    const flush = () => {
      if (!socket || !pending || aborted) return;
      const write = pending;
      try {
        const count = socket.write(write.data, write.offset, write.data.length - write.offset);
        if (count < 0) { fail(); return; }
        write.offset += count;
        if (write.offset === write.data.length) {
          pending = undefined;
          write.resolve();
        }
        // A short write is resumed from this exact offset by Bun's drain event.
      } catch { fail(); }
    };
    const finish = () => {
      if (ended || aborted) return;
      if (!upgraded) { fail(new Error("Docker closed before completing the upgrade")); return; }
      ended = true;
      input.close();
      const error = new Error("Docker stream closed");
      pending?.reject(error);
      pending = undefined;
      if (!writeClosed) output.error(error);
      socket?.terminate();
    };
    const payload = body === undefined ? "" : JSON.stringify(body);
    const request = encodeText(
      `POST ${path} HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n` +
      `Content-Type: application/json\r\nContent-Length: ${encodeText(payload).byteLength}\r\n\r\n${payload}`,
    );
    void Bun.connect({
      unix: socketPath,
      allowHalfOpen: true,
      socket: {
        binaryType: "uint8array",
        open(connected) {
          socket = connected;
          if (aborted) { connected.terminate(); return; }
          void write(request).then(() => { requestSent = true; deliver(); }, () => fail());
        },
        data(connected, data) {
          if (aborted || ended) return;
          let chunk = Uint8Array.from(data);
          if (!upgraded) {
            headers = concatBytes([headers, chunk]);
            let boundary = -1;
            for (let i = 0; i + 3 < headers.length; i++) {
              if (headers[i] === 13 && headers[i + 1] === 10 && headers[i + 2] === 13 && headers[i + 3] === 10) { boundary = i; break; }
            }
            if ((boundary === -1 ? headers.length : boundary) > 16_384) {
              fail(new Error("Docker upgrade headers exceeded their limit"));
              return;
            }
            if (boundary === -1) return;
            const lines = String.fromCharCode(...headers.subarray(0, boundary)).split("\r\n");
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
            headers = new Uint8Array(0);
            upgraded = true;
            deliver();
          }
          if (chunk.length) {
            input.enqueue(chunk);
            if ((input.desiredSize ?? 0) <= 0) connected.pause();
          }
        },
        drain() { flush(); },
        end() { finish(); },
        close(_connected, error) {
          if (ended || aborted) return;
          if (error || !upgraded) { fail(); return; }
          // After local shutdown Bun can deliver graceful EOF as close only.
          finish();
        },
        error() { fail(); },
        connectError() { fail(); },
      },
    }).then((connected) => {
      if (aborted) connected.terminate();
    }, () => { fail(); });
  });
}
