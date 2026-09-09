import type { WebSocket } from "ws";
import type { WebSocketAuth } from "./auth.js";
import { hasServerCapability, type ServerCapability } from "./authorization.js";
import { getLogicalServer } from "./identity.js";
import { resolveAuthorizedServer, type ServerContext } from "./servers.js";

/** A stream stays attached to the exact reviewed binding used when it opened.
 * Check local revocation before every output frame, and refresh Docker while
 * idle so external recreation also closes an otherwise silent connection. */
export async function authorizeServerSocket(
  ws: WebSocket,
  auth: WebSocketAuth,
  serverId: string,
  capability: ServerCapability,
): Promise<{
  context: ServerContext;
  allowed: (required?: ServerCapability) => boolean;
  refresh: () => Promise<ServerContext>;
}> {
  const user = auth.validate();
  if (!user) throw new Error("Server access unavailable");
  auth.user = user;
  const context = await resolveAuthorizedServer(user, serverId, capability);
  const revision = context.logical.bindingRevision;

  const allowed = (required = capability): boolean => {
    if (ws.readyState !== ws.OPEN) return false;
    const currentUser = auth.validate();
    const binding = getLogicalServer(serverId);
    if (
      !currentUser ||
      !hasServerCapability(currentUser, serverId, capability) ||
      !hasServerCapability(currentUser, serverId, required) ||
      binding?.bindingRevision !== revision ||
      binding.containerId !== context.logical.containerId
    ) {
      ws.close(1008, "Server access changed");
      return false;
    }
    auth.user = currentUser;
    return true;
  };

  const refresh = async (): Promise<ServerContext> => {
    if (!allowed()) throw new Error("Server access unavailable");
    try {
      const refreshed = await resolveAuthorizedServer(
        auth.user,
        serverId,
        capability,
        revision,
      );
      if (!allowed()) throw new Error("Server access unavailable");
      return refreshed;
    } catch {
      ws.close(1008, "Server access changed");
      throw new Error("Server access unavailable");
    }
  };

  let checking = false;
  const interval = setInterval(() => {
    if (checking || !allowed()) return;
    checking = true;
    void refresh()
      .catch(() => undefined)
      .finally(() => {
        checking = false;
      });
  }, 2_000);
  interval.unref();
  const cleanup = () => clearInterval(interval);
  ws.once("close", cleanup);
  ws.once("error", cleanup);
  if (ws.readyState !== ws.OPEN) cleanup();
  return { context, allowed, refresh };
}
