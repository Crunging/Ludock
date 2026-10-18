import { useEffect, useId, useMemo, useRef, useState } from "react";
import { nextScheduleRun, scheduleAnnouncementsSchema, scheduleLocalDate, scheduleSchema, type Schedule, type ScheduleAction, type ScheduleInput } from "@ludock/shared";
import { operationStatusLabels, operationTones } from "../../operations";
import StatusPip from "../StatusPip";

const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const minuteStart = () => Math.floor(Date.now() / 60_000) * 60_000;
const localTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
const timezoneOptions = [...new Set([localTimezone, "UTC", ...Intl.supportedValuesOf("timeZone")])]
  .sort()
  .map((timezone) => <option key={timezone} value={timezone} />);
const allDays = [0, 1, 2, 3, 4, 5, 6];
const dayPresets = [
  { label: "Every day", days: allDays },
  { label: "Weekdays", days: [1, 2, 3, 4, 5] },
  { label: "Weekends", days: [0, 6] },
];
const actionHelp: Record<ScheduleAction, string> = {
  start: "Starts the server if it is stopped.",
  stop: "Stops the server at the selected time. Connected players will be disconnected.",
  restart: "Stops and starts the server at the selected time. Connected players will be disconnected.",
  backup: "Each backup stops the server for the copy and restores its previous running state. An administrator must enable backup storage in Settings first.",
};

export interface ScheduleEdit {
  id: string;
  revision: number;
  draft: ScheduleInput;
}

interface Props {
  schedules: Schedule[];
  historyLoading?: boolean;
  historyUnavailable?: boolean;
  selectedScheduleId?: string | null;
  snapshotReady?: boolean;
  draft: ScheduleInput;
  editing: ScheduleEdit | null;
  conflict: boolean;
  onDraftChange: (value: ScheduleInput) => void;
  scheduleActions: ScheduleAction[];
  /** Whether the user may save or resume a schedule with these settings. */
  canRunSchedule: (schedule: ScheduleInput) => boolean;
  canCreateBackup: boolean;
  /** The game has a broadcast command and the user has console access. */
  canAnnounce: boolean;
  busy: boolean;
  saving: boolean;
  blocked: boolean;
  onSave: () => void;
  onEdit: (schedule: Schedule) => void;
  onCancelEdit: () => void;
  onResolveConflict: (useSaved: boolean) => void;
  onToggle: (schedule: Schedule) => void;
  onDelete: (schedule: Schedule) => void;
  onViewActivity: (schedule: Schedule) => void;
}

const unavailableGuidance: Record<NonNullable<Schedule["nextRunUnavailableReason"]>, string> = {
  owner_missing: "Schedule owner no longer exists. Recreate this schedule with an authorized owner.",
  owner_disabled: "Schedule owner is disabled. Ask an administrator to enable the owner’s account.",
  owner_access_removed: "Schedule owner no longer has schedule access. Ask an administrator to restore server view and schedule management grants.",
  action_access_removed: "Schedule owner no longer has access to this action. Ask an administrator to restore the action grant, or edit the schedule to use an allowed action.",
  binding_changed: "Server identity changed. Ask an administrator to review the binding, then recreate this schedule.",
  binding_unavailable: "Server binding is unavailable. Ask an administrator to resolve its identity before this schedule can run.",
  unavailable: "The next run cannot be determined. Try refreshing this page, then review the schedule settings.",
};

function formatDate(timestamp: number, timezone: string) {
  const date = new Intl.DateTimeFormat(undefined, {
    weekday: "short", year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: timezone,
  }).format(timestamp);
  return `${date} (${timezone})`;
}

function formatRun(timestamp: number, timezone: string, now: number) {
  const prefix = timestamp === now ? "Due now · " : timestamp < now ? "Awaiting scheduler · " : "";
  return `${prefix}${formatDate(timestamp, timezone)}`;
}

function when(schedule: ScheduleInput) {
  if (schedule.kind === "once") return `Once, ${schedule.date} at ${schedule.time}`;
  const preset = dayPresets.find(({ days }) =>
    days.length === schedule.days.length && days.every((day) => schedule.days.includes(day)));
  return `${schedule.time}, ${preset ? preset.label.toLowerCase() : schedule.days.map((day) => weekdays[day]).join(", ")}`;
}

function state(schedule: Schedule) {
  return !schedule.enabled ? "Paused" : schedule.completedAt !== null ? "Completed" : "Enabled";
}

