import { JSDOM } from "jsdom";

const { window } = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost:3000/",
  pretendToBeVisual: true,
});

// Keep Bun's network APIs and timers, but use one DOM realm for browser events,
// elements and form data so jsdom accepts values created by the tests.
const browserGlobals = new Set([
  "window", "self", "document", "navigator", "location", "localStorage", "sessionStorage",
  "Event", "EventTarget", "CustomEvent", "DOMException", "File", "Blob", "FormData",
]);
for (const name of Object.getOwnPropertyNames(window)) {
  if (!(name in globalThis) || browserGlobals.has(name)) {
    Object.defineProperty(globalThis, name, {
      configurable: true,
      writable: true,
      value: Reflect.get(window, name),
    });
  }
}
Object.defineProperty(globalThis, "IS_REACT_ACT_ENVIRONMENT", { configurable: true, writable: true, value: true });

// Application hooks use window timers; forwarding keeps Bun's fake clock in
// control of both browser and global timer calls.
window.setTimeout = ((...args: Parameters<typeof setTimeout>) => Number(globalThis.setTimeout(...args))) as typeof window.setTimeout;
window.clearTimeout = ((id: number) => globalThis.clearTimeout(id)) as typeof window.clearTimeout;
window.setInterval = ((...args: Parameters<typeof setInterval>) => Number(globalThis.setInterval(...args))) as typeof window.setInterval;
window.clearInterval = ((id: number) => globalThis.clearInterval(id)) as typeof window.clearInterval;
