import type { DockerContainerId, LogicalServerId } from "@ludock/shared";
import type {
  changeContainerState,
  getContainer,
  ManagedContainer,
} from "../../src/docker.js";
import type { LogicalServer } from "../../src/identity.js";
import type { ServerContext } from "../../src/servers.js";

type AssertTrue<Value extends true> = Value;
type AssertFalse<Value extends false> = Value;

// These compile-only checks fail if a future refactor erases the domain brands
// or permits a public logical UUID to be passed directly to Docker wrappers.
export type LogicalUuidCannotStartContainer = AssertFalse<
  LogicalServerId extends Parameters<typeof changeContainerState>[0] ? true : false
>;
export type RawStringCannotOpenContainer = AssertFalse<
  string extends Parameters<typeof getContainer>[0] ? true : false
>;
export type ManagedContainerUsesDockerId = AssertTrue<
  ManagedContainer["id"] extends DockerContainerId ? true : false
>;
export type LogicalServerUsesLogicalId = AssertTrue<
  LogicalServer["id"] extends LogicalServerId ? true : false
>;
export type ResolvedBindingUsesDockerId = AssertTrue<
  ServerContext["logical"]["containerId"] extends DockerContainerId
    ? true
    : false
>;
