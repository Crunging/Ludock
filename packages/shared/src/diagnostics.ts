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
export const gameIntegrationSchema = z.object({
  gameType: z.string(),
  repositories: z.array(z.string()),
  /** Display name of the built-in console adapter, if any. */
  console: z.string().nullable(),
});
export type GameIntegration = z.infer<typeof gameIntegrationSchema>;
export const integrationsResponseSchema = z.object({
  integrations: z.array(gameIntegrationSchema),
});
