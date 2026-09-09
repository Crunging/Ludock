import { z } from "zod";

export const updateRequestSchema = z
  .object({
    createBackup: z.boolean(),
    skipBackupConfirmation: z.string().max(200).optional(),
    forceRecreate: z.boolean().default(false),
  })
  .strict();
export type CreateServerUpdateRequest = z.input<typeof updateRequestSchema>;

export const composeRegistrationSchema = z
  .object({
    projectName: z
      .string()
      .regex(/^[a-z0-9][a-z0-9_-]*$/)
      .max(128),
    projectDirectory: z.string().min(1).max(1024),
    composeFiles: z.array(z.string().min(1).max(1024)).min(1).max(16),
    envFiles: z.array(z.string().min(1).max(1024)).max(16).default([]),
  })
  .strict();
export type ComposeProjectRegistration = z.infer<
  typeof composeRegistrationSchema
>;

export const composeProjectSchema = composeRegistrationSchema.extend({
  envFiles: composeRegistrationSchema.shape.envFiles.removeDefault(),
  id: z.string().uuid(),
  disabled: z.boolean(),
});
export type ComposeProject = z.infer<typeof composeProjectSchema>;
export const composeProjectsResponseSchema = z.object({
  projects: z.array(composeProjectSchema),
});
export const composeProjectResponseSchema = z.object({
  project: composeProjectSchema,
});
export const updateCapabilitySchema = z.object({
  available: z.boolean(),
  actionLabel: z.enum(["Update server", "Update image"]),
  manager: z.enum(["compose", "external"]),
  projectName: z.string().optional(),
  serviceName: z.string().optional(),
  image: z.string().optional(),
  unavailableReason: z.string().optional(),
});
export type UpdateCapability = z.infer<typeof updateCapabilitySchema>;
export const updateCapabilityResponseSchema = z.object({
  capability: updateCapabilitySchema,
});
