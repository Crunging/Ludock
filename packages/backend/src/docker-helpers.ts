import type * as Docker from "./docker-client.js";
import { docker } from "./docker-client.js";
import { AppError } from "./errors.js";

/** Callers own the mounts, privileges, labels, and lifetime of each helper. */
export async function createHelperContainer(
  options: Docker.ContainerCreateOptions & { Image: string },
): Promise<Docker.Container> {
  // Helpers reuse the runtime image but never start the panel or its healthcheck.
  const helperOptions = { ...options, Healthcheck: { Test: ["NONE"] } };
  try {
    return await docker.createContainer(helperOptions);
  } catch (error) {
    if ((error as { statusCode?: number }).statusCode !== 404 || !options.Image.includes("@sha256:")) throw error;
  }
  await docker.pull(options.Image);
  return docker.createContainer(helperOptions);
}

export async function removeHelperContainer(
  container: Docker.Container,
): Promise<void> {
  async function remove(): Promise<void> {
    for (let attempt = 0; ; attempt++) {
      try {
        // Remove image-declared anonymous volumes; Docker retains named mounts.
        await container.remove({ force: true, v: true });
        return;
      } catch (error) {
        const status = (error as { statusCode?: number }).statusCode;
        if (status === 404) return;
        // AutoRemove can race our explicit cleanup. A conflict is not proof
        // of removal: keep the caller's locks until Docker confirms it is gone.
        if (status !== 409 || attempt >= 20) throw error;
        await Bun.sleep(100);
      }
    }
  }
  try {
    await remove();
    return;
  } catch {
    // Try stopping a writer before the final removal attempt below.
  }
  // A failed removal must not silently leave a writer running. Try stopping it
  // before removal again, and surface any remaining cleanup failure safely.
  try {
    await container.stop({ t: 0 }).catch((error: { statusCode?: number }) => {
      if (error.statusCode !== 304 && error.statusCode !== 404) throw error;
    });
    await remove();
  } catch {
    throw new AppError(
      "HELPER_CLEANUP_FAILED",
      409,
      "A temporary data helper could not be removed. Check Docker and administrator diagnostics before retrying.",
    );
  }
}
