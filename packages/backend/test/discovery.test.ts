import { expect, describe, it } from "bun:test";
import {
  approvedConfigurationLabels,
  evaluateContainerEligibility,
  parseBooleanLabel,
} from "../src/discovery.js";

describe("container eligibility", () => {
  for (const value of ["true", " TRUE ", "TrUe"]) {
    it(`explicitly includes unknown one-offs with ${JSON.stringify(value)}`, () => {
      expect(evaluateContainerEligibility("custom/server", {
          "ludock.enable": value,
          "com.docker.compose.oneoff": "True",
        })).toStrictEqual({
          eligible: true,
          reason: "explicitly-enabled",
          recognizedGameType: "unknown",
        });
    });
  }

  for (const value of ["false", " FALSE ", "FaLsE"]) {
    it(`excludes recognized servers with ${JSON.stringify(value)}`, () => {
      expect(evaluateContainerEligibility("itzg/minecraft-server", {
          "ludock.enable": value,
        }).reason).toBe("opted-out");
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
      expect(result.eligible).toBe(false);
      expect(result.reason).toBe("invalid-enable-label");
      expect(JSON.stringify(result).includes("secret-token")).toBe(false);
    });
  }

  it("automatically discovers recognized mirror images", () => {
    expect(evaluateContainerEligibility(
        "private.example:5000/mirror/itzg/minecraft-server:latest",
      ).reason).toBe("recognized-image");
  });

  it("does not treat a game override as image recognition", () => {
    expect(evaluateContainerEligibility("custom/server", {
        "ludock.game": "minecraft",
      }).eligible).toBe(false);
  });

  it("excludes Compose one-offs before automatic recognition", () => {
    expect(evaluateContainerEligibility("itzg/minecraft-server", {
        "com.docker.compose.oneoff": " TRUE ",
      }).reason).toBe("compose-oneoff");
    expect(evaluateContainerEligibility("itzg/minecraft-server", {
        "com.docker.compose.oneoff": "False",
      }).eligible).toBe(true);
  });

  it("does not accept non-boolean spellings", () => {
    for (const value of [undefined, "", "1", "yes"])
      expect(parseBooleanLabel(value)).toBe(undefined);
    expect(parseBooleanLabel(" true\n")).toBe(true);
    expect(parseBooleanLabel(" FALSE ")).toBe(false);
  });

  it("allows only known configuration labels into server metadata", () => {
    expect(approvedConfigurationLabels({
        "ludock.enable": "true",
        "ludock.console": "source-rcon",
        "ludock.files": "/data",
        "ludock.password": "secret",
        "ludock.console.password": "secret",
        "ludock.token": "secret",
        "other.password": "secret",
        "com.docker.compose.project": "private-project",
      })).toStrictEqual({
        "ludock.enable": "true",
        "ludock.console": "source-rcon",
        "ludock.files": "/data",
      });
  });
});
