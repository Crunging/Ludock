import type Docker from "dockerode";
import { docker } from "./docker-client.js";
import { AppError } from "./errors.js";

/** Callers own the mounts, privileges, labels, and lifetime of each helper. */
export async function createHelperContainer(
  options: Docker.ContainerCreateOptions & { Image: string },
): Promise<Docker.Container> {
  try {
    return await docker.createContainer(options);
  } catch (error) {
    if ((error as { statusCode?: number }).statusCode !== 404) throw error;
  }
  const stream = await docker.pull(options.Image);
  await new Promise<void>((resolve, reject) => {
    docker.modem.followProgress(stream, (error) =>
      error ? reject(error) : resolve(),
    );
  });
  return docker.createContainer(options);
}

export async function removeHelperContainer(
  container: Docker.Container,
): Promise<void> {
  async function remove(): Promise<void> {
    for (let attempt = 0; ; attempt++) {
      try {
        await container.remove({ force: true });
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
