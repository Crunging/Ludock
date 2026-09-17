// Bun's stream adapter is used only to reproduce the historical implementation.
// eslint-disable-next-line no-restricted-imports -- Benchmark the pre-Web-Stream baseline under Bun; never shipped.
import { PassThrough, Readable } from "node:stream";
import { rm } from "node:fs/promises";

const [source, scenario] = process.argv.slice(2);
const { DockerTransport } = await import(source + "/docker-transport.ts");
const streams = await import(source + "/docker-stream.ts");
const native = typeof streams.dockerStdout === "function";
const managed = native ? (await import(source + "/managed-readable.ts")).managedReadable : null;
const total = (scenario === "slow-consumer" ? 32 : 256) * 1024 * 1024;
const frame = new Uint8Array(65_536 + 8).fill(97);
frame.fill(0, 0, 8);
frame[0] = 1;
new DataView(frame.buffer).setUint32(4, 65_536);
const socketPath = "/tmp/ludock-transfer-" + crypto.randomUUID() + ".sock";
const upgrade = new TextEncoder().encode("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n");
const sockets = new Set();
function flush(socket) {
  const state = socket.data;
  while (state.bytes) {
    const written = socket.write(state.bytes, state.offset, state.bytes.length - state.offset);
    if (written <= 0) return;
    state.offset += written;
    if (state.offset < state.bytes.length) return;
    if (state.bytes === frame) state.sent += 65_536;
    state.offset = 0;
    state.bytes = state.sent < total ? frame : null;
  }
  socket.end();
}
const server = Bun.listen({ unix: socketPath, socket: {
  open(socket) { sockets.add(socket); socket.data = { started: false, sent: 0, offset: 0, bytes: upgrade }; },
  data(socket) { if (!socket.data.started) { socket.data.started = true; flush(socket); } },
  drain(socket) { if (socket.data.started) flush(socket); },
  close(socket) { sockets.delete(socket); },
  error(socket, error) { socket.terminate(); console.error(error); },
} });
let peakRss = process.memoryUsage.rss();
const sampler = setInterval(() => { peakRss = Math.max(peakRss, process.memoryUsage.rss()); }, 2);
let connection;
try {
  const start = performance.now();
  connection = await new DockerTransport(socketPath).hijack("/fixture");
  let body, completed;
  if (native) {
    const output = managed(streams.dockerStdout(connection.readable), { cleanup() { connection.abort(); } });
    body = output.stream;
    completed = output.completed;
  } else {
    const output = new PassThrough();
    completed = streams.demuxDockerStream(connection, output).then(() => output.end());
    body = Readable.toWeb(output);
  }
  let received = 0;
  for await (const value of new Response(body).body) {
    received += value.length;
    if (value[0] !== 97 || value.at(-1) !== 97) throw new Error("Corrupt transfer");
    if (scenario === "slow-consumer") await Bun.sleep(1);
  }
  await completed;
  if (received !== total) throw new Error("Truncated transfer");
  const ms = performance.now() - start;
  peakRss = Math.max(peakRss, process.memoryUsage.rss());
  console.log(JSON.stringify({ bun: Bun.version, ms, bytes: received, mibPerSecond: total / 1048576 / (ms / 1000), peakRssMiB: peakRss / 1048576 }));
} finally {
  clearInterval(sampler);
  if (native) connection?.abort();
  else connection?.destroy();
  for (const socket of sockets) socket.terminate();
  server.stop(true);
  await rm(socketPath, { force: true });
}
