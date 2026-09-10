import { deploymentSettingsResponseSchema, diagnosticsResponseSchema, integrationsResponseSchema, notificationSettingsRequestSchema, notificationSettingsResponseSchema, type DiscoveryDiagnostic, type GameCapability, type GameIntegration, } from "@ludock/shared";
import path from "node:path";
import { assertRequestUser, } from "../auth.js";
import { assertAdministrator } from "../authorization.js";
import { isComposeAvailable } from "../compose.js";
import { getDiscoveryDiagnostics } from "../docker.js";
import { configureNotifications, notificationConfiguration, } from "../notifications.js";
import { getGameCapabilityMatrix, type GameCapability as RegistryCapability, } from "../server-presets.js";
import { administrator, audit, requestUser, respond, type ApiRoutes } from "./request.js";
function publicCapability(capability: RegistryCapability): GameCapability {
  return { ...capability, evidence: [...capability.evidence] };
}
function publicIntegration(integration: ReturnType<typeof getGameCapabilityMatrix>[number]): GameIntegration {
  const { capabilities } = integration;
  return {
    gameType: integration.gameType,
    repositories: [...integration.repositories],
    capabilities: {
      recognition: publicCapability(capabilities.recognition),
      platforms: publicCapability(capabilities.platforms),
      console: publicCapability(capabilities.console),
      backup: publicCapability(capabilities.backup),
      readiness: publicCapability(capabilities.readiness),
      update: publicCapability(capabilities.update),
    },
  };
}

export const settingsRoutes: ApiRoutes = {
  "/api/v1/settings/deployment": {
    GET: administrator(async (ctx) => {
      const composeAvailable = await isComposeAvailable();
      assertAdministrator(assertRequestUser(ctx.request, requestUser(ctx)));
      return respond(deploymentSettingsResponseSchema, {
        backupRoots: [...new Set((process.env.LUDOCK_BACKUP_ROOTS || "").split(path.delimiter).filter(Boolean))],
        composeRoots: [...new Set((process.env.LUDOCK_COMPOSE_ROOTS || "")
          .split(path.delimiter).map((root) => root.trim()).filter(Boolean))],
        composeAvailable,
      });
    })
  },
  "/api/v1/notifications": {
    GET: administrator(() => respond(notificationSettingsResponseSchema, notificationConfiguration())),
    PUT: administrator((ctx) => {
      const input = notificationSettingsRequestSchema.parse(ctx.body);
      configureNotifications(input.enabled, input.webhookUrl);
      audit(requestUser(ctx), "notifications.configured");
      return respond(notificationSettingsResponseSchema, notificationConfiguration());
    })
  },
  "/api/v1/diagnostics": {
    GET: administrator(async (ctx) => {
      let diagnostics: DiscoveryDiagnostic[] = [];
      let dockerConnected = true;
      try {
        diagnostics = await getDiscoveryDiagnostics();
      }
      catch {
        dockerConnected = false;
      }
      const composeAvailable = await isComposeAvailable();
      assertAdministrator(assertRequestUser(ctx.request, requestUser(ctx)));
      return respond(diagnosticsResponseSchema, {
        diagnostics,
        dockerConnected,
        composeAvailable,
      });
    })
  },
  "/api/v1/integrations": {
    GET: administrator(() => respond(integrationsResponseSchema, {
      integrations: getGameCapabilityMatrix().map(publicIntegration),
    }))
  }
};
