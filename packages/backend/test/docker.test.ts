import { rejectedBy } from "./fixtures/errors.js";
import { expect, afterEach, describe, it } from "bun:test";
import type { DockerContainerId } from "@ludock/shared";
import { docker } from "../src/docker-client.js";
import { parseAddressLabel } from "../src/discovery.js";
import {
  changeContainerState,
  getContainer,
  getManagedContainerObservation,
  getDiscoveryDiagnostics,
  listManagedContainerObservations,
} from "../src/docker.js";

/** Container IDs arrive from untrusted sources; the functions under test validate them. */
const untrusted = (id: string) => id as DockerContainerId;

const originalGetContainer = docker.getContainer.bind(docker);
const originalListContainers = docker.listContainers.bind(docker);

afterEach(() => {
  docker.getContainer = originalGetContainer;
  docker.listContainers = originalListContainers;
});

function inspectFixture(
  image = "itzg/minecraft-server:latest",
  labels: Record<string, string> = {},
) {
  return {
    Id: "a".repeat(64),
    Name: "/minecraft",
    Config: {
      Image: image,
      Labels: labels,
      Env: ["RCON_PASSWORD=private-password"],
    },
    State: { Status: "running" },
    Mounts: [
      {
        Type: "volume",
        Name: "game-data",
        Source: "/var/lib/docker/volumes/game-data/_data",
        Destination: "/data",
        RW: true,
      },
    ],
    NetworkSettings: { Ports: {} },
    Created: "2026-01-01T00:00:00.000Z",
  };
}

function listFixture(
  image: string,
  labels: Record<string, string> = {},
  id = "a".repeat(64),
) {
  return {
    Id: id,
    Names: ["/minecraft"],
    Image: image,
    Labels: labels,
    State: "running",
    Status: "Up 1 hour",
    Ports: [],
    Created: 1_767_225_600,
    Mounts: [],
  };
}

