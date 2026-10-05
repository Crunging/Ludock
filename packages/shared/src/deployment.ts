import { z } from "zod";

// Configuration guidance for administrators. These paths are not a substitute
// for the mount and filesystem validation performed when settings are saved.
export const deploymentSettingsResponseSchema = z.object({
  backupRoots: z.array(z.string().min(1)),
  composeRoots: z.array(z.string().min(1)),
  composeAvailable: z.boolean(),
});
export type DeploymentSettings = z.infer<typeof deploymentSettingsResponseSchema>;

/** The public name or IP address players use to reach this Docker host. */
export const connectionHostSchema = z.string().trim().toLowerCase().max(253)
  .pipe(z.union([z.hostname(), z.ipv4(), z.ipv6()]));
export const connectionSettingsSchema = z.object({
  host: connectionHostSchema.nullable(),
}).strict();
export type ConnectionSettings = z.infer<typeof connectionSettingsSchema>;
