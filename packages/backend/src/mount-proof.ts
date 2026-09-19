import { demuxDockerStream, dockerStdout } from "./docker-stream.js";
import path from "node:path";
import type * as Docker from "./docker-client.js";
import { docker } from "./docker-client.js";
import type { ContainerFileMount } from "./file-storage.js";
import { resolveHelperImage } from "./runtime-images.js";
import { AppError } from "./errors.js";
import { createHelperContainer, removeHelperContainer } from "./docker-helpers.js";

// Bun embeds these checked programs without executing them in the backend.
// @ts-expect-error TypeScript models JS exports, not Bun's text import attribute.
import mountIdentitiesSource from "./helpers/mount-identities.js" with { type: "text" };
// @ts-expect-error TypeScript models JS exports, not Bun's text import attribute.
import mountProofSource from "./helpers/mount-proof.js" with { type: "text" };
const MOUNT_IDENTITIES_SCRIPT = mountIdentitiesSource as string;
export const MOUNT_PROOF_SCRIPT = mountProofSource as string;

export interface MountIdentity {
  dev: string;
  ino: string;
}
export interface MountProof {
  identities: Record<string, MountIdentity>;
  cleanup(): Promise<void>;
}

function proofError(): AppError {
  return new AppError(
    "UNVERIFIED_DATA_MOUNT", 409,
    "A data mount could not be verified. Bind source paths must contain no symbolic links and must be accessible to the Docker host validator; named volumes must use the local driver without host remapping options.",
  );
}

/** A private validator exposes only directory device/inode metadata. It never
 * runs user commands, reads game file contents, or exposes its host mount to a
 * file endpoint. Retaining its FDs prevents source inode reuse until cleanup. */
export async function createMountProof(
  mounts: readonly ContainerFileMount[],
  operationId?: string,
): Promise<MountProof> {
  for (const mount of mounts.filter((entry) => entry.Type === "volume")) {
    const name = mount.Name || mount.Source;
    if (!name || name.includes("/")) throw proofError();
    let info: Docker.VolumeInspectInfo;
    try {
      info = await docker.getVolume(name).inspect();
    } catch {
      throw proofError();
    }
    if (info.Driver !== "local" || Object.keys(info.Options || {}).length > 0)
      throw proofError();
  }
  const sources = mounts
    .filter((entry) => entry.Type === "bind")
    .map((mount) => ({
      source: path.posix.normalize(mount.Source).replace(/\/$/, ""),
      destination: path.posix.normalize(mount.Destination).replace(/\/$/, ""),
    }));
  if (!sources.length) return { identities: {}, cleanup: async () => {} };
  const image = await resolveHelperImage();
  const options: Docker.ContainerCreateOptions & { Image: string } = {
    Image: image,
    Entrypoint: ["bun", "-e"],
    Cmd: [
      MOUNT_PROOF_SCRIPT,
      JSON.stringify(sources),
      String(operationId ? 0 : 35 * 60_000),
    ],
    User: "0",
    Labels: {
      "ludock.enable": "false",
      "ludock.internal": "mount-validator",
      ...(operationId ? { "ludock.operation": operationId } : {}),
    },
    HostConfig: {
      Mounts: [
        {
          Type: "bind",
          Source: "/",
          Target: "/host",
          ReadOnly: true,
          BindOptions: {
            ReadOnlyForceRecursive: true,
          },
        },
      ],
      NetworkMode: "none",
      ReadonlyRootfs: true,
      AutoRemove: true,
      CapDrop: ["ALL"],
      CapAdd: ["DAC_READ_SEARCH"],
      SecurityOpt: ["no-new-privileges"],
      PidsLimit: 32,
      Memory: 128 * 1024 * 1024,
      NanoCpus: 1_000_000_000,
    },
  };
  let container: Docker.Container;
  try {
    container = await createHelperContainer(options);
  } catch {
    throw proofError();
  }
  let removal: Promise<void> | undefined;
  const cleanup = () => {
    removal ??= removeHelperContainer(container);
    return removal;
  };
  try {
    await container.start();
    const stream = await container.logs({
      follow: true,
      stdout: true,
      stderr: false,
      tail: 10,
    });
    const reader = dockerStdout(stream).getReader();
    const timeout = setTimeout(() => { void reader.cancel().catch(() => {}); }, 10_000);
    let identities: Record<string, MountIdentity>;
    try {
      let buffer = "";
      const decoder = new TextDecoder();
      while (true) {
        const { value, done } = await reader.read();
        if (done) throw proofError();
        buffer += decoder.decode(value, { stream: true });
        if (buffer.length > 16_384) throw proofError();
        if (!buffer.includes("\n")) continue;
        const result = (JSON.parse(buffer.slice(0, buffer.indexOf("\n"))) as { identities?: Record<string, MountIdentity> }).identities;
        if (!result || sources.some(source => !result[source.destination] ||
            !/^\d+$/.test(result[source.destination].dev) || !/^\d+$/.test(result[source.destination].ino)))
          throw proofError();
        identities = result;
        break;
      }
    } finally {
      clearTimeout(timeout);
      await reader.cancel().catch(() => {});
      reader.releaseLock();
    }
    return { identities, cleanup };
  } catch {
    await cleanup();
    throw proofError();
  }
}

/** For backup helpers that project roots under aliases. The caller must keep
 * the proof alive until the data helper is removed. */
export async function assertMountIdentities(
  container: Docker.Container,
  identities: Record<string, MountIdentity>,
): Promise<void> {
  if (!Object.keys(identities).length) return;
  const execution = await container.exec({
    Cmd: ["bun", "-e", MOUNT_IDENTITIES_SCRIPT, JSON.stringify(identities)],
    AttachStdout: true,
    AttachStderr: true,
  });
  const stream = await execution.start();
  const timeout = setTimeout(() => stream.abort(proofError()), 10_000);
  try { await demuxDockerStream(stream.readable); }
  finally { clearTimeout(timeout); stream.abort(); }
  if ((await execution.inspect()).ExitCode !== 0) throw proofError();
}
