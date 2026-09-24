import path from "node:path";
import { composeSourceProjectSchema, type ComposeProjectSource } from "@ludock/shared";
import { AppError } from "./errors.js";

// Compose records temporary snapshot paths after recreation. Carry the original
// source on the replacement so subsequent updates survive cleanup and restarts.
// Like Docker's labels, this is untrusted input, never permission to read a path.
export const COMPOSE_SOURCE_LABEL = "ludock.compose.source";
export const COMPOSE_WORKING_DIR_LABEL = "com.docker.compose.project.working_dir";
export const COMPOSE_CONFIG_FILES_LABEL = "com.docker.compose.project.config_files";
export const COMPOSE_ENV_FILES_LABEL = "com.docker.compose.project.environment_file";

interface ComposeSource {
  project: ComposeProjectSource;
  loadDefaultEnv: boolean;
}

export function composeSourceLabels(labels: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    [COMPOSE_SOURCE_LABEL, COMPOSE_WORKING_DIR_LABEL, COMPOSE_CONFIG_FILES_LABEL, COMPOSE_ENV_FILES_LABEL]
      .filter((key) => Object.hasOwn(labels, key))
      .map((key) => [key, labels[key]]),
  );
}

export function discoverComposeSource(projectName: string, labels: Record<string, string>): ComposeSource {
  const unavailable = () => new AppError(
    "COMPOSE_SOURCE_UNAVAILABLE", 409,
    "Docker has no usable Compose source paths for this server. Recreate it through its owning manager to restore that metadata.",
  );
  let project: unknown;
  let loadDefaultEnv: boolean;
  if (Object.hasOwn(labels, COMPOSE_SOURCE_LABEL)) {
    try {
      const saved = JSON.parse(labels[COMPOSE_SOURCE_LABEL]) as Partial<ComposeSource>;
      if (!saved || typeof saved.loadDefaultEnv !== "boolean") throw unavailable();
      project = saved.project;
      loadDefaultEnv = saved.loadDefaultEnv;
    } catch {
      throw unavailable();
    }
  } else {
    const environment = labels[COMPOSE_ENV_FILES_LABEL];
    project = {
      projectName,
      projectDirectory: labels[COMPOSE_WORKING_DIR_LABEL],
      composeFiles: labels[COMPOSE_CONFIG_FILES_LABEL]?.split(","),
      envFiles: environment ? environment.split(",") : [],
    };
    loadDefaultEnv = !environment;
  }
  const parsed = composeSourceProjectSchema.safeParse(project);
  if (!parsed.success || parsed.data.projectName !== projectName ||
      ![parsed.data.projectDirectory, ...parsed.data.composeFiles, ...parsed.data.envFiles]
        .every((filename) => path.isAbsolute(filename) && !filename.includes("\0"))) {
    throw unavailable();
  }
  return { project: parsed.data, loadDefaultEnv };
}
