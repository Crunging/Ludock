import { expect, describe, it } from "bun:test";
import {
  COMPOSE_CONFIG_FILES_LABEL, COMPOSE_ENV_FILES_LABEL, COMPOSE_SOURCE_LABEL,
  COMPOSE_WORKING_DIR_LABEL, discoverComposeSource,
} from "../src/compose-source.js";

const labels = {
  [COMPOSE_WORKING_DIR_LABEL]: "/srv/games",
  [COMPOSE_CONFIG_FILES_LABEL]: "/srv/games/compose.yaml,/srv/games/override.yaml",
};

describe("automatic Compose source discovery", () => {
  it("uses original paths after a Ludock recreation replaces Docker's source labels", () => {
    const source = discoverComposeSource("games", labels);
    expect(discoverComposeSource("games", {
      ...labels,
      [COMPOSE_CONFIG_FILES_LABEL]: "/tmp/removed-ludock-snapshot/resolved.json",
      [COMPOSE_ENV_FILES_LABEL]: "/tmp/removed-ludock-snapshot/empty.env",
      [COMPOSE_SOURCE_LABEL]: JSON.stringify(source),
    })).toStrictEqual(source);
  });
});
