import type { DockerContainerId } from "@ludock/shared";

/** Fixture container IDs; production code receives these from Docker. */
export const dockerId = (id: string) => id as DockerContainerId;
