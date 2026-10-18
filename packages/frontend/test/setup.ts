import "./dom";
import { afterEach, beforeEach, jest } from "bun:test";

// Testing Library binds `screen` during module initialization, after the DOM
// globals above have been installed.
const { cleanup } = await import("@testing-library/react");

beforeEach(() => {
  jest.restoreAllMocks();
  jest.clearAllMocks();
});
afterEach(() => {
  cleanup();
  jest.restoreAllMocks();
  jest.clearAllMocks();
});
