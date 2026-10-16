import { afterEach, describe, expect, it, mock, spyOn } from "bun:test";
import { docker, type Container, type ContainerCreateOptions } from "../src/docker-client.js";
import { executeNetworkConsoleHelper } from "../src/network-console-helper.js";
import { executeGameCommand } from "../src/game-console-runtime.js";
import { resolveGameConsoleAdapter } from "../src/game-console.js";
import { ConsoleOutputRedactor } from "../src/console-redaction.js";
import { acquireLocks, withLocks } from "../src/operation-locks.js";
import { byteView, concatBytes, decodeText, encodeText } from "../src/bytes.js";
import { StreamFixture } from "./fixtures/web-streams.js";
import { FALLBACK_HELPER_IMAGE } from "../src/runtime-images.js";
import { MAX_NETWORK_CONSOLE_BYTES, type NetworkConsoleRequest } from "../src/helpers/network-console.js";

const previousImage = process.env.FILE_HELPER_IMAGE;
const request: NetworkConsoleRequest = {
  transport: "source-rcon", port: 27015, password: "private-console-credential", command: "status",
};
afterEach(() => {
  mock.restore();
  if (previousImage === undefined) delete process.env.FILE_HELPER_IMAGE;
  else process.env.FILE_HELPER_IMAGE = previousImage;
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

function fixture() {
  process.env.FILE_HELPER_IMAGE = FALLBACK_HELPER_IMAGE;
  const stream = new StreamFixture();
  const input = deferred<void>();
  const removing = deferred<void>();
  const start = mock(async () => {});
  const remove = mock(async () => { removing.resolve(); });
  const helper = {
    start, remove, stop: mock(async () => {}),
    attach: mock(async () => stream.connection),
  } as unknown as Container;
  const create = spyOn(docker, "createContainer").mockResolvedValue(helper);
  stream.onInput = () => { input.resolve(); };
  const send = (record: object) => {
    const payload = encodeText(JSON.stringify(record) + "\n");
    const header = new Uint8Array(8);
    header[0] = 1;
    byteView(header).setUint32(4, payload.length);
    stream.enqueue(concatBytes([header, payload]));
  };
  return { stream, input, removing, start, remove, helper, create, send };
}

function deadline(milliseconds: number) {
  const original = globalThis.setTimeout;
  const called = deferred<void>();
  let expire!: () => void;
  spyOn(globalThis, "setTimeout").mockImplementation(((callback: () => void, delay?: number) => {
    if (delay === milliseconds) { expire = callback; called.resolve(); }
    return original(callback, delay);
  }) as typeof setTimeout);
  return async () => { await called.promise; expire(); };
}

describe("Namespace-sharing console helper", () => {
  for (const gameType of ["factorio", "rust", "7-days-to-die"]) {
    it(`selects the helper and the internal port by default for ${gameType}`, async () => {
      const f = fixture();
      const adapter = resolveGameConsoleAdapter({ gameType, labels: {} })!;
      const container = {
        id: "resolved-docker-container",
        inspect: async () => ({ Config: { Env: ["RCON_PASSWORD=credential", "TELNET_PASSWORD=credential"] } }),
      } as unknown as Container;
      const pending = executeGameCommand(container, { state: "running", labels: {} }, adapter, "status", {
        stdout: () => {}, stderr: () => {}, system: () => {},
      });
      await f.input.promise;
      expect(f.create.mock.calls[0][0].HostConfig?.NetworkMode).toBe("container:resolved-docker-container");
      expect(JSON.parse(decodeText(concatBytes(f.stream.received)))).toMatchObject({
        transport: adapter.transport, port: adapter.defaultPort,
      });
      f.send({ type: "result", response: "Ready" });
      f.stream.close();
      await pending;
    });
  }

  for (const transport of ["source-rcon", "rust-webrcon", "telnet"] as const) {
    it(`uses private stdin and the authorized Docker namespace for ${transport}`, async () => {
      const f = fixture();
      const pending = executeNetworkConsoleHelper("physical-container", { ...request, transport });
      await f.input.promise;
      const options: ContainerCreateOptions = f.create.mock.calls[0][0];
      expect(options.HostConfig).toMatchObject({
        NetworkMode: "container:physical-container", ReadonlyRootfs: true, AutoRemove: true,
        CapDrop: ["ALL"], SecurityOpt: ["no-new-privileges"],
      });
      expect(options.Healthcheck).toEqual({ Test: ["NONE"] });
      expect(options.Env).toBeUndefined();
      expect(JSON.stringify(options)).not.toContain(request.password);
      expect(JSON.parse(decodeText(concatBytes(f.stream.received)))).toEqual({ ...request, transport });
      f.send({ type: "authorize" });
      f.send({ type: "result", response: "Ready 🐉" });
      f.stream.close();
      expect(await pending).toBe("Ready 🐉");
      expect(decodeText(concatBytes(f.stream.received))).toContain('{"type":"authorized"}');
      expect(f.remove).toHaveBeenCalledWith({ force: true, v: true });
      expect(f.stream.cancelled).toBe(true);
    });
  }

  for (const cause of ["timeout", "disconnect", "revocation"] as const) {
    it(`removes the helper and holds the server lock through ${cause} cleanup`, async () => {
      const f = fixture();
      const removed = deferred<void>();
      f.remove.mockImplementation(async () => { f.removing.resolve(); await removed.promise; });
      const abort = new AbortController();
      let allowed = true;
      const expire = deadline(10_000);
      const pending = withLocks(["console-helper-test"], () => executeNetworkConsoleHelper(
        "physical-container", request,
        () => { if (!allowed) throw new Error("Access changed"); }, abort.signal,
      ));
      let settled = false;
      const result = pending.then(() => { settled = true; }, () => { settled = true; });
      await f.input.promise;
      if (cause === "timeout") await expire();
      else if (cause === "disconnect") abort.abort();
      else { allowed = false; f.send({ type: "authorize" }); }
      await f.removing.promise;
      expect(settled).toBe(false);
      expect(() => acquireLocks(["console-helper-test"])).toThrow();
      expect(f.stream.cancelled).toBe(true);
      removed.resolve();
      await result;
      await expect(pending).rejects.toThrow(cause === "timeout" ? /timed out/ : cause === "disconnect" ? /cancelled/ : /Access changed/);
      const release = acquireLocks(["console-helper-test"]);
      release();
    });
  }

  it("keeps retrying cleanup through a daemon outage", async () => {
    const f = fixture();
    const retry = deferred<void>();
    spyOn(Bun, "sleep").mockImplementation(async () => { await retry.promise; });
    f.remove.mockRejectedValueOnce(new Error("daemon unavailable"))
      .mockRejectedValueOnce(new Error("daemon unavailable"));
    const abort = new AbortController();
    const pending = executeNetworkConsoleHelper("physical-container", request, undefined, abort.signal);
    await f.input.promise;
    abort.abort();
    await Promise.resolve();
    retry.resolve();
    await expect(pending).rejects.toThrow(/cancelled/);
    expect(f.remove).toHaveBeenCalledTimes(3);
  });

  for (const cause of ["timeout", "disconnect"] as const) {
    it(`removes a late-created helper after preparation ${cause} without starting it`, async () => {
      const f = fixture();
      const created = deferred<Container>();
      const creating = deferred<void>();
      f.create.mockImplementation(async () => { creating.resolve(); return created.promise; });
      const expire = deadline(5_000);
      const abort = new AbortController();
      const pending = executeNetworkConsoleHelper("physical-container", request, undefined, abort.signal);
      await creating.promise;
      if (cause === "timeout") await expire(); else abort.abort();
      created.resolve(f.helper);
      await expect(pending).rejects.toThrow(cause === "timeout" ? /preparation timed out/ : /cancelled/);
      expect(f.start).not.toHaveBeenCalled();
      expect(f.remove).toHaveBeenCalled();
    });
  }

  it("cleans up a possibly-created helper by its unique name when Docker loses the response", async () => {
    const f = fixture();
    f.create.mockRejectedValue(new Error("lost response"));
    const get = spyOn(docker, "getContainer").mockReturnValue(f.helper);
    await expect(executeNetworkConsoleHelper("physical-container", request)).rejects.toThrow(/lost response/);
    expect(get.mock.calls[0][0]).toMatch(/^ludock-console-[0-9a-f-]+$/);
    expect(f.remove).toHaveBeenCalled();
    expect(f.start).not.toHaveBeenCalled();
  });

  it("removes a helper after an attachment timeout and aborts the late stream", async () => {
    const f = fixture();
    const attaching = deferred<void>();
    const attached = deferred<StreamFixture>();
    spyOn(f.helper, "attach").mockImplementation(async () => {
      attaching.resolve();
      return (await attached.promise).connection;
    });
    const expire = deadline(5_000);
    const pending = executeNetworkConsoleHelper("physical-container", request);
    await attaching.promise;
    await expire();
    await expect(pending).rejects.toThrow(/preparation timed out/);
    expect(f.remove).toHaveBeenCalled();
    expect(f.start).not.toHaveBeenCalled();
    attached.resolve(f.stream);
    await Promise.resolve();
    await Promise.resolve();
    expect(f.stream.cancelled).toBe(true);
  });

  for (const failure of ["start", "stream", "error", "oversized", "eof"] as const) {
    it(`cleans up after ${failure} failure without returning output`, async () => {
      const f = fixture();
      if (failure === "start") f.start.mockRejectedValue(new Error("start failed"));
      const pending = executeNetworkConsoleHelper("physical-container", request);
      const rejected = pending.catch(error => error as Error);
      if (failure !== "start") {
        await f.input.promise;
        if (failure === "stream") f.stream.fail(new Error("stream failed"));
        else {
          if (failure === "error") f.send({ type: "error" });
          if (failure === "oversized") f.send({ type: "result", response: "x".repeat(MAX_NETWORK_CONSOLE_BYTES + 65_537) });
          f.stream.close();
        }
      }
      expect(await rejected).toBeInstanceOf(Error);
      expect(f.remove).toHaveBeenCalled();
    });
  }

  it("rechecks access during silent commands", async () => {
    const f = fixture();
    let check!: () => void;
    const original = globalThis.setInterval;
    spyOn(globalThis, "setInterval").mockImplementation(((callback: () => void, delay?: number) => {
      if (delay === 250) check = callback;
      return original(callback, delay);
    }) as typeof setInterval);
    let allowed = true;
    const pending = executeNetworkConsoleHelper("physical-container", request, () => {
      if (!allowed) throw new Error("Access changed");
    });
    await f.input.promise;
    allowed = false;
    check();
    await expect(pending).rejects.toThrow(/Console access changed/);
    expect(f.remove).toHaveBeenCalled();
  });

  it("keeps helper output on the existing redaction path", async () => {
    const f = fixture();
    const adapter = resolveGameConsoleAdapter({ gameType: "factorio", labels: {} })!;
    const container = {
      id: "physical-container",
      inspect: async () => ({ Config: { Env: [`RCON_PASSWORD=${request.password}`] } }),
    } as unknown as Container;
    let output = "";
    const redactor = new ConsoleOutputRedactor([request.password], value => { output += value; });
    const pending = executeGameCommand(container, { state: "running", labels: {} }, adapter, "status", {
      stdout: value => redactor.push(value), stderr: () => {}, system: () => {}, complete: () => redactor.end(),
    });
    await f.input.promise;
    f.send({ type: "result", response: `Ready ${request.password}` });
    f.stream.close();
    await pending;
    expect(output).toBe("Ready [redacted]");
  });
});
