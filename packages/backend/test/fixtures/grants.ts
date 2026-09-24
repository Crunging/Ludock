import type { ServerCapability } from "@ludock/shared";
import { listUserServerGrants, setUserServerGrants } from "../../src/authorization.js";
import type { SessionUser } from "../../src/database.js";

/** Replace one server's grant while keeping the user's other assignments. */
export function setServerGrant(
  userId: string,
  serverId: string,
  capabilities: readonly string[],
  actor: SessionUser,
) {
  const others = listUserServerGrants(userId).filter((grant) => grant.serverId !== serverId);
  return setUserServerGrants(
    userId,
    [...others, { serverId, capabilities: capabilities as ServerCapability[] }],
    actor,
  );
}
