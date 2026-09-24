/** Failures under test: Error instances, often carrying an app code or HTTP status. */
export type TestError = Error & { code?: string; statusCode?: number };

/** Return a synchronous failure for assertions about codes and secret redaction. */
export function thrownBy(work: () => unknown): TestError {
  try {
    work();
  } catch (error) {
    return error as TestError;
  }
  throw new Error("Expected the operation to throw");
}

export async function rejectedBy(work: Promise<unknown>): Promise<TestError> {
  try {
    await work;
  } catch (error) {
    return error as TestError;
  }
  throw new Error("Expected the operation to reject");
}