function WarningLeadTimes({ minutes, onChange }: { minutes: number[]; onChange: (values: number[]) => void }) {
  const [text, setText] = useState(() => minutes.join(", "));
  const lastValue = useRef(JSON.stringify(minutes));
  useEffect(() => {
    const value = JSON.stringify(minutes);
    if (value !== lastValue.current) setText(minutes.join(", "));
    lastValue.current = value;
  }, [minutes]);
  const valid = scheduleAnnouncementsSchema.safeParse({ leadMinutes: minutes }).success;
  return (
    <label>Warning lead times (minutes)
      <input required value={text} aria-invalid={!valid} onChange={(event) => {
        const values = event.target.value.split(",").map((value) => Number(value.trim()));
        lastValue.current = JSON.stringify(values);
        setText(event.target.value);
        onChange(values);
      }} />
      <small>Comma-separated whole minutes, 1–60; up to 10 unique times. Default: 10, 5, 1.</small>
      {!valid && <small role="alert">Enter 1–10 unique whole numbers between 1 and 60.</small>}
    </label>
  );
}

export default function SchedulesPanel(props: Props) {
  const {
    schedules, draft: schedule, editing, conflict, onDraftChange, scheduleActions,
    busy, saving, blocked, onSave, onEdit, onCancelEdit, onResolveConflict, onToggle, onDelete, onViewActivity,
  } = props;
  const [now, setNow] = useState(minuteStart);
  const timezoneId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  const previousEdit = useRef(editing?.id);
  // Keep the other kind's days or date so switching back doesn't lose them.
  // They belong to one draft, so starting or leaving an edit clears them.
  const otherKind = useRef<{ days?: number[]; date?: string }>({});
  useEffect(() => {
    const interval = window.setInterval(() => setNow(minuteStart()), 1000);
    return () => window.clearInterval(interval);
  }, []);
  useEffect(() => {
    if (previousEdit.current !== editing?.id) otherKind.current = {};
    if (editing && previousEdit.current !== editing.id) heading.current?.focus();
    previousEdit.current = editing?.id;
  }, [editing]);
  const saved = editing ? schedules.find((item) => item.id === editing.id) : undefined;
  const preview = useMemo(() => {
    const parsed = scheduleSchema.safeParse(schedule);
    return parsed.success ? nextScheduleRun({ ...parsed.data, enabled: true }, now, saved?.lastSlot) : null;
  }, [schedule, now, saved?.lastSlot]);
  const missing = Boolean(editing && !saved && props.snapshotReady !== false);
  // One-time runs are saved only while their time is still ahead.
  const runAhead = preview !== null && (schedule.kind === "recurring" || preview >= now);
  const canSave = !busy && !blocked && !conflict && !missing && runAhead && props.canRunSchedule(schedule);
  const today = scheduleLocalDate(schedule.timezone, now) ?? undefined;

  function setKind(kind: ScheduleInput["kind"]) {
    if (kind === schedule.kind) return;
    const base = { action: schedule.action, createBackup: schedule.createBackup, enabled: schedule.enabled, time: schedule.time, timezone: schedule.timezone };
    if (schedule.kind === "recurring") otherKind.current.days = schedule.days;
    else otherKind.current.date = schedule.date;
    onDraftChange(kind === "once"
      ? { ...base, kind, date: otherKind.current.date ?? today ?? "" }
      : { ...base, kind, days: otherKind.current.days ?? [...allDays] });
  }
  const showForm = editing || scheduleActions.length > 0;

  return (
    <>
      <h2>Schedules</h2>
      <p className="section-lede">Start, stop, restart, or back up this server at set times, or once at a chosen date and time.</p>
      <details className="disclosure">
        <summary>How schedules run</summary>
        <p>
          Each schedule runs in its own time zone, and only while its owner still
          has access to the action. Missed times and daylight-saving gaps are
          skipped; a repeated hour runs once.
        </p>
        <p>
          A one-time schedule runs once. If Ludock isn’t running at that time and
          misses it by more than five minutes, the run is recorded as missed and
          never runs late. Edit it to choose a new time.
        </p>
        <p>Pausing stops future runs. It doesn’t undo work that already started.</p>
      </details>
      {props.selectedScheduleId && props.snapshotReady && !schedules.some((item) => item.id === props.selectedScheduleId) && (
        <p role="status" className="section-note">This schedule is no longer available or you no longer have access.</p>
      )}
      <div className="table-scroll schedules-table-scroll" tabIndex={0} role="region" aria-label="Schedule list">
        <table className="data-table schedules-table" aria-label="Schedules">
          <thead>
            <tr>
              <th scope="col">Action</th>
              <th scope="col">When</th>
              <th scope="col">State</th>
              <th scope="col">Next run</th>
              <th scope="col">Last result</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            {schedules.length === 0 && (
              <tr><td colSpan={6} className="muted">{props.historyLoading ? "Loading schedules…"
                : props.historyUnavailable ? "Schedule history is unavailable." : "No schedules."}</td></tr>
            )}
            {schedules.map((item) => (
              <tr key={item.id} id={`schedule-${item.id}`} tabIndex={-1} aria-current={editing?.id === item.id || props.selectedScheduleId === item.id ? "true" : undefined}>
                <td className="capitalize">{item.action}{item.createBackup && <> <small className="table-detail">Backup before restart</small></>}</td>
                <td>{when(item)}<small className="table-detail">{item.timezone}</small>
                  {item.announcements && <small className="table-detail">Warnings: {item.announcements.leadMinutes.join(", ")} min before</small>}
                </td>
                <td>{state(item)}</td>
                <td className="schedule-next-run">
                  {item.completedAt !== null ? "None" : !item.enabled ? "Paused" : item.nextRunAt !== null
                    ? formatRun(item.nextRunAt, item.timezone, now)
                    : "Unavailable"}
                  {(item.nextRunUnavailableReason || (item.enabled && item.completedAt === null && item.nextRunAt === null)) && (
                    <small className="table-detail">{unavailableGuidance[item.nextRunUnavailableReason ?? "unavailable"]}</small>
                  )}
                </td>
                <td className="schedule-result">
                  {item.lastOperation
                    ? <><StatusPip tone={operationTones[item.lastOperation.status]} />{operationStatusLabels[item.lastOperation.status]}</>
                    : item.lastResult || "No runs yet"}
                  {item.lastRunAt !== null && <small className="table-detail"><time dateTime={new Date(item.lastRunAt).toISOString()}>{formatDate(item.lastRunAt, item.timezone)}</time></small>}
                  {item.lastOperation && (
                    <button type="button" className="text-link schedule-activity-link" onClick={() => onViewActivity(item)}>View activity</button>
                  )}
                </td>
                <td>
                  <div className="schedule-row-actions">
                    <button
                      type="button" className="secondary-btn" id={`schedule-edit-${item.id}`}
                      disabled={busy || blocked || Boolean(editing) || scheduleActions.length === 0}
                      onClick={() => onEdit(item)}
                    >Edit</button>
                    <button
                      type="button" className="secondary-btn"
                      disabled={busy || Boolean(editing) || (item.completedAt !== null && !(item.enabled && item.lastOperation?.status === "queued")) || (!item.enabled && (blocked || !props.canRunSchedule(item)))}
                      onClick={() => onToggle(item)}
                    >{item.enabled ? "Pause" : "Resume"}</button>
                    <button
                      type="button" className="secondary-btn secondary-btn--danger"
                      disabled={busy || Boolean(editing)}
                      onClick={() => { if (window.confirm("Delete this schedule?")) onDelete(item); }}
                    >Delete</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {showForm ? (
        <form className="stack-form settings-section schedule-form" onSubmit={(event) => { event.preventDefault(); if (canSave) onSave(); }}>
          <h3 ref={heading} tabIndex={-1}>{editing ? "Edit schedule" : "Add schedule"}</h3>
          {editing && <p className="schedule-form-note">This schedule stays {schedule.enabled ? "enabled" : "paused"} when saved. Its owner is unchanged.</p>}
          {missing && <p className="alert alert--error" role="alert">This schedule is no longer available. Your draft is preserved. Cancel editing to return to the new schedule form.</p>}
          {conflict && saved && (
            <div className="schedule-conflict" role="alert">
              <p>This schedule changed elsewhere. Your draft is preserved. Review the current saved settings before saving.</p>
              <p><strong>Current saved settings:</strong> {saved.action}{saved.createBackup ? " with backup before restart" : ""} · {when(saved)} · {saved.timezone} · {state(saved)}
                {saved.announcements ? ` · Warnings: ${saved.announcements.leadMinutes.join(", ")} min before` : " · Warnings off"}
              </p>
              <div className="inline-actions">
                <button type="button" className="secondary-btn" disabled={busy} onClick={() => onResolveConflict(false)}>Keep my draft</button>
                <button type="button" className="secondary-btn" disabled={busy} onClick={() => onResolveConflict(true)}>Load saved settings</button>
              </div>
            </div>
          )}
          <fieldset className="weekday-input" disabled={saving}>
            <legend>Repeats</legend>
            <label className="check-label">
              <input type="radio" name={`${timezoneId}-kind`} checked={schedule.kind === "recurring"} onChange={() => setKind("recurring")} />On chosen days
            </label>
            <label className="check-label">
              <input type="radio" name={`${timezoneId}-kind`} checked={schedule.kind === "once"} onChange={() => setKind("once")} />Once
            </label>
          </fieldset>
          <div className="form-columns">
            <label>Action
              <select value={scheduleActions.includes(schedule.action) ? schedule.action : ""} disabled={saving} onChange={(event) => {
                const action = event.target.value as ScheduleAction;
                const restart = action === "restart";
                onDraftChange({ ...schedule, action, createBackup: restart && schedule.createBackup, announcements: restart ? schedule.announcements : undefined });
              }} required>
                <option value="" disabled>Select action</option>
                {scheduleActions.map((action) => <option key={action} value={action}>{action.charAt(0).toUpperCase() + action.slice(1)}</option>)}
              </select>
            </label>
            {schedule.kind === "once" && (
              <label>Date
                <input type="date" required min={today} value={schedule.date} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, date: event.target.value })} />
              </label>
            )}
            <label>Time
              <input type="time" required value={schedule.time} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, time: event.target.value })} />
            </label>
            <label>Time zone
              <input required list={timezoneId} aria-describedby={`${timezoneId}-help`} autoCapitalize="none" spellCheck={false} value={schedule.timezone} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, timezone: event.target.value })} />
              <datalist id={timezoneId}>{timezoneOptions}</datalist>
            </label>
          </div>
          <div className="inline-actions">
            <p className="muted" id={`${timezoneId}-help`}>Search by city, for example London or Los_Angeles. Times follow this zone, including daylight saving.</p>
            <button type="button" className="secondary-btn" disabled={saving} onClick={() => onDraftChange({ ...schedule, timezone: localTimezone })}>Use my time zone ({localTimezone})</button>
          </div>
          {schedule.kind === "recurring" && <fieldset className="weekday-input" disabled={saving}>
            <legend>Days</legend>
            <div className="inline-actions schedule-day-presets">
              {dayPresets.map(({ label, days }) => (
                <button type="button" className="secondary-btn" key={label}
                  aria-pressed={schedule.days.length === days.length && days.every((day) => schedule.days.includes(day))}
                  onClick={() => onDraftChange({ ...schedule, days: [...days] })}
                >{label}</button>
              ))}
            </div>
            {weekdays.map((day, index) => (
              <label className="check-label" key={day}>
                <input type="checkbox" checked={schedule.days.includes(index)} onChange={(event) => onDraftChange({ ...schedule, days: event.target.checked ? [...schedule.days, index].sort() : schedule.days.filter((value) => value !== index) })} />{day}
              </label>
            ))}
          </fieldset>}
          {schedule.action === "restart" && (props.canCreateBackup || schedule.createBackup) && (
            <label className="check-label">
              <input type="checkbox" checked={schedule.createBackup} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, createBackup: event.target.checked })} />
              Create a backup before restarting
            </label>
          )}
          {schedule.action === "restart" && (
            props.canAnnounce ? (
              <fieldset disabled={saving}>
                <legend>Player warnings</legend>
                <label className="check-label">
                  <input type="checkbox" checked={Boolean(schedule.announcements)} onChange={(event) => onDraftChange({
                    ...schedule, announcements: event.target.checked ? scheduleAnnouncementsSchema.parse({}) : undefined,
                  })} />
                  Announce countdown to players
                </label>
                {schedule.announcements && (
                  <WarningLeadTimes key={editing?.id ?? "new"} minutes={schedule.announcements.leadMinutes}
                    onChange={(leadMinutes) => onDraftChange({ ...schedule, announcements: { leadMinutes } })} />
                )}
                <p className="section-note">Warnings use the game console. Missed or failed warnings are skipped; the action still runs.</p>
              </fieldset>
            ) : (
              <p className="section-note">Player warnings require a supported broadcast console and console access.
                {schedule.announcements && <button type="button" className="text-link" disabled={saving} onClick={() => onDraftChange({ ...schedule, announcements: undefined })}>Turn off saved warnings</button>}
              </p>
            )
          )}
          {!editing && (
            <label className="check-label">
              <input type="checkbox" checked={!schedule.enabled} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, enabled: !event.target.checked })} />
              Create paused
            </label>
          )}
          <p className="schedule-preview">
            <strong>{schedule.enabled ? "Next run:" : "Next run when resumed:"}</strong>{" "}
            {!runAhead
              ? schedule.kind === "once"
                ? "Choose a future date and time that exists in this time zone."
                : "Choose a valid time, time zone, and at least one day."
              : formatRun(preview, schedule.timezone, now)}
            <small className="table-detail">Preview assumes the owner has access and the server binding is active.</small>
          </p>
          <p className="schedule-form-note">
            {actionHelp[schedule.action]}
            {schedule.createBackup && " The server must be running. It stays stopped while the backup runs, then starts again. If the backup fails, it stays stopped."}
          </p>
          <div className="inline-actions schedule-form-actions">
            <button className="primary-btn" disabled={!canSave}>{editing ? "Save changes" : "Add schedule"}</button>
            {editing && <button type="button" className="secondary-btn" disabled={saving} onClick={onCancelEdit}>Cancel editing</button>}
          </div>
        </form>
      ) : (
        <p className="section-note">No schedule actions are granted. An administrator must also grant the specific start, stop, restart, or backup action. Existing schedules can still be paused or deleted.</p>
      )}
    </>
  );
}
