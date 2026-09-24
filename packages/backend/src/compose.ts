import { decodeText, concatBytes } from "./bytes.js";
import path from "node:path";
import os from "node:os";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { parse } from "yaml";
import {
  composeSourceProjectSchema,
  type ComposeProjectSource,
  type UpdateCapability,
} from "@ludock/shared";
import {
  getDatabase,
  keyedComposeSourceFingerprint,
} from "./database.js";
import {
  approvedPath,
  configuredRoots,
  readApprovedFile,
  readOptionalApprovedFile,
} from "./approved-paths.js";
import { AppError } from "./errors.js";
import type { ServerContext } from "./servers.js";
import { docker } from "./docker-client.js";
import { isServerBusy } from "./operation-locks.js";
import { COMPOSE_SOURCE_LABEL, discoverComposeSource } from "./compose-source.js";

type Model = Record<string, unknown>;
export interface ComposeSnapshot {
  directory: string;
  configPath: string;
  model: Model;
  fingerprint: string;
  project: ComposeProjectSource;
  cleanup: () => Promise<void>;
}
const serviceNamePattern = /^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/;
export function composeEnvironment(): NodeJS.ProcessEnv {
  return {
    PATH: process.env.PATH ?? "/usr/local/bin:/usr/bin:/bin",
    HOME: "/nonexistent",
    LANG: "C.UTF-8",
    DOCKER_HOST: `unix://${process.env.DOCKER_SOCKET || "/var/run/docker.sock"}`,
    DOCKER_CONFIG: process.env.LUDOCK_DOCKER_CONFIG || "/nonexistent",
    COMPOSE_DISABLE_ENV_FILE: "true",
    COMPOSE_ANSI: "never",
    COMPOSE_PROGRESS: "plain",
  };
}
export async function runCompose(
  args: string[],
  timeoutMs = 120_000,
): Promise<string> {
  let child: Bun.Subprocess<"ignore", "pipe", "pipe">;
  try {
    child = Bun.spawn(["docker", "compose", ...args], {
      env: composeEnvironment(),
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
      detached: process.platform !== "win32",
    });
  } catch {
    throw new AppError(
      "COMPOSE_UNAVAILABLE",
      409,
      "Docker Compose could not be executed",
    );
  }
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  let limited = false;
  let kill: ReturnType<typeof setTimeout> | undefined;
  const signal = (value: "SIGTERM" | "SIGKILL") => {
    try {
      // Docker launches the Compose plugin as a child. Keep both in the same
      // group so cancellation cannot leave a plugin running after lock release.
      if (process.platform !== "win32") process.kill(-child.pid, value);
      else child.kill(value);
    } catch {
      // The process or group may have exited between the deadline and signal.
      try {
        child.kill(value);
      } catch {
        // Already exited.
      }
    }
  };
  const timer = setTimeout(() => {
    limited = true;
    signal("SIGTERM");
    kill = setTimeout(() => signal("SIGKILL"), 5000);
  }, timeoutMs);
  const drain = async (stream: ReadableStream<Uint8Array>, capture: boolean) => {
    const reader = stream.getReader();
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        // Raw diagnostics and resolved configuration can contain arbitrary
        // secrets. Drain stderr without retaining or exposing its contents.
        if (!capture) continue;
        bytes += value.byteLength;
        if (bytes > 8 * 1024 * 1024) {
          limited = true;
          signal("SIGKILL");
        } else chunks.push(value);
      }
    } catch (error) {
      signal("SIGKILL");
      throw error;
    } finally {
      reader.releaseLock();
    }
  };
  // Keep the deadline active until inherited output pipes close as well as the
  // CLI exiting: an orphaned plugin may still hold them and modify the project.
  const [exit, stdout, stderr] = await Promise.allSettled([
    child.exited,
    drain(child.stdout, true),
    drain(child.stderr, false),
  ]).finally(() => {
    clearTimeout(timer);
    if (kill) clearTimeout(kill);
  });
  if (
    limited ||
    exit.status !== "fulfilled" ||
    exit.value !== 0 ||
    stdout.status === "rejected" ||
    stderr.status === "rejected"
  ) {
    throw new AppError(
      "COMPOSE_FAILED",
      409,
      limited
        ? "Docker Compose exceeded its execution limit"
        : "Docker Compose failed. Review the project through its owning manager.",
    );
  }
  return decodeText(concatBytes(chunks));
}
export async function isComposeAvailable(): Promise<boolean> {
  try {
    if (
      process.platform !== "linux" ||
      configuredRoots(process.env.LUDOCK_COMPOSE_ROOTS).length === 0
    ) return false;
    await runCompose(["version", "--short"], 5000);
    return (
      process.platform === "linux" &&
      configuredRoots(process.env.LUDOCK_COMPOSE_ROOTS).length > 0
    );
  } catch {
    return false;
  }
}
function object(
  value: unknown,
  message = "Unsupported Compose structure",
): Model {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new AppError("UNSUPPORTED_COMPOSE", 400, message);
  return value as Model;
}
function no(value: Model, keys: string[]) {
  for (const key of keys)
    if (key in value)
      throw new AppError(
        "UNSUPPORTED_COMPOSE",
        400,
        `Compose '${key}' is outside the supported update configuration`,
      );
}
function inputPath(value: unknown, base: string): string {
  if (
    typeof value !== "string" ||
    !value ||
    value.includes("$") ||
    value.includes("://")
  )
    throw new AppError(
      "UNSUPPORTED_COMPOSE",
      400,
      "Compose file references must use literal local paths",
    );
  return path.isAbsolute(value) ? value : path.resolve(base, value);
}
/** Materialize only vetted inputs. Compose reads a private snapshot, never
 * reparses mutable sources between config/pull/up. Source files are
 * never edited; relative service volume paths retain the original base. */
