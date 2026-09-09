import { randomUUID } from "node:crypto";

import type {
  ApplicationLogLevel,
  ApplicationLogEntry,
  ApplicationLogContext,
} from "@ludock/shared";
export type {
  ApplicationLogLevel,
  ApplicationLogEntry,
  ApplicationLogContext,
} from "@ludock/shared";

const MAX_LOG_ENTRIES = 1_000;
const MAX_MESSAGE_LENGTH = 16_384;
const MAX_CONTEXT_VALUE_LENGTH = 4_096;
const generation = randomUUID();
const entries: ApplicationLogEntry[] = [];
let nextId = 1;

const SECRET_KEY_PATTERN =
  /(password|passwd|secret|token|authorization|cookie|api[-_]?key|session)(\s*[=:]\s*)(["']?)([^\s,"';&}]+)\3/gi;
const BEARER_PATTERN = /\bBearer\s+[^\s,;]+/gi;
const COOKIE_PATTERN = /\b(ludock_session(?:_[a-z0-9_-]+)?)=([^;\s]+)/gi;
const SENSITIVE_QUERY_PATTERN =
  /([?&](?:token|api[-_]?key|password|secret|session)=)[^&#\s]+/gi;
const JSON_SECRET_PATTERN =
  /("(?:password|passwd|secret|token|authorization|cookie|api[-_]?key|session)"\s*:\s*")([^"]*)(")/gi;

export function redactApplicationLog(value: string): string {
  return value
    .replace(BEARER_PATTERN, "Bearer [REDACTED]")
    .replace(COOKIE_PATTERN, "$1=[REDACTED]")
    .replace(SENSITIVE_QUERY_PATTERN, "$1[REDACTED]")
    .replace(JSON_SECRET_PATTERN, "$1[REDACTED]$3")
    .replace(SECRET_KEY_PATTERN, "$1$2[REDACTED]");
}

export function recordApplicationLog(input: {
  timestamp: number;
  level: ApplicationLogLevel;
  component: string;
  message: string;
  context?: ApplicationLogContext;
}): void {
  const message = redactApplicationLog(input.message);
  const context = input.context
    ? Object.fromEntries(
        Object.entries(input.context).map(([key, value]) => [
          key,
          typeof value === "string"
            ? redactApplicationLog(value).slice(0, MAX_CONTEXT_VALUE_LENGTH)
            : value,
        ]),
      )
    : undefined;
  entries.push({
    id: nextId++,
    timestamp: input.timestamp,
    level: input.level,
    component: input.component,
    message:
      message.length > MAX_MESSAGE_LENGTH
        ? `${message.slice(0, MAX_MESSAGE_LENGTH)}… [truncated]`
        : message,
    ...(context && Object.keys(context).length > 0 ? { context } : {}),
  });
  if (entries.length > MAX_LOG_ENTRIES) {
    entries.splice(0, entries.length - MAX_LOG_ENTRIES);
  }
}

export function listApplicationLogs(options: {
  after?: number;
  limit: number;
  generation?: string;
}): { generation: string; entries: ApplicationLogEntry[] } {
  const after =
    options.generation && options.generation !== generation
      ? 0
      : options.after || 0;
  return {
    generation,
    entries: entries.filter((entry) => entry.id > after).slice(-options.limit),
  };
}
