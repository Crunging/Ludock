import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import {
  getDockerInstance,
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
          Config: { Labels: { "game-panel.enable": "true" } },
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
