import assert from "node:assert/strict";
import { afterEach, describe, it } from "bun:test";
import {
  getContainer,
  getDockerInstance,
  getManagedContainerObservation,
  getDiscoveryDiagnostics,
  listManagedContainerObservations,
  restartContainer,
  startContainer,
  stopContainer,
} from "../src/docker.js";

const docker = getDockerInstance();
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
  const cases: Array<{
    name: string;
    image: string;
    labels: Record<string, string>;
    included: boolean;
  }> = [
    {
      name: "recognized",
      image: "itzg/minecraft-server",
      labels: {},
      included: true,
    },
    {
      name: "mirror",
      image: "registry.private:5000/cache/itzg/minecraft-server:latest",
      labels: {},
      included: true,
    },
    {
      name: "opt-out",
      image: "itzg/minecraft-server",
      labels: { "ludock.enable": " FALSE " },
      included: false,
    },
    {
      name: "invalid",
      image: "itzg/minecraft-server",
      labels: { "ludock.enable": "yes" },
      included: false,
    },
    {
      name: "one-off",
      image: "itzg/minecraft-server",
      labels: { "com.docker.compose.oneoff": "True" },
      included: false,
    },
    {
      name: "enabled one-off",
      image: "custom/server",
      labels: { "com.docker.compose.oneoff": "True", "ludock.enable": "TRUE" },
      included: true,
    },
    {
      name: "explicit unknown",
      image: "custom/server",
      labels: { "ludock.enable": " true " },
      included: true,
    },
    { name: "unknown", image: "custom/server", labels: {}, included: false },
  ];

  for (const entry of cases) {
    it(`applies the same list, detail and mutation policy to ${entry.name}`, async () => {
      const invoked: string[] = [];
      docker.listContainers = (async (options: unknown) => {
        assert.deepEqual(options, { all: true });
        return [listFixture(entry.image, entry.labels)];
      }) as unknown as typeof docker.listContainers;
      docker.getContainer = (() => ({
        inspect: async () => inspectFixture(entry.image, entry.labels),
        start: async () => invoked.push("start"),
        stop: async () => invoked.push("stop"),
        restart: async () => invoked.push("restart"),
      })) as unknown as typeof docker.getContainer;

      assert.equal(
        (await listManagedContainerObservations()).length,
        entry.included ? 1 : 0,
      );
      for (const action of [
        getManagedContainerObservation,
        startContainer,
        stopContainer,
        restartContainer,
      ]) {
        if (entry.included) await action("minecraft");
        else await assert.rejects(action("minecraft"), { code: "FORBIDDEN" });
      }
      assert.deepEqual(
        invoked,
        entry.included ? ["start", "stop", "restart"] : [],
      );
    });
  }

  it("returns fixed diagnostics without untrusted label values", async () => {
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server", {
        "ludock.enable": "private-token",
      }),
      listFixture("custom/server"),
    ]) as unknown as typeof docker.listContainers;
    const diagnostics = await getDiscoveryDiagnostics();
    assert.equal(diagnostics.length, 1);
    assert.equal(diagnostics[0].code, "INVALID_ENABLE_LABEL");
    assert.equal(JSON.stringify(diagnostics).includes("private-token"), false);
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
    const { container, observation } =
      await getManagedContainerObservation("minecraft");
    assert.deepEqual(observation.compose, {
      project: "games",
      service: "rust",
      containerNumber: "1",
    });
    assert.equal(observation.mounts[0].name, "game-data");
    assert.equal(
      observation.mounts[0].source,
      "/var/lib/docker/volumes/game-data/_data",
    );
    assert.equal(
      observation.gameConfiguration?.["env:RCON_PASSWORD"],
      "private-password",
    );
    for (const secret of [
      "private-password",
      "hidden-token",
      "/var/lib/docker",
      "com.docker.compose",
    ]) {
      assert.equal(JSON.stringify(container).includes(secret), false, secret);
    }
  });

  it("rechecks eligibility on inspection after listing", async () => {
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server"),
    ]) as unknown as typeof docker.listContainers;
    docker.getContainer = (() => ({
      inspect: async () =>
        inspectFixture("itzg/minecraft-server", { "ludock.enable": "false" }),
    })) as unknown as typeof docker.getContainer;
    assert.deepEqual(await listManagedContainerObservations(), []);
  });

  it("isolates incomplete Compose identities without breaking discovery", async () => {
    const labels = { "com.docker.compose.project": "games" };
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server", labels),
    ]) as unknown as typeof docker.listContainers;
    docker.getContainer = (() => ({
      inspect: async () => inspectFixture("itzg/minecraft-server", labels),
    })) as unknown as typeof docker.getContainer;
    assert.deepEqual(await listManagedContainerObservations(), []);
    assert.equal(
      (await getDiscoveryDiagnostics())[0]?.code,
      "INVALID_COMPOSE_IDENTITY",
    );
    await assert.rejects(startContainer("minecraft"), {
      code: "INVALID_COMPOSE_IDENTITY",
    });
  });

  it("gives explicit Compose one-offs standalone identity, separate from service replicas", async () => {
    const labels = {
      "ludock.enable": "true",
      "com.docker.compose.oneoff": "True",
      "com.docker.compose.project": "games",
      "com.docker.compose.service": "minecraft",
      "com.docker.compose.container-number": "1",
    };
    docker.getContainer = (() => ({
      inspect: async () => inspectFixture("itzg/minecraft-server", labels),
    })) as unknown as typeof docker.getContainer;
    const { observation } = await getManagedContainerObservation("minecraft");
    assert.equal(observation.compose, undefined);
    assert.equal(observation.name, "minecraft");
  });

  it("tolerates external removal during observation but propagates daemon failures", async () => {
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server"),
    ]) as unknown as typeof docker.listContainers;
    let statusCode = 404;
    docker.getContainer = (() => ({
      inspect: async () => {
        throw Object.assign(new Error("Docker failed"), { statusCode });
      },
    })) as unknown as typeof docker.getContainer;
    assert.deepEqual(await listManagedContainerObservations(), []);
    statusCode = 500;
    await assert.rejects(listManagedContainerObservations(), /Docker failed/);
  });

  it("bounds concurrent inspections and preserves list order with fresh observations", async () => {
    const ids = Array.from({ length: 7 }, (_, index) =>
      index.toString(16).padStart(64, "0"),
    );
    docker.listContainers = (async () => [
      listFixture("itzg/minecraft-server", { "ludock.enable": "false" }, "excluded"),
      ...ids.map((id) => listFixture("itzg/minecraft-server", {}, id)),
    ]) as unknown as typeof docker.listContainers;
    const started: string[] = [];
    const releases = new Map<string, () => void>();
    let active = 0;
    let peak = 0;
    docker.getContainer = ((id: string) => ({
      inspect: async () => {
        started.push(id);
        active++;
        peak = Math.max(peak, active);
        await new Promise<void>((resolve) => releases.set(id, resolve));
        active--;
        if (id === ids[2])
          throw Object.assign(new Error("Container removed"), { statusCode: 404 });
        return { ...inspectFixture(), Id: id, Name: `/current-${id}` };
      },
    })) as unknown as typeof docker.getContainer;

    const pending = listManagedContainerObservations();
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.deepEqual(started, ids.slice(0, 4));

    // A removed container releases capacity; later completions keep list order.
    releases.get(ids[2])!();
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.deepEqual(started, ids.slice(0, 5));
    assert.equal(active, 4);
    releases.get(ids[0])!();
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.deepEqual(started, ids.slice(0, 6));
    releases.get(ids[4])!();
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.deepEqual(started, ids);

    for (const release of releases.values()) release();
    const observations = await pending;
    const remaining = ids.filter((id) => id !== ids[2]);
    assert.equal(peak, 4);
    assert.equal(active, 0);
    assert.deepEqual(observations.map(({ container }) => container.id), remaining);
    assert.deepEqual(
      observations.map(({ observation }) => observation.name),
      remaining.map((id) => `current-${id}`),
    );
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
    assert.deepEqual(started, ids.slice(0, 4));
    const failure = Object.assign(new Error("Docker failed"), { statusCode: 500 });
    inspections.get(ids[1])!.reject(failure);
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.equal(settled, false);
    assert.deepEqual(started, ids.slice(0, 4));

    inspections.get(ids[2])!.resolve();
    inspections.get(ids[0])!.reject(new Error("Another inspection failed"));
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.equal(settled, false);
    assert.deepEqual(started, ids.slice(0, 4));

    inspections.get(ids[3])!.resolve();
    await assert.rejects(pending, (error) => error === failure);
    assert.equal(settled, true);
    assert.deepEqual(started, ids.slice(0, 4));
  });
});

