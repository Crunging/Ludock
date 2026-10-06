import { describe, expect, it } from "bun:test";
import { connectAddress, exitDetail, serverStatus, stateSinceText } from "../src/server-lifecycle";
import { formatRelativeTime } from "../src/format";

describe("server status words", () => {
  it("names running servers by their health check", () => {
    expect(serverStatus({ state: "running", health: null })).toEqual({ label: "Running", tone: "ok" });
    expect(serverStatus({ state: "running", health: "healthy" })).toEqual({ label: "Running", tone: "ok" });
    expect(serverStatus({ state: "running", health: "starting" })).toEqual({ label: "Starting", tone: "active" });
    expect(serverStatus({ state: "running", health: "unhealthy" })).toEqual({ label: "Unhealthy", tone: "attention" });
  });

  it("tells a stop from a crash, a forced stop, or running out of memory", () => {
    const exited = (code: number, oomKilled = false) =>
      ({ state: "exited", health: null, exit: { code, oomKilled } });
    for (const code of [0, 130, 143]) expect(serverStatus(exited(code)).label).toBe("Stopped");
    expect(serverStatus(exited(137))).toEqual({ label: "Force-stopped", tone: "attention" });
    expect(serverStatus(exited(1))).toEqual({ label: "Crashed", tone: "failed" });
    expect(serverStatus(exited(137, true))).toEqual({ label: "Out of memory", tone: "failed" });
    expect(serverStatus({ state: "exited", health: null, exit: null }).label).toBe("Stopped");
    expect(exitDetail(exited(0))).toBeNull();
    expect(exitDetail(exited(2))).toBe("Exit code 2");
  });

  it("says when the current state began without repeating its label", () => {
    const now = Date.UTC(2026, 8, 15, 12);
    const since = now - 26 * 3_600_000;
    expect(stateSinceText({ state: "running", stateSince: now - 3 * 3_600_000 }, { now })).toBe("Started 3 hours ago");
    expect(stateSinceText({ state: "exited", stateSince: since }, { now })).toBe("yesterday");
    expect(stateSinceText({ state: "exited", stateSince: since }, { now, standalone: true })).toBe("Yesterday");
    expect(stateSinceText({ state: "created", stateSince: null }, { now })).toBeNull();
  });

  it("builds the address players type, bracketing IPv6 hosts", () => {
    expect(connectAddress({ connection: null })).toBeNull();
    expect(connectAddress({ connection: { host: "play.example.com", port: 25565, source: "detected" } })).toBe("play.example.com:25565");
    expect(connectAddress({ connection: { host: "2001:db8::1", port: 2456, source: "detected" } })).toBe("[2001:db8::1]:2456");
    expect(connectAddress({ connection: { host: null, port: 7777, source: "detected" } })).toBe(`${window.location.hostname}:7777`);
    expect(connectAddress({ connection: { host: "mc.example.com", port: null, source: "label" } })).toBe("mc.example.com");
    expect(connectAddress({ connection: { host: "2001:db8::1", port: null, source: "label" } })).toBe("2001:db8::1");
    // A page opened over IPv6 reports its host with brackets already.
    expect(connectAddress({ connection: { host: "[2001:db8::1]", port: 2456, source: "detected" } })).toBe("[2001:db8::1]:2456");
  });

  it("phrases recent times relative to now", () => {
    const now = Date.UTC(2026, 8, 15, 12);
    expect(formatRelativeTime(now - 20_000, now)).toBe("just now");
    expect(formatRelativeTime(now - 5 * 60_000, now)).toBe("5 minutes ago");
    expect(formatRelativeTime(now - 3 * 3_600_000, now)).toBe("3 hours ago");
    expect(formatRelativeTime(now - 26 * 3_600_000, now)).toBe("yesterday");
    expect(formatRelativeTime(now + 2 * 3_600_000, now)).toBe("in 2 hours");
  });
});
