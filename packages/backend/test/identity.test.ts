import assert from "node:assert/strict";
import { afterAll as after, beforeEach, describe, it } from "bun:test";
import { closeDatabase, getDatabase } from "../src/database.js";
import {
  assertObservedServerBinding,
  bindingFingerprint,
  externalServerIdentity,
  getDockerHostId,
  getLogicalServer,
  listLogicalServers,
  reconcileServers,
  resolveServerBinding,
  reviewServerBinding,
  type ServerObservation,
} from "../src/identity.js";

process.env.LUDOCK_DB_PATH = ":memory:";
beforeEach(() => closeDatabase());
after(() => closeDatabase());

function observation(
  overrides: Partial<ServerObservation> = {},
): ServerObservation {
  return {
    containerId: "docker-a",
    name: "minecraft-1",
    displayName: "Minecraft",
    gameType: "minecraft",
    mounts: [
      {
        type: "bind",
        source: "/srv/minecraft",
        destination: "/data",
        writable: true,
      },
    ],
    gameConfiguration: { "ludock.console": "minecraft-rcon" },
    ...overrides,
  };
}

describe("logical server identities", () => {
  it("preserves persisted fingerprints across the hashing implementation change", () => {
    assert.equal(
      bindingFingerprint(observation()),
      "c0c7db1b3d3d4e526b3b86f116b918a80272cf5f3cfdcde8bc6696fac968ce99",
    );
  });

  it("reattaches ordinary recreations to the same UUID and revises observed-container attribution", () => {
    const original = reconcileServers([observation()], { now: 100 })[0];
    const recreated = reconcileServers(
      [observation({ containerId: "docker-b" })],
      { now: 200 },
    )[0];
    assert.equal(recreated.id, original.id);
    assert.equal(recreated.status, "active");
    assert.equal(recreated.bindingRevision, original.bindingRevision + 1);
    assert.equal(recreated.containerId, "docker-b");
    assert.equal(recreated.firstSeenAt, 100);
    assert.equal(recreated.lastSeenAt, 200);
    assert.throws(
      () => resolveServerBinding(recreated.id, original.bindingRevision),
      /binding changed/,
    );
    assert.deepEqual(
      getDatabase()
        .prepare("SELECT container_id FROM server_bindings ORDER BY id")
        .all()
        .map((row) => row.container_id),
      ["docker-a", "docker-b"],
    );
  });

  it("uses Compose project/service/replica rather than Docker container name", () => {
    const compose = {
      project: "games",
      service: "minecraft",
      containerNumber: "1",
    };
    const original = reconcileServers([observation({ compose })])[0];
    const recreated = reconcileServers([
      observation({
        compose,
        containerId: "replacement",
        name: "renamed-by-compose",
      }),
    ])[0];
    assert.equal(recreated.id, original.id);
    assert.equal(recreated.externalIdentity, "compose:games:minecraft:1");
    assert.equal(recreated.status, "active");
  });

  it("keeps missing history and assigns a new UUID to standalone renames", () => {
    const original = reconcileServers([observation()])[0];
    const result = reconcileServers([observation({ name: "different-name" })]);
    assert.equal(result.length, 2);
    assert.equal(getLogicalServer(original.id)?.status, "missing");
    assert.equal(getLogicalServer(original.id)?.containerId, null);
    assert.ok(
      result.some(
        (server) => server.id !== original.id && server.status === "active",
      ),
    );
    assert.throws(
      () => resolveServerBinding(original.id),
      /binding is unavailable/,
    );
  });

  it("scopes the same identity to separate persisted Docker hosts", () => {
    const localHost = getDockerHostId();
    assert.equal(getDockerHostId(), localHost);
    const otherHost = getDockerHostId("another-daemon");
    const first = reconcileServers([observation()], { hostId: localHost })[0];
    const second = reconcileServers([observation()], { hostId: otherHost })[0];
    assert.notEqual(first.id, second.id);
    assert.equal(listLogicalServers().length, 2);
    assert.equal(getLogicalServer(first.id)?.status, "active");
  });

  it("shows duplicate Compose identities but refuses binding and history attribution", () => {
    const compose = { project: "p", service: "s", containerNumber: "1" };
    const original = reconcileServers([observation({ compose })])[0];
    const duplicate = reconcileServers([
      observation({ compose }),
      observation({ compose, containerId: "docker-b", name: "duplicate" }),
    ])[0];
    assert.equal(duplicate.id, original.id);
    assert.equal(duplicate.status, "ambiguous");
    assert.equal(duplicate.containerId, null);
    assert.throws(
      () => resolveServerBinding(duplicate.id),
      /binding is unavailable/,
    );
    assert.equal(
      getDatabase()
        .prepare("SELECT COUNT(*) AS count FROM server_bindings")
        .get()?.count,
      1,
    );
    const recovered = reconcileServers([observation({ compose })])[0];
    assert.equal(recovered.id, original.id);
    assert.equal(recovered.status, "active");
  });

  it("requires explicit review after mount, game, registration, or configuration changes", () => {
    const original = reconcileServers([observation()])[0];
    const changed = observation({
      mounts: [
        {
          type: "bind",
          source: "/srv/unrelated-world",
          destination: "/data",
          writable: true,
        },
      ],
    });
    const pending = reconcileServers([changed])[0];
    assert.equal(pending.id, original.id);
    assert.equal(pending.status, "review_required");
    assert.equal(pending.bindingFingerprint, original.bindingFingerprint);
    assert.notEqual(pending.pendingFingerprint, original.bindingFingerprint);
    assert.throws(() => resolveServerBinding(pending.id), /review/);
    assert.throws(
      () => reviewServerBinding(pending.id, "stale-fingerprint"),
      /pending server binding changed/,
    );
    const reviewed = reviewServerBinding(
      pending.id,
      pending.pendingFingerprint!,
    );
    assert.equal(reviewed.status, "active");
    assert.equal(reviewed.bindingFingerprint, bindingFingerprint(changed));
    assert.ok(reviewed.bindingRevision > pending.bindingRevision);
    for (const candidate of [
      { ...changed, gameType: "factorio" },
      { ...changed, projectRegistrationId: "replacement-registration" },
      { ...changed, gameConfiguration: { "ludock.console": "different" } },
    ]) {
      assert.notEqual(
        bindingFingerprint(candidate),
        reviewed.bindingFingerprint,
      );
    }
  });

  it("keeps review suspension across disappearance and apparent reversion", () => {
    const original = reconcileServers([observation()])[0];
    reconcileServers([observation({ gameType: "factorio" })]);
    reconcileServers([]);
    const reappeared = reconcileServers([observation()])[0];
    assert.equal(reappeared.id, original.id);
    assert.equal(reappeared.status, "review_required");
    assert.equal(reappeared.reviewRequired, true);
  });

  it("rejects an external replacement observed between authorization and mutation", () => {
    const server = reconcileServers([observation()])[0];
    assert.doesNotThrow(() =>
      assertObservedServerBinding(
        server.id,
        observation(),
        server.bindingRevision,
      ),
    );
    assert.throws(
      () =>
        assertObservedServerBinding(
          server.id,
          observation({ containerId: "external-new" }),
        ),
      /identity changed/,
    );
    assert.throws(
      () =>
        assertObservedServerBinding(
          server.id,
          observation({ gameType: "factorio" }),
        ),
      /identity changed/,
    );
    assert.throws(
      () =>
        assertObservedServerBinding(server.id, observation({ name: "other" })),
      /identity changed/,
    );
  });

  it("makes fingerprints independent of mount/config order and ignores read-only mounts", () => {
    const first = observation({
      gameConfiguration: { port: "1", password: "never-store-me" },
    });
    const second = {
      ...first,
      gameConfiguration: { password: "never-store-me", port: "1" },
      mounts: [
        {
          type: "bind",
          source: "/etc/config",
          destination: "/config",
          writable: false,
        },
        ...first.mounts,
      ],
    };
    assert.equal(bindingFingerprint(first), bindingFingerprint(second));
    reconcileServers([first]);
    const rows = getDatabase().prepare("SELECT * FROM logical_servers").all();
    assert.equal(JSON.stringify(rows).includes("never-store-me"), false);
    assert.equal(JSON.stringify(rows).includes("/srv/minecraft"), false);
  });

  it("rejects invalid or partial observations before mutating the snapshot", () => {
    const original = reconcileServers([observation()])[0];
    assert.throws(
      () => reconcileServers([observation(), observation()]),
      /Duplicate container/,
    );
    assert.throws(
      () =>
        reconcileServers([
          observation({
            compose: { project: "games", service: "", containerNumber: "1" },
          }),
        ]),
      /Incomplete Compose/,
    );
    assert.equal(getLogicalServer(original.id)?.status, "active");
    assert.notEqual(
      externalServerIdentity(
        observation({
          compose: { project: "a:b", service: "c", containerNumber: "1" },
        }),
      ),
      externalServerIdentity(
        observation({
          compose: { project: "a", service: "b:c", containerNumber: "1" },
        }),
      ),
    );
  });
});
