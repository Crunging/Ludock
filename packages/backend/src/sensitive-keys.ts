/** Shared names for credentials in container environment and application logs. */
export const SENSITIVE_KEY_SOURCE =
  "password|passwd|secret|token|credential|rconpw|authorization|cookie|api[-_]?key|session";

const sensitiveKeyPattern = new RegExp(SENSITIVE_KEY_SOURCE, "i");

export function isSensitiveKey(key: string): boolean {
  return sensitiveKeyPattern.test(key);
}
