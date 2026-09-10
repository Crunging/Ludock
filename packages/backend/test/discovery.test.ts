import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import {
  approvedConfigurationLabels,
  evaluateContainerEligibility,
  parseBooleanLabel,
} from "../src/discovery.js";

describe("container eligibility", () => {
  for (const value of ["true", " TRUE ", "TrUe"]) {
    it(`explicitly includes unknown one-offs with ${JSON.stringify(value)}`, () => {
      assert.deepEqual(
        evaluateContainerEligibility("custom/server", {
          "ludock.enable": value,
          "com.docker.compose.oneoff": "True",
        }),
        {
          eligible: true,
          reason: "explicitly-enabled",
          recognizedGameType: "unknown",
        },
      );
    });
  }

  for (const value of ["false", " FALSE ", "FaLsE"]) {
    it(`excludes recognized servers with ${JSON.stringify(value)}`, () => {
      assert.equal(
        evaluateContainerEligibility("itzg/minecraft-server", {
          "ludock.enable": value,
        }).reason,
        "opted-out",
      );
    });
  }

  for (const value of [
    "",
    "1",
    "0",
    "yes",
    "no",
    "true false",
    "secret-token",
  ]) {
    it(`excludes invalid enable values ${JSON.stringify(value)}`, () => {
      const result = evaluateContainerEligibility("itzg/minecraft-server", {
        "ludock.enable": value,
      });
      assert.equal(result.eligible, false);
      assert.equal(result.reason, "invalid-enable-label");
      assert.equal(JSON.stringify(result).includes("secret-token"), false);
    });
  }

  it("automatically discovers recognized mirror images", () => {
    assert.equal(
      evaluateContainerEligibility(
        "private.example:5000/mirror/itzg/minecraft-server:latest",
      ).reason,
      "recognized-image",
    );
  });

  it("does not treat a game override as image recognition", () => {
    assert.equal(
      evaluateContainerEligibility("custom/server", {
        "ludock.game": "minecraft",
      }).eligible,
      false,
    );
  });

  it("excludes Compose one-offs before automatic recognition", () => {
    assert.equal(
      evaluateContainerEligibility("itzg/minecraft-server", {
        "com.docker.compose.oneoff": " TRUE ",
      }).reason,
      "compose-oneoff",
    );
    assert.equal(
      evaluateContainerEligibility("itzg/minecraft-server", {
        "com.docker.compose.oneoff": "False",
      }).eligible,
      true,
    );
  });

  it("does not accept non-boolean spellings", () => {
    for (const value of [undefined, "", "1", "yes"])
      assert.equal(parseBooleanLabel(value), undefined);
    assert.equal(parseBooleanLabel(" true\n"), true);
    assert.equal(parseBooleanLabel(" FALSE "), false);
  });

  it("allows only known configuration labels into server metadata", () => {
    assert.deepEqual(
      approvedConfigurationLabels({
        "ludock.enable": "true",
        "ludock.console": "source-rcon",
        "ludock.files": "/data",
        "ludock.password": "secret",
        "ludock.console.password": "secret",
        "ludock.token": "secret",
        "other.password": "secret",
        "com.docker.compose.project": "private-project",
      }),
      {
        "ludock.enable": "true",
        "ludock.console": "source-rcon",
        "ludock.files": "/data",
      },
    );
  });
});
