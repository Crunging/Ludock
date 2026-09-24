import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { expect, afterAll as after, beforeEach, describe, it } from "bun:test";
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
import type { SQLQueryBindings } from "bun:sqlite";
import { dockerId } from "./fixtures/ids.js";

const identityDirectory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-identity-"));
process.env.LUDOCK_DB_PATH = ":memory:";
beforeEach(() => {
  closeDatabase();
  process.env.LUDOCK_DB_PATH = ":memory:";
});
after(() => {
  closeDatabase();
  fs.rmSync(identityDirectory, { recursive: true, force: true });
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
  it("keeps fingerprints stable per installation while detecting changed secrets", () => {
    const firstPath = path.join(identityDirectory, "first.db");
    process.env.LUDOCK_DB_PATH = firstPath;
    const original = bindingFingerprint(observation({ gameConfiguration: { password: "first-secret" } }));
    expect(original).toMatch(/^hmac-sha256:[a-f0-9]{64}$/);
    expect(original).toBe(bindingFingerprint(observation({ gameConfiguration: { password: "first-secret" } })));
    expect(original).not.toBe(bindingFingerprint(observation({ gameConfiguration: { password: "second-secret" } })));
    closeDatabase();
    expect(original).toBe(bindingFingerprint(observation({ gameConfiguration: { password: "first-secret" } })));

    closeDatabase();
    process.env.LUDOCK_DB_PATH = path.join(identityDirectory, "second.db");
    expect(original).not.toBe(bindingFingerprint(observation({ gameConfiguration: { password: "first-secret" } })));
  });

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
        containerId: dockerId("replacement"),
        name: "renamed-by-compose",
      }),
    ])[0];
    expect(recreated.id).toBe(original.id);
    expect(recreated.externalIdentity).toBe("compose:games:minecraft:1");
    expect(recreated.status).toBe("active");
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

  it("scopes the same identity to separate persisted Docker hosts", () => {
    const localHost = getDockerHostId();
    expect(getDockerHostId()).toBe(localHost);
    const otherHost = getDockerHostId("another-daemon");
    const first = reconcileServers([observation()], { hostId: localHost })[0];
    const second = reconcileServers([observation()], { hostId: otherHost })[0];
    expect(first.id).not.toBe(second.id);
    expect(listLogicalServers().length).toBe(2);
    expect(getLogicalServer(first.id)?.status).toBe("active");
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
    expect(bindingFingerprint(first)).toBe(bindingFingerprint(second));
    reconcileServers([first]);
    const rows = getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT * FROM logical_servers").all();
    expect(JSON.stringify(rows).includes("never-store-me")).toBe(false);
    expect(JSON.stringify(rows).includes("/srv/minecraft")).toBe(false);
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
