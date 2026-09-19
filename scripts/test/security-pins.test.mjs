import { readFile, readdir } from "node:fs/promises";
import { expect, describe, it } from "bun:test";
import { FALLBACK_HELPER_IMAGE } from "../../packages/backend/src/runtime-images.ts";

const root = new URL("../../", import.meta.url);
const read = (name) => readFile(new URL(name, root), "utf8");

describe("security dependency pins", () => {
  it("keeps the hardened example deployment self-contained and directly editable", async () => {
    const source = await read("compose.yaml");
    expect(source).not.toContain("${");
    const service = Bun.YAML.parse(source).services.ludock;
    expect(service.env_file).toEqual([{ path: ".env", required: false }]);
    expect(service.image).toBe("ghcr.io/crunging/ludock:latest");
    expect(service.ports).toEqual(["3000:3000"]);
    // Defaults live in the image so optional .env values can override them.
    expect(service.environment).toBeUndefined();
    expect(await read("Dockerfile")).toContain("ENV LUDOCK_BACKUP_ROOTS=/backups");
    expect(service.volumes[0]).toMatchObject({
      source: "/var/run/docker.sock", target: "/var/run/docker.sock",
      read_only: true, bind: { create_host_path: false },
    });
    expect(service.read_only).toBe(true);
    expect(service.cap_drop).toEqual(["ALL"]);
    expect(service.cap_add).toEqual(["DAC_OVERRIDE"]);
    expect(service.security_opt).toEqual(["no-new-privileges:true"]);
    expect(service.tmpfs).toEqual(["/tmp:rw,nosuid,nodev,noexec,size=256m,mode=1777"]);
  });

  it("pins build and fixture images to immutable manifests", async () => {
    const dockerfile = await read("Dockerfile");
    const bun = dockerfile.match(/^ARG BUN_IMAGE=(.+)$/m)?.[1];
    const alpine = dockerfile.match(/^ARG ALPINE_IMAGE=(.+)$/m)?.[1];
    expect(bun).toMatch(/^[^\s@]+@sha256:[a-f0-9]{64}$/);
    expect(FALLBACK_HELPER_IMAGE).toMatch(/^ghcr\.io\/crunging\/ludock@sha256:[a-f0-9]{64}$/);
    expect(alpine).toMatch(/^alpine:3@sha256:[a-f0-9]{64}$/);
    const fixtures = await read("scripts/test-compose.mjs");
    expect(await read("packages/backend/test/docker-client.integration.ts")).toContain(alpine);
    expect(fixtures.match(/^const expectedFixtureImage = "(.+)";$/m)?.[1]).toBe(alpine);
    const fixtureImages = [...fixtures.matchAll(/^\s+image: (.+)$/gm)].map((match) => match[1]);
    // Compose update acceptance needs a tag, whose contents the harness checks
    // against the shared immutable pin before it creates fixture services.
    expect(fixtureImages).toStrictEqual(["alpine:3", "alpine:3"]);
  });

  it("requires full immutable references for external workflow actions", async () => {
    for (const file of await readdir(new URL(".github/workflows/", root))) {
      if (!/\.ya?ml$/.test(file)) continue;
      for (const [, reference] of (await read(`.github/workflows/${file}`)).matchAll(/^\s*(?:- )?uses:\s+(\S+)/gm)) {
        if (reference.startsWith("./")) continue;
        expect(reference, `${file}: ${reference}`).toMatch(reference.startsWith("docker://")
          ? /^docker:\/\/[^\s@]+@sha256:[a-f0-9]{64}$/
          : /^[^\s@]+@[a-f0-9]{40}$/);
      }
    }
  });
});
