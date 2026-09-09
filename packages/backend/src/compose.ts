import { spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import path from "node:path";
import os from "node:os";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { parse } from "yaml";
import {
  composeRegistrationSchema,
  type ComposeProjectRegistration,
  type UpdateCapability,
} from "@ludock/shared";
import { getDatabase } from "./database.js";
import {
  approvedPath,
  configuredRoots,
  readApprovedFile,
} from "./approved-paths.js";
import { AppError } from "./errors.js";
import type { ServerContext } from "./servers.js";
import { getDockerInstance } from "./docker.js";
import { isServerBusy } from "./operation-locks.js";

type Model = Record<string, unknown>;
export interface ComposeProject extends ComposeProjectRegistration {
  id: string;
  disabled: boolean;
  sourceFingerprint: string;
}
interface ProjectRow {
  id: string;
  registration_json: string;
  source_fingerprint: string;
  disabled: number;
}
export interface ComposeSnapshot {
  directory: string;
  configPath: string;
  model: Model;
  fingerprint: string;
  project: ComposeProjectRegistration;
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
  return new Promise((resolve, reject) => {
    const child = spawn("docker", ["compose", ...args], {
      env: composeEnvironment(),
      stdio: ["ignore", "pipe", "pipe"],
    });
    const chunks: Buffer[] = [];
    let bytes = 0;
    let timedOut = false;
    let kill: ReturnType<typeof setTimeout> | undefined;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill("SIGTERM");
      kill = setTimeout(() => child.kill("SIGKILL"), 5000);
    }, timeoutMs);
    child.stdout.on("data", (chunk: Buffer) => {
      bytes += chunk.length;
      if (bytes > 8 * 1024 * 1024) {
        timedOut = true;
        child.kill("SIGKILL");
      } else chunks.push(chunk);
    });
    // Raw diagnostics and resolved configuration can contain arbitrary secrets.
    child.stderr.resume();
    child.once("error", () => {
      clearTimeout(timer);
      if (kill) clearTimeout(kill);
      reject(
        new AppError(
          "COMPOSE_UNAVAILABLE",
          409,
          "Docker Compose could not be executed",
        ),
      );
    });
    child.once("close", (code) => {
      clearTimeout(timer);
      if (kill) clearTimeout(kill);
      if (code !== 0 || timedOut)
        reject(
          new AppError(
            "COMPOSE_FAILED",
            409,
            timedOut
              ? "Docker Compose exceeded its execution limit"
              : "Docker Compose failed. Review the project through its owning manager.",
          ),
        );
      else resolve(Buffer.concat(chunks).toString("utf8"));
    });
  });
}
export async function isComposeAvailable(): Promise<boolean> {
  try {
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
 * reparses mutable registered sources between config/pull/up. Source files are
 * never edited; relative service volume paths retain the registered base. */
export async function createComposeSnapshot(
  input: ComposeProjectRegistration,
): Promise<ComposeSnapshot> {
  const registration = composeRegistrationSchema.parse(input);
  const roots = configuredRoots(process.env.LUDOCK_COMPOSE_ROOTS);
  const base = approvedPath(registration.projectDirectory, roots);
  const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-compose-"));
  const cleanup = () => rm(directory, { recursive: true, force: true });
  const hash = createHash("sha256").update(JSON.stringify(registration));
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
      registration.projectName,
      "--project-directory",
      base,
    ];
    for (const filename of registration.envFiles)
      flags.push("--env-file", await snapshotFile(inputPath(filename, base)));
    // Explicit empty env-file also avoids Compose's implicit .env search when
    // the CLI version ignores COMPOSE_DISABLE_ENV_FILE.
    if (!registration.envFiles.length) flags.push("--env-file", emptyEnv);
    for (const original of registration.composeFiles) {
      const filename = inputPath(original, base);
      const bytes = await readApprovedFile(filename, roots);
      hash.update(filename).update("\0").update(bytes);
      let model: Model;
      try {
        model = object(
          parse(bytes.toString("utf8"), {
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
          service.env_file = await Promise.all(
            files.map(async (entry) => {
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
              return {
                ...record,
                path: await snapshotFile(inputPath(record.path, base)),
              };
            }),
          );
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
    await writeFile(configPath, output, { mode: 0o600 });
    return {
      directory,
      configPath,
      model,
      fingerprint: hash.digest("hex"),
      project: registration,
      cleanup,
    };
  } catch (error) {
    await cleanup();
    throw error;
  }
}
export function listComposeProjects(): ComposeProject[] {
  return (
    getDatabase()
      .prepare("SELECT * FROM compose_projects ORDER BY project_name")
      .all() as unknown as ProjectRow[]
  ).map((row) => ({
    ...composeRegistrationSchema.parse(JSON.parse(row.registration_json)),
    id: row.id,
    sourceFingerprint: row.source_fingerprint,
    disabled: Boolean(row.disabled),
  }));
}
export async function registerComposeProject(
  input: unknown,
): Promise<ComposeProject> {
  const registration = composeRegistrationSchema.parse(input);
  const snapshot = await createComposeSnapshot(registration);
  try {
    const existing = listComposeProjects().find(
      (project) => project.projectName === registration.projectName,
    );
    const id = existing?.id ?? randomUUID();
    getDatabase()
      .prepare(
        `INSERT INTO compose_projects(id,project_name,registration_json,source_fingerprint,created_at) VALUES(?,?,?,?,?) ON CONFLICT(project_name) DO UPDATE SET registration_json=excluded.registration_json,source_fingerprint=excluded.source_fingerprint,disabled=0`,
      )
      .run(
        id,
        registration.projectName,
        JSON.stringify(registration),
        snapshot.fingerprint,
        Date.now(),
      );
    return listComposeProjects().find((project) => project.id === id)!;
  } finally {
    await snapshot.cleanup();
  }
}
export function deleteComposeProject(id: string): void {
  getDatabase().prepare("DELETE FROM compose_projects WHERE id=?").run(id);
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
  const registration = listComposeProjects().find(
    (project) => project.projectName === compose.project && !project.disabled,
  );
  if (!registration)
    throw new AppError(
      "UNREGISTERED_PROJECT",
      409,
      "An administrator must register this Compose project before updating",
    );
  const { projectName, projectDirectory, composeFiles, envFiles } =
    registration;
  const snapshot = await createComposeSnapshot({
    projectName,
    projectDirectory,
    composeFiles,
    envFiles,
  });
  try {
    if (snapshot.fingerprint !== registration.sourceFingerprint)
      throw new AppError(
        "COMPOSE_SOURCE_CHANGED",
        409,
        "Compose source changed. Validate and register the project again before updating.",
      );
    const services = object(snapshot.model.services);
    const service = object(
      services[compose.service],
      "The selected service is absent from the registered project",
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
        .prepare(
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
          : "The registered Compose configuration could not be validated",
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
  const containers = await getDockerInstance().listContainers({
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