describe("automatic discovery boundary", () => {
  it("returns fixed diagnostics without untrusted label values", async () => {
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server", {
        "ludock.enable": "private-token",
      }),
      listFixture("custom/server"),
    ]) as unknown as typeof docker.listContainers;
    const diagnostics = await getDiscoveryDiagnostics();
    expect(diagnostics.length).toBe(1);
    expect(diagnostics[0].code).toBe("INVALID_ENABLE_LABEL");
    expect(JSON.stringify(diagnostics).includes("private-token")).toBe(false);
  });

  it("reports an invalid address label without echoing its value", async () => {
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server", { "ludock.address": "https://private-token.example" }),
      listFixture("itzg/minecraft-server", { "ludock.address": "play.example.com:25565" }, "b".repeat(64)),
    ]) as unknown as typeof docker.listContainers;
    const diagnostics = await getDiscoveryDiagnostics();
    expect(diagnostics.map((item) => item.code)).toEqual(["INVALID_ADDRESS_LABEL"]);
    expect(JSON.stringify(diagnostics).includes("private-token")).toBe(false);
  });

  it("keeps host mounts, Compose identity and environment out of public metadata", async () => {
    const info = inspectFixture("didstopia/rust-server", {
      "ludock.console": "rust-webrcon",
      "ludock.private-token": "hidden-token",
      "com.docker.compose.project": "games",
      "com.docker.compose.service": "rust",
      "com.docker.compose.container-number": "1",
    });
    docker.getContainer = (() => ({
      inspect: async () => info,
    })) as unknown as typeof docker.getContainer;
    const { container } =
      await getManagedContainerObservation(untrusted("minecraft"));
    for (const secret of [
      "private-password",
      "hidden-token",
      "/var/lib/docker",
      "com.docker.compose",
    ]) {
      expect(JSON.stringify(container).includes(secret), secret).toBe(false);
    }
  });

  it("parses ludock.address as the exact address players type", () => {
    expect(parseAddressLabel(" Play.Example.com ")).toEqual({ host: "play.example.com", port: null });
    expect(parseAddressLabel("203.0.113.10:30000")).toEqual({ host: "203.0.113.10", port: 30000 });
    expect(parseAddressLabel("[2001:db8::1]:2456")).toEqual({ host: "2001:db8::1", port: 2456 });
    expect(parseAddressLabel("[2001:db8::1]")).toEqual({ host: "2001:db8::1", port: null });
    expect(parseAddressLabel("2001:db8::1")).toEqual({ host: "2001:db8::1", port: null });
    for (const invalid of [
      undefined, "", "https://play.example.com", "play.example.com:0", "play.example.com:65536",
      "play.example.com:25565/udp", "[play.example.com]:25565", "two words", "host:port:1",
    ]) expect(parseAddressLabel(invalid), String(invalid)).toBeNull();
  });

  it("uses a valid ludock.address instead of detection, without affecting identity", async () => {
    const info = {
      ...inspectFixture("itzg/minecraft-server", { "ludock.address": "mc.example.com" }),
      NetworkSettings: { Ports: { "25565/tcp": [{ HostIp: "0.0.0.0", HostPort: "25565" }] } },
    };
    docker.getContainer = (() => ({ inspect: async () => info })) as unknown as typeof docker.getContainer;
    const { container, observation } = await getManagedContainerObservation(untrusted("minecraft"));
    expect(container.addressLabel).toEqual({ host: "mc.example.com", port: null });
    expect(container.connectPort).toBe(25565);
    expect(JSON.stringify(observation).includes("ludock.address")).toBe(false);

    const invalid = { ...info, Config: { ...info.Config, Labels: { "ludock.address": "not an address" } } };
    docker.getContainer = (() => ({ inspect: async () => invalid })) as unknown as typeof docker.getContainer;
    expect((await getManagedContainerObservation(untrusted("minecraft"))).container.addressLabel).toBeNull();
  });

  it("offers the game's own port by protocol and never a loopback-only binding", async () => {
    const connectPortFor = async (image: string, labels: Record<string, string>, ports: Record<string, Array<{ HostIp: string; HostPort: string }>>) => {
      const info = { ...inspectFixture(image, labels), NetworkSettings: { Ports: ports } };
      docker.getContainer = (() => ({ inspect: async () => info })) as unknown as typeof docker.getContainer;
      return (await getManagedContainerObservation(untrusted("game"))).container.connectPort;
    };
    // TCP and UDP mappings of one container port are independent; Factorio uses UDP.
    expect(await connectPortFor("factoriotools/factorio", {}, {
      "34197/tcp": [{ HostIp: "0.0.0.0", HostPort: "30000" }],
      "34197/udp": [{ HostIp: "0.0.0.0", HostPort: "34197" }],
    })).toBe(34197);
    // The other protocol is never a substitute, even when the game's own is loopback-only.
    expect(await connectPortFor("factoriotools/factorio", {}, {
      "34197/udp": [{ HostIp: "127.0.0.1", HostPort: "34197" }],
      "34197/tcp": [{ HostIp: "0.0.0.0", HostPort: "34197" }],
    })).toBeNull();
    // A game port reachable only from the Docker host offers no address, not the RCON port.
    expect(await connectPortFor("itzg/minecraft-server", {}, {
      "25565/tcp": [{ HostIp: "127.0.0.1", HostPort: "25565" }, { HostIp: "::1", HostPort: "25565" }],
      "25575/tcp": [{ HostIp: "0.0.0.0", HostPort: "25575" }],
    })).toBeNull();
    // Other images skip loopback-only bindings when choosing their first port.
    expect(await connectPortFor("example/custom-game", { "ludock.enable": "true" }, {
      "8080/tcp": [{ HostIp: "127.0.0.1", HostPort: "8080" }],
      "7777/udp": [{ HostIp: "", HostPort: "7777" }],
    })).toBe(7777);
  });

  it("reports health, uptime, exits, and the port players connect to", async () => {
    const startedAt = "2026-09-15T10:00:00.000Z";
    const running = {
      ...inspectFixture(),
      State: { Status: "running", StartedAt: startedAt, FinishedAt: "0001-01-01T00:00:00Z", ExitCode: 0, OOMKilled: false, Health: { Status: "starting" } },
      NetworkSettings: { Ports: {
        "25575/tcp": [{ HostIp: "0.0.0.0", HostPort: "25575" }],
        "25565/tcp": [{ HostIp: "0.0.0.0", HostPort: "25566" }],
      } },
    };
    docker.getContainer = (() => ({ inspect: async () => running })) as unknown as typeof docker.getContainer;
    const { container } = await getManagedContainerObservation(untrusted("minecraft"));
    expect(container).toMatchObject({
      health: "starting",
      stateSince: Date.parse(startedAt),
      exit: null,
      gameName: "Minecraft",
      connectPort: 25566,
    });
    expect(container.gameConsole?.commands).toContainEqual({ label: "List players", command: "list" });

    const finishedAt = "2026-09-15T12:00:00.000Z";
    const crashed = {
      ...inspectFixture("example/custom-game", { "ludock.enable": "true" }),
      State: {
        Status: "exited", StartedAt: startedAt, FinishedAt: finishedAt, ExitCode: 1, OOMKilled: false,
        Health: { Status: "unhealthy" },
      },
      NetworkSettings: { Ports: { "7777/udp": [{ HostIp: "0.0.0.0", HostPort: "7777" }] } },
    };
    docker.getContainer = (() => ({ inspect: async () => crashed })) as unknown as typeof docker.getContainer;
    const { container: stopped } = await getManagedContainerObservation(untrusted("custom"));
    expect(stopped).toMatchObject({
      health: null,
      stateSince: Date.parse(finishedAt),
      exit: { code: 1, oomKilled: false },
      gameName: "Other game",
      connectPort: 7777,
    });
  });

  it("rechecks eligibility on inspection after listing", async () => {
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server"),
    ]) as unknown as typeof docker.listContainers;
    docker.getContainer = (() => ({
      inspect: async () =>
        inspectFixture("itzg/minecraft-server", { "ludock.enable": "false" }),
    })) as unknown as typeof docker.getContainer;
    expect(await listManagedContainerObservations()).toStrictEqual([]);
  });

  it("stops dispatching after failure and drains active reads before rejecting", async () => {
    const ids = Array.from({ length: 7 }, (_, index) =>
      index.toString(16).padStart(64, "0"),
    );
    docker.listContainers = (async () =>
      ids.map((id) => listFixture("itzg/minecraft-server", {}, id))
    ) as unknown as typeof docker.listContainers;
    const started: string[] = [];
    const inspections = new Map<string, {
      resolve: () => void;
      reject: (error: Error) => void;
    }>();
    docker.getContainer = ((id: string) => ({
      inspect: async () => {
        started.push(id);
        await new Promise<void>((resolve, reject) =>
          inspections.set(id, { resolve, reject }),
        );
        return { ...inspectFixture(), Id: id };
      },
    })) as unknown as typeof docker.getContainer;

    let settled = false;
    const pending = listManagedContainerObservations();
    void pending.then(
      () => { settled = true; },
      () => { settled = true; },
    );
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(started).toStrictEqual(ids.slice(0, 4));
    const failure = Object.assign(new Error("Docker failed"), { statusCode: 500 });
    inspections.get(ids[1])!.reject(failure);
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(settled).toBe(false);
    expect(started).toStrictEqual(ids.slice(0, 4));

    inspections.get(ids[2])!.resolve();
    inspections.get(ids[0])!.reject(new Error("Another inspection failed"));
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(settled).toBe(false);
    expect(started).toStrictEqual(ids.slice(0, 4));

    inspections.get(ids[3])!.resolve();
    await expect(await rejectedBy(pending)).toSatisfy((error) => error === failure);
    expect(settled).toBe(true);
    expect(started).toStrictEqual(ids.slice(0, 4));
  });
});

