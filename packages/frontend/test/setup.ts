import "./dom";
import { afterEach, beforeEach, jest } from "bun:test";

// Testing Library binds `screen` during module initialization, after the DOM
// globals above have been installed.
const { cleanup } = await import("@testing-library/react");

// jsdom does not implement native modal dialogs. Browser checks cover focus trapping.
HTMLDialogElement.prototype.showModal = function () {
  this.setAttribute("open", "");
  this.querySelector<HTMLElement>("[autofocus]")?.focus();
};
HTMLDialogElement.prototype.close = function () {
  this.removeAttribute("open");
};

beforeEach(() => {
  jest.restoreAllMocks();
  jest.clearAllMocks();
});
afterEach(() => {
  cleanup();
  jest.restoreAllMocks();
  jest.clearAllMocks();
});
