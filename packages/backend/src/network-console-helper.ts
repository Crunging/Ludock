import type * as Docker from "./docker-client.js";
import { docker } from "./docker-client.js";
import { createHelperContainer, removeHelperContainer } from "./docker-helpers.js";
import { demuxDockerStream } from "./docker-stream.js";
import type { DockerConnection } from "./docker-transport.js";
import { resolveHelperImage } from "./runtime-images.js";
import { NETWORK_CONSOLE_SCRIPT } from "./helper-scripts.js";
import { JsonLineDecoder } from "./json-lines.js";
import { encodeText } from "./bytes.js";
import { MAX_NETWORK_CONSOLE_BYTES, type NetworkConsoleRequest } from "./helpers/network-console.js";

const PREPARATION_TIMEOUT_MS = 5_000;
const COMMAND_TIMEOUT_MS = 10_000;
// JSON escaping can expand each response byte sixfold.
const MAX_HELPER_WIRE_BYTES = 6 * (MAX_NETWORK_CONSOLE_BYTES + 65_536) + 65_536;

export async function executeNetworkConsoleHelper(
  containerId: string,
  request: NetworkConsoleRequest,
  assertAccess?: () => void,
  signal?: AbortSignal,
): Promise<string> {
  const check = () => {
    if (signal?.aborted) throw new Error("Console command cancelled");
    assertAccess?.();
  };
  check();
  if (encodeText(request.password).length + encodeText(request.command).length > MAX_NETWORK_CONSOLE_BYTES)
    throw new Error("Console command or credential is too large");
  const name = `ludock-console-${crypto.randomUUID()}`;
  let helper: Docker.Container | undefined;
  let stream: DockerConnection | undefined;
  let failure: Error | undefined;
  let rejectFailure!: (error: Error) => void;
  const failed = new Promise<never>((_, reject) => { rejectFailure = reject; });
  // Attach a rejection handler even while an uncertain Docker mutation settles.
  void failed.catch(() => {});
  const cancel = (error: Error) => {
    failure ??= error;
    rejectFailure(failure);
    stream?.abort(failure);
  };
  const onAbort = () => cancel(new Error("Console command cancelled"));
  signal?.addEventListener("abort", onAbort, { once: true });
  if (signal?.aborted) onAbort();
  const revalidate = setInterval(() => {
    try { check(); } catch { cancel(new Error("Console access changed")); }
  }, 250);
  let deadline = setTimeout(() => cancel(new Error("Game console preparation timed out")), PREPARATION_TIMEOUT_MS);
  const guard = () => { if (failure) throw failure; check(); };
  try {
    const image = await Promise.race([resolveHelperImage(), failed]);
    guard();
    // Do not race create/start: an HTTP timeout cannot prove Docker rejected a
    // mutation. Wait for its outcome, then remove even a late-created helper.
    try {
      helper = await createHelperContainer({
        name,
        Image: image,
        Entrypoint: ["bun", "-e"],
        Cmd: [NETWORK_CONSOLE_SCRIPT],
        User: "65534:65534",
        OpenStdin: true,
        StdinOnce: true,
        Tty: false,
        Labels: { "ludock.enable": "false", "ludock.internal": "console-helper" },
        HostConfig: {
          NetworkMode: `container:${containerId}`,
          ReadonlyRootfs: true,
          AutoRemove: true,
          CapDrop: ["ALL"],
          SecurityOpt: ["no-new-privileges"],
          PidsLimit: 32,
          Memory: 256 * 1024 * 1024,
          NanoCpus: 1_000_000_000,
        },
      });
    } catch (error) {
      // A lost create response may still have created the named container.
      helper = docker.getContainer(name);
      throw error;
    }
    guard();
    const attaching = helper.attach({ stream: true, stdin: true, stdout: true, stderr: true }).then(attached => {
      if (failure) { attached.abort(failure); throw failure; }
      return attached;
    });
    stream = await Promise.race([attaching, failed]);
    guard();
    await helper.start();
    guard();
    clearTimeout(deadline);
    deadline = setTimeout(() => cancel(new Error("Game console command timed out")), COMMAND_TIMEOUT_MS);
    const writer = stream.writable.getWriter();
    let response: string | undefined;
    try {
      await Promise.race([writer.write(encodeText(JSON.stringify(request) + "\n")), failed]);
      const records: unknown[] = [];
      const decoder = new JsonLineDecoder(record => { records.push(record); }, false, MAX_HELPER_WIRE_BYTES);
      let received = 0;
      await Promise.race([
        demuxDockerStream(stream.readable, async chunk => {
          received += chunk.length;
          if (received > MAX_HELPER_WIRE_BYTES) throw new Error("Game console output exceeded its limit");
          decoder.push(chunk);
          for (const record of records.splice(0)) {
            guard();
            if (!record || typeof record !== "object" || !("type" in record) || response !== undefined)
              throw new Error("Invalid console helper response");
            if (record.type === "authorize") {
              await Promise.race([writer.write(encodeText('{"type":"authorized"}\n')), failed]);
            } else if (record.type === "result" && "response" in record && typeof record.response === "string") {
              if (encodeText(record.response).length > MAX_NETWORK_CONSOLE_BYTES + 65_536)
                throw new Error("Game console output exceeded its limit");
              response = record.response;
            } else throw new Error("Game console command failed");
          }
        }, () => {
          // Helper diagnostics are never console output or error messages.
          throw new Error("Game console helper failed");
        }),
        failed,
      ]);
      decoder.end();
      if (records.length) throw new Error("Incomplete console helper response");
      guard();
      if (response === undefined) throw new Error("Game console helper ended without a response");
      return response;
    } finally { writer.releaseLock(); }
  } finally {
    clearTimeout(deadline);
    clearInterval(revalidate);
    signal?.removeEventListener("abort", onAbort);
    stream?.abort();
    if (helper) {
      while (true) {
        try { await removeHelperContainer(helper); break; }
        catch {
          // The caller owns the server lock until Docker confirms removal,
          // including daemon outages after timeout, revocation, or disconnect.
          await Bun.sleep(1_000);
        }
      }
    }
  }
}
