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
export const bindingStatusSchema = z.enum([
  "active",
  "missing",
  "ambiguous",
  "review_required",
]);

export const serverSchema = z.object({
  id: z.string().uuid(),
  shortId: z.string(),
  name: z.string(),
  displayName: z.string(),
  image: z.string(),
  state: z.string(),
  status: z.string(),
  gameType: z.string(),
  gameConsole: z
    .object({
      id: z.enum([
        "minecraft-rcon",
        "source-rcon",
        "rust-webrcon",
        "telnet-console",
        "stdin-console",
      ]),
      name: z.string(),
      commandPlaceholder: z.string(),
    })
    .nullable(),
  fileRoots: z.array(
    z.object({ id: z.string(), name: z.string(), path: z.string() }),
  ),
  ports: z.array(
    z.object({ private: z.number(), public: z.number(), type: z.string() }),
  ),
  created: z.number(),
  labels: z.record(z.string()),
  permissions: z.array(serverCapabilitySchema),
  bindingStatus: bindingStatusSchema,
});
export type Server = z.infer<typeof serverSchema>;
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

export const updateRequestSchema = z
  .object({
    createBackup: z.boolean(),
    skipBackupConfirmation: z.string().max(200).optional(),
    forceRecreate: z.boolean().default(false),
  })
  .strict();
export type CreateServerUpdateRequest = z.input<typeof updateRequestSchema>;

export const backupSettingsSchema = z
  .object({
    destination: z.string().min(1).max(1024),
    retentionCount: z.number().int().min(1).max(1000),
    maxBytes: z.number().int().positive().safe(),
    reserveBytes: z.number().int().nonnegative().safe(),
  })
  .strict();
export type BackupSettings = z.infer<typeof backupSettingsSchema>;
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
export const availabilitySchema = z
  .object({
    enabled: z.boolean(),
    maintenance: z.boolean().default(false),
    graceSeconds: z.number().int().min(10).max(86400).default(120),
  })
  .strict();
export const scheduleSchema = z
  .object({
    action: z.enum(["start", "stop", "restart", "backup"]),
    enabled: z.boolean().default(true),
    time: z.string().regex(/^(?:[01]\d|2[0-3]):[0-5]\d$/),
    days: z.array(z.number().int().min(0).max(6)).min(1).max(7),
    timezone: z
      .string()
      .min(1)
      .max(100)
      .refine((value) => {
        try {
          new Intl.DateTimeFormat("en", { timeZone: value });
          return true;
        } catch {
          return false;
        }
      }, "Unknown timezone"),
  })
  .strict();
export type ScheduleInput = z.infer<typeof scheduleSchema>;
export type OperationStatus =
  | "queued"
  | "running"
  | "succeeded"
  | "already_current"
  | "failed"
  | "interrupted";
export interface Operation {
  id: string;
  serverId: string;
  kind: string;
  status: OperationStatus;
  phase: string;
  createdAt: number;
  updatedAt: number;
  error: string | null;
  result: Record<string, unknown> | null;
}
export interface Backup {
  id: string;
  serverId: string;
  createdAt: number;
  size: number;
  checksum: string;
  roots: Array<{ id: string; path: string }>;
  state: "complete" | "failed";
}
export interface UpdateCapability {
  available: boolean;
  actionLabel: "Update server" | "Update image";
  manager: "compose" | "external";
  projectName?: string;
  serviceName?: string;
  image?: string;
  unavailableReason?: string;
}
