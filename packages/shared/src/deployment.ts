import { z } from "zod";

// Configuration guidance for administrators. These paths are not a substitute
// for the mount and filesystem validation performed when settings are saved.
export const deploymentSettingsResponseSchema = z.object({
  backupRoots: z.array(z.string().min(1)),
  composeRoots: z.array(z.string().min(1)),
  composeAvailable: z.boolean(),
});
export type DeploymentSettings = z.infer<typeof deploymentSettingsResponseSchema>;
