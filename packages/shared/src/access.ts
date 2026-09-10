import { z } from "zod";

export const SERVER_CAPABILITIES = [
  "server.view",
  "server.start",
  "server.stop",
  "server.restart",
  "logs.read",
  "console.execute",
  "files.read",
  "files.write",
  "backups.create",
  "schedules.manage",
  "server.update",
  "server.recreate",
  "backups.restore",
  "backups.read",
  "backups.delete",
  "console.shell",
] as const;
export const serverCapabilitySchema = z.enum(SERVER_CAPABILITIES);
export type ServerCapability = z.infer<typeof serverCapabilitySchema>;
export const roleSchema = z.enum(["admin", "operator", "viewer"]);
export type UserRole = z.infer<typeof roleSchema>;

export const serverGrantsSchema = z
  .object({
    grants: z
      .array(
        z
          .object({
            serverId: z.string().uuid(),
            capabilities: z
              .array(serverCapabilitySchema)
              .max(SERVER_CAPABILITIES.length),
          })
          .strict(),
      )
      .max(1000),
  })
  .strict();

export const serverGrantSchema = serverGrantsSchema.shape.grants.element.extend(
  {
    updatedAt: z.number().int().nonnegative(),
  },
);
export type ServerGrant = z.infer<typeof serverGrantSchema>;
export const serverGrantsResponseSchema = z.object({
  grants: z.array(serverGrantSchema),
});

export type ServerGrantInput = z.infer<
  typeof serverGrantsSchema
>["grants"][number];
