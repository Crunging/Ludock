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
  try {
    await container.remove({ force: true });
    return;
  } catch (error) {
    if ((error as { statusCode?: number }).statusCode === 404) return;
  }
  // A failed removal must not silently leave a writer running. Try stopping it
  // before removal again, and surface any remaining cleanup failure safely.
  try {
    await container.stop({ t: 0 }).catch((error: { statusCode?: number }) => {
      if (error.statusCode !== 304 && error.statusCode !== 404) throw error;
    });
    await container
      .remove({ force: true })
      .catch((error: { statusCode?: number }) => {
        if (error.statusCode !== 404) throw error;
      });
  } catch {
    throw new AppError(
      "HELPER_CLEANUP_FAILED",
      409,
      "A temporary data helper could not be removed. Check Docker and administrator diagnostics before retrying.",
    );
  }
}
