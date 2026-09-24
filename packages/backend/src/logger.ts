import {
  recordApplicationLog,
} from "./application-logs.js";

const LOG_LEVELS = ["error", "warn", "info", "debug"] as const;

type LogLevel = (typeof LOG_LEVELS)[number];
type LogContext = Record<
  string,
  string | number | boolean | null | undefined
>;

const LOG_LEVEL_PRIORITY: Record<LogLevel, number> = {
  error: 0,
  warn: 1,
  info: 2,
  debug: 3,
};

interface LogLevelConfiguration {
  level: LogLevel;
  invalidValue?: string;
}

export function getLogLevelConfiguration(
  configured = process.env.LOG_LEVEL
): LogLevelConfiguration {
  const value = configured?.trim().toLowerCase();
  if (!value) return { level: "info" };
  if (LOG_LEVELS.includes(value as LogLevel)) {
    return { level: value as LogLevel };
  }
  return { level: "info", invalidValue: configured };
}

export function createLogger(component: string) {
  return {
    error: (message: string, context?: LogContext) =>
      writeLog("error", component, message, context),
    warn: (message: string, context?: LogContext) =>
      writeLog("warn", component, message, context),
    info: (message: string, context?: LogContext) =>
      writeLog("info", component, message, context),
    debug: (message: string, context?: LogContext) =>
      writeLog("debug", component, message, context),
  };
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function writeLog(
  level: LogLevel,
  component: string,
  message: string,
  context?: LogContext
): void {
  const configuredLevel = getLogLevelConfiguration().level;
  if (LOG_LEVEL_PRIORITY[level] > LOG_LEVEL_PRIORITY[configuredLevel]) return;

  const timestamp = Date.now();
  const sanitized = recordApplicationLog({
    timestamp,
    level,
    component,
    message,
    context,
  });
  const fields = sanitized.context;
  const suffix =
    fields && Object.keys(fields).length > 0 ? ` ${JSON.stringify(fields)}` : "";
  const line = `${new Date(timestamp).toISOString()} ${level.toUpperCase()} [${component}] ${sanitized.message}${suffix}`;

  if (level === "error") console.error(line);
  else if (level === "warn") console.warn(line);
  else if (level === "debug") console.debug(line);
  else console.info(line);
}
