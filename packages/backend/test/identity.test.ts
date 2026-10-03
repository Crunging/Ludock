import { expect, afterAll as after, beforeEach, describe, it } from "bun:test";
import { closeDatabase, getDatabase } from "../src/database.js";
import {
  assertObservedServerBinding,
  bindingFingerprint,
  externalServerIdentity,
  getLogicalServer,
  reconcileServers,
  resolveServerBinding,
  reviewServerBinding,
  type ServerObservation,
} from "../src/identity.js";
import type { SQLQueryBindings } from "bun:sqlite";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";
beforeEach(() => {
  closeDatabase();
  process.env.LUDOCK_DB_PATH = ":memory:";
});
after(() => {
  closeDatabase();
});

function observation(
  overrides: Partial<ServerObservation> = {},
): ServerObservation {
  return {
    containerId: dockerId("docker-a"),
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
  it("reattaches ordinary recreations to the same UUID and revises observed-container attribution", () => {
    const original = reconcileServers([observation()], { now: 100 })[0];
    const recreated = reconcileServers(
      [observation({ containerId: dockerId("docker-b") })],
      { now: 200 },
    )[0];
    expect(recreated.id).toBe(original.id);
    expect(recreated.status).toBe("active");
    expect(recreated.bindingRevision).toBe(original.bindingRevision + 1);
    expect<string | null>(recreated.containerId).toBe("docker-b");
    expect(recreated.firstSeenAt).toBe(100);
    expect(recreated.lastSeenAt).toBe(200);
    expect(() => resolveServerBinding(recreated.id, original.bindingRevision)).toThrow(/binding changed/);
    expect(getDatabase()
        .prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT container_id FROM server_bindings ORDER BY id")
        .all()
        .map((row) => row.container_id)).toStrictEqual(["docker-a", "docker-b"]);
  });

  it("keeps missing history and assigns a new UUID to standalone renames", () => {
    const original = reconcileServers([observation()])[0];
    const result = reconcileServers([observation({ name: "different-name" })]);
    expect(result.length).toBe(2);
    expect(getLogicalServer(original.id)?.status).toBe("missing");
    expect(getLogicalServer(original.id)?.containerId).toBe(null);
    expect(result.some(
        (server) => server.id !== original.id && server.status === "active",
      )).toBeTruthy();
    expect(() => resolveServerBinding(original.id)).toThrow(/binding is unavailable/);
  });

  it("shows duplicate Compose identities but refuses binding and history attribution", () => {
    const compose = { project: "p", service: "s", containerNumber: "1" };
    const original = reconcileServers([observation({ compose })])[0];
    const duplicate = reconcileServers([
      observation({ compose }),
      observation({ compose, containerId: dockerId("docker-b"), name: "duplicate" }),
    ])[0];
    expect(duplicate.id).toBe(original.id);
    expect(duplicate.status).toBe("ambiguous");
    expect(duplicate.containerId).toBe(null);
    expect(() => resolveServerBinding(duplicate.id)).toThrow(/binding is unavailable/);
    expect(getDatabase()
        .prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT COUNT(*) AS count FROM server_bindings")
        .get()?.count).toBe(1);
    const recovered = reconcileServers([observation({ compose })])[0];
    expect(recovered.id).toBe(original.id);
    expect(recovered.status).toBe("active");
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
    expect(pending.id).toBe(original.id);
    expect(pending.status).toBe("review_required");
    expect(pending.bindingFingerprint).toBe(original.bindingFingerprint);
    expect(pending.pendingFingerprint).not.toBe(original.bindingFingerprint);
    expect(() => resolveServerBinding(pending.id)).toThrow(/review/);
    expect(() => reviewServerBinding(pending.id, "stale-fingerprint")).toThrow(/pending server binding changed/);
    const reviewed = reviewServerBinding(
      pending.id,
      pending.pendingFingerprint!,
    );
    expect(reviewed.status).toBe("active");
    expect(reviewed.bindingFingerprint).toBe(bindingFingerprint(changed));
    expect(reviewed.bindingRevision > pending.bindingRevision).toBeTruthy();
    for (const candidate of [
      { ...changed, gameType: "factorio" },
      { ...changed, gameConfiguration: { "ludock.console": "different" } },
    ]) {
      expect(bindingFingerprint(candidate)).not.toBe(reviewed.bindingFingerprint);
    }
  });

  it("keeps review suspension across disappearance and apparent reversion", () => {
    const original = reconcileServers([observation()])[0];
    reconcileServers([observation({ gameType: "factorio" })]);
    reconcileServers([]);
    const reappeared = reconcileServers([observation()])[0];
    expect(reappeared.id).toBe(original.id);
    expect(reappeared.status).toBe("review_required");
    expect(reappeared.reviewRequired).toBe(true);
  });

  it("rejects an external replacement observed between authorization and mutation", () => {
    const server = reconcileServers([observation()])[0];
    expect(() =>
      assertObservedServerBinding(
        server.id,
        observation(),
        server.bindingRevision,
      )).not.toThrow();
    expect(() =>
        assertObservedServerBinding(
          server.id,
          observation({ containerId: dockerId("external-new") }),
        )).toThrow(/identity changed/);
    expect(() =>
        assertObservedServerBinding(
          server.id,
          observation({ gameType: "factorio" }),
        )).toThrow(/identity changed/);
    expect(() =>
        assertObservedServerBinding(server.id, observation({ name: "other" }))).toThrow(/identity changed/);
  });

  it("rejects invalid or partial observations before mutating the snapshot", () => {
    const original = reconcileServers([observation()])[0];
    expect(() => reconcileServers([observation(), observation()])).toThrow(/Duplicate container/);
    expect(() =>
        reconcileServers([
          observation({
            compose: { project: "games", service: "", containerNumber: "1" },
          }),
        ])).toThrow(/Incomplete Compose/);
    expect(getLogicalServer(original.id)?.status).toBe("active");
    expect(externalServerIdentity(
        observation({
          compose: { project: "a:b", service: "c", containerNumber: "1" },
        }),
      )).not.toBe(externalServerIdentity(
        observation({
          compose: { project: "a", service: "b:c", containerNumber: "1" },
        }),
      ));
  });
});
