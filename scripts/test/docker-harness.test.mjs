import { afterEach, describe, expect, it, spyOn } from "bun:test";
import { createTestDocker } from "../test-docker.mjs";

const encoder = new TextEncoder();
let spawn;
afterEach(() => spawn?.mockRestore());

function mockDocker({ endpoint = "unix:///tmp/test.sock", mountedId = "test-daemon" } = {}) {
  spawn = spyOn(Bun, "spawnSync").mockImplementation((args) => {
    let output = "";
    if (args.includes("context")) output = JSON.stringify(endpoint);
    else if (args.includes("run")) output = mountedId;
    else if (args.includes("info")) output = JSON.stringify({ ID: "test-daemon", OperatingSystem: "Alpine Linux" });
    return { exitCode: 0, stdout: encoder.encode(output), stderr: new Uint8Array() };
  });
  return spawn;
}

describe("Docker acceptance daemon selection", () => {
  it("pins the chosen context and mounts its Unix socket before fixture commands", () => {
    const calls = mockDocker();
    const harness = createTestDocker({ environment: { DOCKER_CONTEXT: "test", DOCKER_HOST: "unix:///wrong.sock" } });
    expect(calls.mock.calls[0][0]).toEqual([
      "docker", "context", "inspect", "test", "--format", "{{json .Endpoints.docker.Host}}",
    ]);
    expect(harness.socketArguments).toEqual([
      "--mount", "type=bind,source=/tmp/test.sock,target=/var/run/docker.sock,readonly",
    ]);
    const preflight = calls.mock.calls[2][0];
    expect(preflight.slice(0, 4)).toEqual(["docker", "--host", "unix:///tmp/test.sock", "run"]);
    expect(preflight).toContain("--pull=never");
    expect(preflight).toContain("{{.ID}}");
    expect(preflight).not.toContain("pull");
    harness.docker("volume", "create", "fixture");
    expect(calls.mock.calls[3][0]).toEqual(["docker", "--host", "unix:///tmp/test.sock", "volume", "create", "fixture"]);
    expect(calls.mock.calls[3][1].env.DOCKER_CONTEXT).toBeUndefined();
    expect(calls.mock.calls[3][1].env.DOCKER_HOST).toBeUndefined();
  });

  it("fails on a different daemon without falling back or running acceptance work", () => {
    const calls = mockDocker({ mountedId: "production-daemon" });
    expect(() => createTestDocker({ environment: {} })).toThrow("different daemon");
    expect(calls.mock.calls).toHaveLength(3);
    expect(calls.mock.calls.flatMap(([args]) => args)).not.toContain("/var/run/docker.sock:/var/run/docker.sock");
  });

  it("rejects remote contexts and invalid socket mappings before a probe container starts", () => {
    const calls = mockDocker({ endpoint: "ssh://remote-host" });
    expect(() => createTestDocker({ environment: {} })).toThrow("local Unix-socket");
    expect(calls.mock.calls).toHaveLength(1);
    calls.mockClear();
    expect(() => createTestDocker({ environment: {
      DOCKER_HOST: "unix:///tmp/test.sock", LUDOCK_TEST_DOCKER_SOCKET: "/tmp/socket,unexpected=option",
    } })).toThrow("absolute daemon-visible");
    expect(calls.mock.calls).toHaveLength(1);
  });
});
