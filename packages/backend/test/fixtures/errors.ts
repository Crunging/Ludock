/** Return a synchronous failure for assertions about codes and secret redaction. */
export function thrownBy(work: () => unknown): unknown {
  try {
    work();
  } catch (error) {
    return error;
  }
  throw new Error("Expected the operation to throw");
}

export async function rejectedBy(work: Promise<unknown>): Promise<unknown> {
  try {
    await work;
  } catch (error) {
    return error;
  }
  throw new Error("Expected the operation to reject");
}