describe("managed container lifecycle boundary", () => {
  for (const [action, run] of [
    ["start", startContainer],
    ["stop", stopContainer],
    ["restart", restartContainer],
  ] as const) {
    it(`allows ${action} for an opted-in container`, async () => {
      let actionCalled = false;
      const container = {
        inspect: async () => ({
          Config: { Labels: { "ludock.enable": "true" } },
        }),
        [action]: async () => {
          actionCalled = true;
        },
      };

      docker.getContainer = (() =>
        container) as unknown as typeof docker.getContainer;

      await run("managed-id");
      assert.equal(actionCalled, true);
    });

    it(`rejects ${action} for an unmanaged container`, async () => {
      let actionCalled = false;
      const container = {
        inspect: async () => ({ Config: { Labels: {} } }),
        [action]: async () => {
          actionCalled = true;
        },
      };

      docker.getContainer = (() =>
        container) as unknown as typeof docker.getContainer;

      await assert.rejects(
        run("unmanaged-id"),
        (error: Error & { statusCode?: number; code?: string }) => {
          assert.equal(error.statusCode, 403);
          assert.equal(error.code, "FORBIDDEN");
          return true;
        },
      );
      assert.equal(actionCalled, false);
    });
  }
});

describe("managed container image inference", () => {
  it("needs only the enable label for a recognized image", async () => {
    const container = {
      inspect: async () => ({
        Id: "a".repeat(64),
        Name: "/terraria",
        Config: {
          Image: "hexlo/terraria-server-docker:latest",
          Labels: { "ludock.enable": "true" },
        },
        State: { Status: "running" },
        Mounts: [
          {
            Type: "bind",
            Source: "/srv/terraria",
            Destination: "/root/.local/share/Terraria/Worlds",
            RW: true,
          },
        ],
        NetworkSettings: { Ports: {} },
        Created: "2026-01-01T00:00:00.000Z",
      }),
    };
    docker.getContainer = (() =>
      container) as unknown as typeof docker.getContainer;

    const { container: managed } = await getManagedContainerObservation("terraria");

    assert.equal(managed.gameType, "terraria");
    assert.equal(managed.gameConsole?.id, "stdin-console");
    assert.deepEqual(managed.fileRoots, [
      {
        id: "root-0",
        name: "Terraria worlds",
        path: "/root/.local/share/Terraria/Worlds",
      },
    ]);
  });
});

