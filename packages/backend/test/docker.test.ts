import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import {
  getContainer,
  getDockerInstance,
  getManagedContainer,
  restartContainer,
  startContainer,
  stopContainer,
} from "../src/docker.js";

const docker = getDockerInstance();
const originalGetContainer = docker.getContainer.bind(docker);

afterEach(() => {
  docker.getContainer = originalGetContainer;
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

      docker.getContainer = (() => container) as unknown as typeof docker.getContainer;

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

      docker.getContainer = (() => container) as unknown as typeof docker.getContainer;

      await assert.rejects(
        run("unmanaged-id"),
        (error: Error & { statusCode?: number; code?: string }) => {
          assert.equal(error.statusCode, 403);
          assert.equal(error.code, "FORBIDDEN");
          return true;
        }
      );
      assert.equal(actionCalled, false);
    });
  }
});

describe("container identifier validation", () => {
  // Express decodes %2f, so a raw identifier can carry "../" and escape
  // /containers/<id>/json. The daemon 301s to the cleaned path and docker-modem
  // re-issues it over the network with a hostname taken from the identifier.
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
      docker.getContainer = ((() => {
        reached = true;
        return { inspect: async () => ({ Config: { Labels: {} } }) };
      }) as unknown) as typeof docker.getContainer;

      for (const run of [
        () => getManagedContainer(id),
        () => startContainer(id),
        async () => getContainer(id),
      ]) {
        await assert.rejects(
          run(),
          (error: Error & { statusCode?: number; code?: string }) => {
            assert.equal(error.code, "INVALID_CONTAINER_ID");
            assert.equal(error.statusCode, 400);
            return true;
          }
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