export async function createComposeSnapshot(
  input: ComposeProjectSource,
  loadDefaultEnv = false,
): Promise<ComposeSnapshot> {
  const project = composeSourceProjectSchema.parse(input);
  // Fail before reading source files or invoking Compose if the installation
  // key needed to protect their digest is unavailable.
  getDatabase();
  const roots = configuredRoots(process.env.LUDOCK_COMPOSE_ROOTS);
  const base = approvedPath(project.projectDirectory, roots);
  const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-compose-"));
  const cleanup = () => rm(directory, { recursive: true, force: true });
  const hash = new Bun.CryptoHasher("sha256").update(JSON.stringify({ project, loadDefaultEnv }));
  let inputCount = 0;
  const snapshotFile = async (original: string) => {
    const data = await readApprovedFile(original, roots);
    hash.update(original).update("\0").update(data);
    const filename = path.join(directory, `input-${inputCount++}`);
    await writeFile(filename, data, { mode: 0o600 });
    return filename;
  };
  try {
    const emptyEnv = path.join(directory, "empty.env");
    await writeFile(emptyEnv, "", { mode: 0o600 });
    const flags = [
      "--project-name",
      project.projectName,
      "--project-directory",
      base,
    ];
    for (const filename of project.envFiles)
      flags.push("--env-file", await snapshotFile(inputPath(filename, base)));
    if (!project.envFiles.length) {
      const filename = path.join(base, ".env");
      const data = loadDefaultEnv ? await readOptionalApprovedFile(filename, roots) : undefined;
      if (data !== undefined) {
        hash.update(filename).update("\0").update(data);
        const copy = path.join(directory, "default.env");
        await writeFile(copy, data, { mode: 0o600 });
        flags.push("--env-file", copy);
      } else flags.push("--env-file", emptyEnv);
    }
    for (const original of project.composeFiles) {
      const filename = inputPath(original, base);
      const bytes = await readApprovedFile(filename, roots);
      hash.update(filename).update("\0").update(bytes);
      let model: Model;
      try {
        model = object(
          parse(decodeText(bytes), {
            maxAliasCount: 20,
            uniqueKeys: true,
          }),
        );
      } catch (error) {
        if (error instanceof AppError) throw error;
        throw new AppError(
          "INVALID_COMPOSE",
          400,
          "Compose YAML could not be parsed safely",
        );
      }
      no(model, ["include", "secrets", "configs", "models"]);
      const services = object(model.services, "Compose must declare services");
      for (const [name, value] of Object.entries(services)) {
        if (!serviceNamePattern.test(name))
          throw new AppError(
            "INVALID_SERVICE",
            400,
            "Invalid Compose service name",
          );
        const service = object(value);
        no(service, [
          "extends",
          "label_file",
          "secrets",
          "configs",
          "credential_spec",
          "pre_start",
          "post_start",
          "pre_stop",
          "provider",
          "develop",
          "volumes_from",
        ]);
        // Builds can read arbitrary contexts. Image+build services remain
        // usable by snapshotting only the pullable image definition.
        if (service.build !== undefined) {
          if (!service.image)
            throw new AppError(
              "BUILD_ONLY",
              400,
              "Build-only services cannot be updated by Ludock",
            );
          delete service.build;
        }
        if (service.env_file !== undefined) {
          const files = Array.isArray(service.env_file)
            ? service.env_file
            : [service.env_file];
          const snapshots = [];
          // Fingerprints follow the declared source order, regardless of how
          // quickly the filesystem can read each environment file.
          for (const entry of files) {
            const record =
              typeof entry === "string" ? { path: entry } : object(entry);
            if (
              Object.keys(record).some(
                (key) => !["path", "required", "format"].includes(key),
              )
            )
              throw new AppError(
                "UNSUPPORTED_COMPOSE",
                400,
                "Unsupported environment-file option",
              );
            snapshots.push({
              ...record,
              path: await snapshotFile(inputPath(record.path, base)),
            });
          }
          service.env_file = snapshots;
        }
      }
      const copy = path.join(directory, `compose-${inputCount++}.json`);
      await writeFile(copy, JSON.stringify(model), { mode: 0o600 });
      flags.push("-f", copy);
    }
    const output = await runCompose([...flags, "config", "--format", "json"]);
    let model: Model;
    try {
      model = object(JSON.parse(output));
    } catch {
      throw new AppError(
        "INVALID_COMPOSE",
        400,
        "Docker Compose returned an invalid configuration",
      );
    }
    const configPath = path.join(directory, "resolved.json"); // Compose serializes literal dollars as $$ in its reusable config output.
    // Preserve that representation; a second escaping pass changes user values.
    // Keep original paths on replacements: Compose's own labels will point at
    // this disposable snapshot. Do not expose environment contents in labels.
    const source = JSON.stringify({ project: {
      ...project,
      composeFiles: project.composeFiles.map((file) => inputPath(file, base)),
      envFiles: project.envFiles.map((file) => inputPath(file, base)),
    }, loadDefaultEnv }).replaceAll("$", "$$");
    for (const value of Object.values(object(model.services))) {
      const service = object(value);
      service.labels = { ...(service.labels ? object(service.labels) : {}), [COMPOSE_SOURCE_LABEL]: source };
    }
    await writeFile(configPath, JSON.stringify(model), { mode: 0o600 });
    return {
      directory,
      configPath,
      model,
      fingerprint: keyedComposeSourceFingerprint(hash.digest("hex")),
      project,
      cleanup,
    };
  } catch (error) {
    await cleanup();
    throw error;
  }
}
export async function validatedProject(
  context: ServerContext,
): Promise<{ snapshot: ComposeSnapshot; service: string; image: string }> {
  const compose = context.observation.compose;
  if (!compose)
    throw new AppError(
      "EXTERNAL_MANAGER",
      409,
      "Update this server through its original container manager",
    );
  const roots = configuredRoots(process.env.LUDOCK_COMPOSE_ROOTS);
  if (!roots.length) throw new AppError(
    "COMPOSE_SOURCES_NOT_MOUNTED", 409,
    "Mount your Compose folder read-only into Ludock at the same absolute host path and set LUDOCK_COMPOSE_ROOTS to that folder. Then check again.",
  );
  const source = discoverComposeSource(compose.project, context.observation.composeSourceLabels ?? {});
  let snapshot: ComposeSnapshot;
  try {
    snapshot = await createComposeSnapshot(source.project, source.loadDefaultEnv);
  } catch (error) {
    if (error instanceof AppError && ["UNAPPROVED_PATH", "UNSAFE_CONFIG_PATH"].includes(error.code)) {
      throw new AppError(error.code, 409,
        "Ludock cannot read this server’s Compose files. Mount the source folder read-only at its original absolute host path, include it in LUDOCK_COMPOSE_ROOTS, and check that all referenced files exist without symlinks.");
    }
    throw error;
  }
  try {
    const services = object(snapshot.model.services);
    const service = object(
      services[compose.service],
      "The selected service is absent from the Compose project",
    );
    validateUpdateService(service);
    await assertSingleServiceContainer(context);
    const image = service.image;
    if (typeof image !== "string" || !image || image.includes("@"))
      throw new AppError(
        "UNSUPPORTED_IMAGE",
        409,
        "Updates require a pullable image tag, not a digest-pinned or build-only service",
      );
    if (service.platform !== undefined && typeof service.platform !== "string")
      throw new AppError("UNSUPPORTED_IMAGE", 409, "Invalid service platform");
    return { snapshot, service: compose.service, image };
  } catch (error) {
    await snapshot.cleanup();
    throw error;
  }
}
export function composeSnapshotArgs(snapshot: ComposeSnapshot): string[] {
  return [
    "--project-name",
    snapshot.project.projectName,
    "--project-directory",
    snapshot.project.projectDirectory,
    "--env-file",
    path.join(snapshot.directory, "empty.env"),
    "-f",
    snapshot.configPath,
  ];
}
export async function updateCapability(
  context: ServerContext,
): Promise<UpdateCapability> {
  const common: Pick<UpdateCapability, "actionLabel" | "manager"> = {
    actionLabel:
      context.container.gameType === "unknown"
        ? "Update image"
        : "Update server",
    manager: context.observation.compose ? "compose" : "external",
  };
  try {
    if (
      isServerBusy(context.logical.id) ||
      getDatabase()
        .query(
          "SELECT id FROM operations WHERE server_id=? AND status IN ('queued','running')",
        )
        .get(context.logical.id)
    )
      throw new AppError(
        "OPERATION_CONFLICT",
        409,
        "Wait for the active operation to finish before updating",
      );
    const { snapshot, service, image } = await validatedProject(context);
    try {
      return {
        ...common,
        available: true,
        projectName: snapshot.project.projectName,
        serviceName: service,
        image,
      };
    } finally {
      await snapshot.cleanup();
    }
  } catch (error) {
    return {
      ...common,
      available: false,
      unavailableReason:
        error instanceof AppError
          ? error.message
          : "The Compose source is inaccessible or invalid. Check its read-only mount and LUDOCK_COMPOSE_ROOTS in Settings.",
    };
  }
}

