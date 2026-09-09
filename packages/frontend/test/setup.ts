import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// jsdom does not implement native modal dialogs. Browser checks cover focus trapping.
HTMLDialogElement.prototype.showModal = function () {
  this.setAttribute("open", "");
  this.querySelector<HTMLElement>("[autofocus]")?.focus();
};
HTMLDialogElement.prototype.close = function () {
  this.removeAttribute("open");
};

afterEach(cleanup);