describe("managed container lifecycle boundary", () => {
  it("rejects starting an unmanaged container", async () => {
    let actionCalled = false;
    docker.getContainer = (() => ({
      inspect: async () => ({ Config: { Labels: {} } }),
      start: async () => { actionCalled = true; },
    })) as unknown as typeof docker.getContainer;

    await expect(changeContainerState(untrusted("unmanaged-id"), "start")).rejects.toMatchObject({
      statusCode: 403, code: "FORBIDDEN",
    });
    expect(actionCalled).toBe(false);
  });
});

describe("container identifier validation", () => {
  it("rejects path traversal before it reaches Docker", async () => {
    let reached = false;
    docker.getContainer = (() => {
      reached = true;
      return { inspect: async () => ({ Config: { Labels: {} } }) };
    }) as unknown as typeof docker.getContainer;

    // URL decoding can turn %2f into "/", allowing an identifier to escape
    // /containers/<id>/json unless it is rejected before any daemon request.
    const id = untrusted("../../info");
    for (const run of [
      () => getManagedContainerObservation(id),
      () => changeContainerState(id, "start"),
      async () => getContainer(id),
    ]) {
      await expect(run()).rejects.toMatchObject({
        code: "INVALID_CONTAINER_ID", statusCode: 400,
      });
    }
    expect(reached, "Docker must never be called").toBe(false);
  });
});