export function validateUpdateService(service: Model): void {
  const deploy = service.deploy === undefined ? {} : object(service.deploy);
  if (Number(service.scale ?? deploy.replicas ?? 1) !== 1)
    throw new AppError(
      "COMPOSE_REPLICAS",
      409,
      "The selected service must have exactly one configured replica",
    );
  for (const key of ["network_mode", "pid", "ipc"])
    if (typeof service[key] === "string" && service[key].startsWith("service:"))
      throw new AppError(
        "SERVICE_DEPENDENCY",
        409,
        "Service namespace dependencies must be updated through their original manager",
      );
}
export async function assertSingleServiceContainer(
  server: ServerContext,
): Promise<void> {
  const compose = server.observation.compose;
  if (!compose)
    throw new AppError(
      "EXTERNAL_MANAGER",
      409,
      "Update this server through its original container manager",
    );
  const containers = await docker.listContainers({
    all: true,
    filters: {
      label: [
        `com.docker.compose.project=${compose.project}`,
        `com.docker.compose.service=${compose.service}`,
      ],
    },
  });
  const normal = containers.filter(
    (container) =>
      container.Labels?.["com.docker.compose.oneoff"]?.toLowerCase() !== "true",
  );
  if (normal.length !== 1 || normal[0].Id !== server.container.id)
    throw new AppError(
      "COMPOSE_REPLICAS",
      409,
      "Updates require exactly one container for the selected Compose service",
    );
}
