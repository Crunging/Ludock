import { describe, expect, it, mock } from "bun:test";
import { render, screen } from "@testing-library/react";
import type { AvailabilityPolicy, AvailabilityState } from "@ludock/shared";
import AvailabilityPanel from "../src/components/server-detail/AvailabilityPanel";
import { NavigationContext } from "../src/navigation-context";

const policy: AvailabilityPolicy = { enabled: true, maintenance: false, graceSeconds: 120 };
const state: AvailabilityState = {
  outageStartedAt: null,
  notified: false,
  suppressedUntil: 0,
  intentionallyStopped: false,
  lastState: "running",
};

function panel(overrides: Partial<Parameters<typeof AvailabilityPanel>[0]> = {}) {
  return <NavigationContext.Provider value={{ pathname: "/servers/test", navigate: mock() }}><AvailabilityPanel
    policy={policy}
    state={state}
    value={policy}
    admin={false}
    busy={false}
    onChange={mock()}
    onSave={mock()}
    {...overrides}
  /></NavigationContext.Provider>;
}

describe("availability status", () => {
  it("shows an outage and observed Docker failure without administrator controls", () => {
    const startedAt = Date.now() - 180_000;
    const view = render(panel({ state: { ...state, lastState: "docker_unavailable", outageStartedAt: startedAt } }));
    expect(screen.getByRole("status").textContent).toBe("Availability problem detected.");
    expect(screen.getByText("Outage since")).toBeTruthy();
    expect(view.container.querySelector("time")?.dateTime).toBe(new Date(startedAt).toISOString());
    expect(screen.getByText("docker unavailable")).toBeTruthy();
    expect(screen.getByText(/Ask an administrator/)).toBeTruthy();
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.queryByRole("spinbutton")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("links administrators to Docker diagnostics for unreachable Docker", () => {
    render(panel({ admin: true, state: { ...state, lastState: "docker_unavailable", outageStartedAt: Date.now() - 180_000 } }));
    expect(screen.getByRole("link", { name: "Check Docker connectivity in Diagnostics" }).getAttribute("href")).toBe("/diagnostics");
    expect(screen.getByText(/Ludock cannot reach Docker/)).toBeTruthy();
  });

  it("uses saved monitoring policy while an administrator edits a draft", () => {
    render(panel({
      admin: true,
      busy: true,
      value: { ...policy, enabled: false, maintenance: true },
      state: { ...state, outageStartedAt: Date.now() - 180_000 },
    }));
    expect(screen.getByRole("status").textContent).toBe("Availability problem detected.");
    expect(screen.getByText(/A running container can still be unhealthy or starting/)).toBeTruthy();
    expect((screen.getByRole("checkbox", { name: "Monitor this server" }) as HTMLInputElement).checked).toBe(false);
    expect((screen.getByRole("checkbox", { name: "Monitor this server" }) as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save monitoring" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("distinguishes the failure grace period from an established outage", () => {
    render(panel({ state: { ...state, lastState: "exited", outageStartedAt: Date.now() } }));
    expect(screen.getByRole("status").textContent).toBe("Availability problem detected. Waiting for the failure grace period.");
    expect(screen.getByText("Outage since")).toBeTruthy();
  });

  it("distinguishes unknown, initial, and healthy observations", () => {
    const view = render(panel({ state: null }));
    expect(screen.getByRole("status").textContent).toBe("Availability status unavailable.");
    view.rerender(panel({ state: { ...state, lastState: null } }));
    expect(screen.getByRole("status").textContent).toBe("Waiting for the first availability observation.");
    view.rerender(panel());
    expect(screen.getByRole("status").textContent).toBe("No outage detected.");
  });

  it("does not present recorded outages as current while monitoring is paused", () => {
    const priorOutage = { ...state, outageStartedAt: Date.now() - 180_000 };
    const view = render(panel({ policy: { ...policy, enabled: false }, state: priorOutage }));
    expect(screen.getByRole("status").textContent).toBe("Monitoring disabled.");
    expect(screen.queryByText("Outage since")).toBeNull();
    view.rerender(panel({ policy: { ...policy, maintenance: true }, state: priorOutage }));
    expect(screen.getByRole("status").textContent).toBe("Monitoring paused for maintenance.");
    expect(screen.queryByText("Outage since")).toBeNull();
    view.rerender(panel({ monitoringPaused: true, state: priorOutage }));
    expect(screen.getByRole("status").textContent).toBe("Monitoring paused while a server operation is active.");
    expect(screen.queryByText("Outage since")).toBeNull();
    view.rerender(panel({ state: { ...priorOutage, intentionallyStopped: true } }));
    expect(screen.getByRole("status").textContent).toContain("Intentionally stopped.");
    expect(screen.queryByText("Outage since")).toBeNull();
    view.rerender(panel({ state: { ...priorOutage, suppressedUntil: Date.now() + 60_000 } }));
    expect(screen.getByRole("status").textContent).toBe("Waiting for monitoring to resume after a server action.");
    expect(screen.getByText("Monitoring resumes")).toBeTruthy();
    expect(screen.queryByText("Outage since")).toBeNull();
  });
});
