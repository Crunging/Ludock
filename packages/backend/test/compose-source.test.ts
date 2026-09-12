import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import {
  COMPOSE_CONFIG_FILES_LABEL, COMPOSE_ENV_FILES_LABEL, COMPOSE_SOURCE_LABEL,
  COMPOSE_WORKING_DIR_LABEL, composeSourceLabels, discoverComposeSource,
} from "../src/compose-source.js";

const labels = {
  [COMPOSE_WORKING_DIR_LABEL]: "/srv/games",
  [COMPOSE_CONFIG_FILES_LABEL]: "/srv/games/compose.yaml,/srv/games/override.yaml",
};

describe("automatic Compose source discovery", () => {
  it("preserves Compose merge and environment file order without registration", () => {
    const source = discoverComposeSource("games", {
      ...labels, [COMPOSE_ENV_FILES_LABEL]: "/srv/games/base.env,/srv/games/local.env",
    });
    assert.deepEqual(source, {
      project: { projectName: "games", projectDirectory: "/srv/games",
        composeFiles: ["/srv/games/compose.yaml", "/srv/games/override.yaml"],
        envFiles: ["/srv/games/base.env", "/srv/games/local.env"] },
      loadDefaultEnv: false,
    });
    assert.equal(discoverComposeSource("games", labels).loadDefaultEnv, true);
  });

  it("uses original paths after a Ludock recreation replaces Docker's source labels", () => {
    const source = discoverComposeSource("games", labels);
    assert.deepEqual(discoverComposeSource("games", {
      ...labels,
      [COMPOSE_CONFIG_FILES_LABEL]: "/tmp/removed-ludock-snapshot/resolved.json",
      [COMPOSE_ENV_FILES_LABEL]: "/tmp/removed-ludock-snapshot/empty.env",
      [COMPOSE_SOURCE_LABEL]: JSON.stringify(source),
    }), source);
  });

  it("rejects missing, malformed, relative, and mismatched source metadata", () => {
    for (const input of [
      {}, { ...labels, [COMPOSE_CONFIG_FILES_LABEL]: "" },
      { ...labels, [COMPOSE_CONFIG_FILES_LABEL]: "relative.yaml" },
      { ...labels, [COMPOSE_CONFIG_FILES_LABEL]: "/srv/games/compose.yaml," },
      { ...labels, [COMPOSE_ENV_FILES_LABEL]: "relative.env" },
      { ...labels, [COMPOSE_WORKING_DIR_LABEL]: "relative" },
      { ...labels, [COMPOSE_SOURCE_LABEL]: "invalid" },
      { ...labels, [COMPOSE_SOURCE_LABEL]: "null" },
      { ...labels, [COMPOSE_SOURCE_LABEL]: JSON.stringify({ ...discoverComposeSource("other", labels) }) },
    ]) assert.throws(() => discoverComposeSource("games", input), /no usable Compose source paths/);
  });

  it("retains only source hints and does not treat labels as file authorization", () => {
    assert.deepEqual(composeSourceLabels({ ...labels, "unrelated.secret": "private" }), labels);
    // Roots are checked by snapshot loading, never expanded from label values.
    assert.equal(discoverComposeSource("games", { ...labels,
      [COMPOSE_CONFIG_FILES_LABEL]: "/outside/compose.yaml",
    }).project.composeFiles[0], "/outside/compose.yaml");
  });
});
