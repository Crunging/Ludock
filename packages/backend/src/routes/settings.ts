import {
  notificationSettingsResponseSchema,
  notificationSettingsRequestSchema,
  diagnosticsResponseSchema,
  integrationsResponseSchema,
  type DiscoveryDiagnostic,
  type GameCapability,
  type GameIntegration,
} from "@ludock/shared";
import { Router, type Router as RouterType } from "express";
import { requireRole } from "../auth.js";
import { getDiscoveryDiagnostics } from "../docker.js";
import {
  getGameCapabilityMatrix,
  type GameCapability as RegistryCapability,
} from "../server-presets.js";
import {
  configureNotifications,
  notificationConfiguration,
} from "../notifications.js";
import { isComposeAvailable } from "../compose.js";
import { respond, actor, audit } from "./request.js";

export const settingsRouter: RouterType = Router();

function publicCapability(capability: RegistryCapability): GameCapability {
  return { ...capability, evidence: [...capability.evidence] };
}

function publicIntegration(
  integration: ReturnType<typeof getGameCapabilityMatrix>[number],
): GameIntegration {
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

settingsRouter.get("/api/v1/notifications", requireRole("admin"), (_req, res) =>
  respond(res, notificationSettingsResponseSchema, notificationConfiguration()),
);
settingsRouter.put(
  "/api/v1/notifications",
  requireRole("admin"),
  (req, res) => {
    const input = notificationSettingsRequestSchema.parse(req.body);
    configureNotifications(input.enabled, input.webhookUrl);
    audit(actor(res), "notifications.configured");
    respond(
      res,
      notificationSettingsResponseSchema,
      notificationConfiguration(),
    );
  },
);
settingsRouter.get(
  "/api/v1/diagnostics",
  requireRole("admin"),
  async (_req, res) => {
    let diagnostics: DiscoveryDiagnostic[] = [];
    let dockerConnected = true;
    try {
      diagnostics = await getDiscoveryDiagnostics();
    } catch {
      dockerConnected = false;
    }
    respond(res, diagnosticsResponseSchema, {
      diagnostics,
      dockerConnected,
      composeAvailable: await isComposeAvailable(),
    });
  },
);
settingsRouter.get("/api/v1/integrations", requireRole("admin"), (_req, res) =>
  respond(res, integrationsResponseSchema, {
    integrations: getGameCapabilityMatrix().map(publicIntegration),
  }),
);
