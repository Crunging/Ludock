import { z } from "zod";

export const discoveryDiagnosticSchema = z.object({
  containerId: z.string(),
  name: z.string(),
  code: z.enum(["INVALID_ENABLE_LABEL", "INVALID_COMPOSE_IDENTITY"]),
  message: z.string(),
});
export type DiscoveryDiagnostic = z.infer<typeof discoveryDiagnosticSchema>;
export const diagnosticsResponseSchema = z.object({
  diagnostics: z.array(discoveryDiagnosticSchema),
  dockerConnected: z.boolean(),
  composeAvailable: z.boolean(),
});
export const gameCapabilitySchema = z.object({
  status: z.enum(["supported", "conditional", "unsupported", "unverified"]),
  description: z.string(),
  evidence: z.array(z.string()),
});
export type GameCapability = z.infer<typeof gameCapabilitySchema>;
export const gameIntegrationSchema = z.object({
  gameType: z.string(),
  repositories: z.array(z.string()),
  capabilities: z.object({
    recognition: gameCapabilitySchema,
    platforms: gameCapabilitySchema,
    console: gameCapabilitySchema,
    backup: gameCapabilitySchema,
    readiness: gameCapabilitySchema,
    update: gameCapabilitySchema,
  }),
});
export type GameIntegration = z.infer<typeof gameIntegrationSchema>;
export const integrationsResponseSchema = z.object({
  integrations: z.array(gameIntegrationSchema),
});
