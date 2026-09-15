import { fixtureBytes } from "./fixtures/bytes.js";
import { concatBytes, decodeText } from "../src/bytes.js";
import { describe, expect, it } from "bun:test";
import { docker, type Container } from "../src/docker-client.js";
import { demuxDockerStream } from "../src/docker-stream.js";
import { dockerEventDecoder } from "../src/events.js";
import { DEFAULT_HELPER_IMAGE } from "../src/runtime-images.js";

describe.skipIf(process.env.LUDOCK_DOCKER_TESTS !== "1")("Native Docker client acceptance", () => {
  it("pulls a pinned image and preserves events, TTY logs, exec output, stdin, and lifecycle state", async () => {
    const containers: Container[] = [];
    let events: ReturnType<ReadableStream<Uint8Array>["getReader"]> | undefined;
    let eventWork: Promise<void> | undefined;
    try {
      await docker.ping();
      await docker.pull(DEFAULT_HELPER_IMAGE);
      const container = await docker.createContainer({
        Image: DEFAULT_HELPER_IMAGE,
        name: `ludock-client-${crypto.randomUUID()}`,
        Entrypoint: ["bun", "-e"],
        Cmd: ["process.on('SIGTERM', () => process.exit(0)); process.stdin.on('data', data => process.stdout.write('command:' + data)); console.log('ready'); setInterval(() => {}, 1000);"],
        OpenStdin: true,
        StdinOnce: false,
        Labels: { "ludock.enable": "false" },
        HostConfig: { Init: true },
      });
      containers.push(container);
      const actions = new Set<string>();
      events = (await docker.getEvents({ filters: { type: ["container"], container: [container.id] } })).getReader();
      const decode = dockerEventDecoder(event => { if (event.Action) actions.add(event.Action); });
      eventWork = (async () => {
        while (true) {
          const { value, done } = await events!.read();
          if (done) return;
          decode(value);
        }
      })();
      void eventWork.catch(() => {});
      await container.start();
      await until(() => actions.has("start"));
      const info = await container.inspect();
      expect(info.State.Running).toBe(true);
      expect(info.Image).toBe((await docker.getImage(DEFAULT_HELPER_IMAGE).inspect()).Id);
      expect((await container.stats({ stream: false })).memory_stats.limit).toBeGreaterThan(0);

      const execution = await container.exec({
        Cmd: ["bun", "-e", "const input = await Bun.stdin.bytes(); console.log(input.length); console.error('stderr-marker');"],
        AttachStdin: true, AttachStdout: true, AttachStderr: true,
      });
      const stream = await execution.start();
      let out = "", err = "";
      const completed = demuxDockerStream(stream.readable, data => { out += decodeText(fixtureBytes(data)); }, data => { err += decodeText(fixtureBytes(data)); });
      const writer = stream.writable.getWriter();
      await writer.write(new Uint8Array(2 * 1024 * 1024).fill(97));
      await writer.close();
      writer.releaseLock();
      await completed;
      expect(out.trim()).toBe("2097152");
      expect(err.trim()).toBe("stderr-marker");
      expect(await execution.inspect()).toMatchObject({ Running: false, ExitCode: 0 });

      const attached = await container.attach({ stream: true, stdin: true, stdout: false, stderr: false });
      try {
        const writer = attached.writable.getWriter();
        await writer.write(fixtureBytes("fixture-command\n"));
        writer.releaseLock();
      } finally { attached.abort(); }
      await until(async () => {
        const logs = await container.logs({ follow: false, stdout: true, stderr: true });
        let content = "";
        const output = (data: Uint8Array) => { content += decodeText(fixtureBytes(data)); };
        await demuxDockerStream(logs, output, output);
        return content.includes("command:fixture-command");
      });
      expect((await container.inspect()).Config.OpenStdin).toBe(true);
      await container.restart();
      expect((await container.inspect()).State.Running).toBe(true);
      await container.stop({ t: 1 });
      expect((await container.wait()).StatusCode).toBe(0);
      await until(() => actions.has("die"));
      expect((await container.inspect()).State.Running).toBe(false);

      const tty = await docker.createContainer({
        Image: DEFAULT_HELPER_IMAGE, Entrypoint: ["bun", "-e"],
        Cmd: ["console.log('tty-marker')"], Tty: true,
        Labels: { "ludock.enable": "false" },
      });
      containers.push(tty);
      await tty.start();
      expect((await tty.wait()).StatusCode).toBe(0);
      const logs = await tty.logs({ follow: false, stdout: true, stderr: true });
      const chunks: Uint8Array[] = [];
      for await (const data of logs) chunks.push(data as Uint8Array);
      expect(decodeText(concatBytes(chunks)).trim()).toBe("tty-marker");
    } finally {
      await events?.cancel().catch(() => {});
      await eventWork?.catch(() => {});
      events?.releaseLock();
      for (const container of containers.reverse()) await container.remove({ force: true });
    }
  }, 120_000);
});

async function until(check: () => boolean | Promise<boolean>): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await Bun.sleep(50);
  }
  throw new Error("Docker fixture did not reach the expected state");
}
