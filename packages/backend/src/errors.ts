export class AppError extends Error {
  constructor(
    public readonly code: string,
    public readonly statusCode: number,
    message: string,
  ) {
    super(message);
  }
}
export function publicError(error: unknown): { error: string; code: string } {
  if (error instanceof AppError)
    return { error: error.message, code: error.code };
  const code = (error as { code?: unknown })?.code;
  if (code === "FORBIDDEN" || code === "SERVER_ACCESS_DENIED")
    return { error: "Server not found or access denied", code: "FORBIDDEN" };
  return {
    error: "The operation failed. Check administrator diagnostics.",
    code: "OPERATION_FAILED",
  };
}