describe("container identifier validation", () => {
  // URL decoding can turn %2f into "/", so an identifier can carry "../" and escape
  // /containers/<id>/json. Reject these before constructing any daemon request.
  const hostile = [
    "../../info",
    "..//attacker.example",
    "../../127.0.0.1:9",
    "abc/def",
    "",
    ".hidden",
    "a".repeat(129),
    "id with spaces",
    "id\nnewline",
  ];

  for (const id of hostile) {
    it(`rejects ${JSON.stringify(id)} before it reaches Docker`, async () => {
      let reached = false;
      docker.getContainer = (() => {
        reached = true;
        return { inspect: async () => ({ Config: { Labels: {} } }) };
      }) as unknown as typeof docker.getContainer;

      for (const run of [
        () => getManagedContainerObservation(id),
        () => startContainer(id),
        async () => getContainer(id),
      ]) {
        await assert.rejects(
          run(),
          (error: Error & { statusCode?: number; code?: string }) => {
            assert.equal(error.code, "INVALID_CONTAINER_ID");
            assert.equal(error.statusCode, 400);
            return true;
          },
        );
      }
      assert.equal(reached, false, "Docker must never be called");
    });
  }

  it("still accepts real Docker IDs and names", () => {
    for (const id of ["a".repeat(64), "abc123", "my-server_1.0", "3f2b1a"]) {
      assert.doesNotThrow(() => getContainer(id));
    }
  });
});
