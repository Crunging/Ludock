import { useEffect, useMemo, useRef, useState } from "react";
import { nextScheduleRun, scheduleSchema, type Schedule, type ScheduleInput } from "@ludock/shared";

const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const minuteStart = () => Math.floor(Date.now() / 60_000) * 60_000;

export interface ScheduleEdit {
  id: string;
  revision: number;
  draft: ScheduleInput;
}

interface Props {
  schedules: Schedule[];
  draft: ScheduleInput;
  editing: ScheduleEdit | null;
  conflict: boolean;
  onDraftChange: (value: ScheduleInput) => void;
  scheduleActions: ScheduleInput["action"][];
  busy: boolean;
  saving: boolean;
  blocked: boolean;
  onSave: () => void;
  onEdit: (schedule: Schedule) => void;
  onCancelEdit: () => void;
  onResolveConflict: (useSaved: boolean) => void;
  onToggle: (schedule: Schedule) => void;
  onDelete: (schedule: Schedule) => void;
}

function formatRun(timestamp: number, timezone: string, now: number) {
  const date = new Intl.DateTimeFormat(undefined, {
    weekday: "short", year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: timezone,
  }).format(timestamp);
  const prefix = timestamp === now ? "Due now · " : timestamp < now ? "Awaiting scheduler · " : "";
  return `${prefix}${date} (${timezone})`;
}

function when(schedule: ScheduleInput) {
  return `${schedule.time} · ${schedule.days.map((day) => weekdays[day]).join(", ")}`;
}

export default function SchedulesPanel(props: Props) {
  const {
    schedules, draft: schedule, editing, conflict, onDraftChange, scheduleActions,
    busy, saving, blocked, onSave, onEdit, onCancelEdit, onResolveConflict, onToggle, onDelete,
  } = props;
  const [now, setNow] = useState(minuteStart);
  const heading = useRef<HTMLHeadingElement>(null);
  const previousEdit = useRef(editing?.id);
  useEffect(() => {
    const interval = window.setInterval(() => setNow(minuteStart()), 1000);
    return () => window.clearInterval(interval);
  }, []);
  useEffect(() => {
    if (editing && previousEdit.current !== editing.id) heading.current?.focus();
    previousEdit.current = editing?.id;
  }, [editing]);
  const saved = editing ? schedules.find((item) => item.id === editing.id) : undefined;
  const preview = useMemo(() => {
    const parsed = scheduleSchema.safeParse(schedule);
    return parsed.success ? nextScheduleRun({ ...parsed.data, enabled: true }, now, saved?.lastSlot) : null;
  }, [schedule, now, saved?.lastSlot]);
  const missing = Boolean(editing && !saved);
  const canSave = !busy && !blocked && !conflict && !missing && preview !== null && scheduleActions.includes(schedule.action);
  const showForm = editing || scheduleActions.length > 0;

  return (
    <>
      <h2>Schedules</h2>
      <p className="section-note">
        Schedules run in their selected time zone while their owner has the required access.
        Missed times and daylight-saving gaps are skipped; repeated times run once.
        Pausing prevents future runs and does not undo work already started.
      </p>
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
              <tr><td colSpan={6} className="muted">No schedules.</td></tr>
            )}
            {schedules.map((item) => (
              <tr key={item.id} aria-current={editing?.id === item.id ? "true" : undefined}>
                <td className="capitalize">{item.action}</td>
                <td>{when(item)}<small className="table-detail">{item.timezone}</small></td>
                <td>{item.enabled ? "Enabled" : "Paused"}</td>
                <td className="schedule-next-run">
                  {!item.enabled ? "Paused" : item.nextRunAt !== null
                    ? formatRun(item.nextRunAt, item.timezone, now)
                    : <>Unavailable<small className="table-detail">Owner access or server binding prevents execution.</small></>}
                </td>
                <td className="schedule-result">{item.lastResult || "No runs yet"}</td>
                <td>
                  <div className="schedule-row-actions">
                    <button
                      type="button" className="secondary-btn" id={`schedule-edit-${item.id}`}
                      disabled={busy || blocked || Boolean(editing) || scheduleActions.length === 0}
                      onClick={() => onEdit(item)}
                    >Edit</button>
                    <button
                      type="button" className="secondary-btn"
                      disabled={busy || Boolean(editing) || (!item.enabled && (blocked || !scheduleActions.includes(item.action)))}
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
              <p><strong>Current saved settings:</strong> {saved.action} · {when(saved)} · {saved.timezone} · {saved.enabled ? "Enabled" : "Paused"}</p>
              <div className="inline-actions">
                <button type="button" className="secondary-btn" disabled={busy} onClick={() => onResolveConflict(false)}>Keep my draft</button>
                <button type="button" className="secondary-btn" disabled={busy} onClick={() => onResolveConflict(true)}>Load saved settings</button>
              </div>
            </div>
          )}
          <div className="form-columns">
            <label>Action
              <select value={scheduleActions.includes(schedule.action) ? schedule.action : ""} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, action: event.target.value as ScheduleInput["action"] })} required>
                <option value="" disabled>Select action</option>
                {scheduleActions.map((action) => <option key={action} value={action}>{action.charAt(0).toUpperCase() + action.slice(1)}</option>)}
              </select>
            </label>
            <label>Time
              <input type="time" required value={schedule.time} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, time: event.target.value })} />
            </label>
            <label>Time zone
              <input required value={schedule.timezone} disabled={saving} onChange={(event) => onDraftChange({ ...schedule, timezone: event.target.value })} />
            </label>
          </div>
          <fieldset className="weekday-input" disabled={saving}>
            <legend>Days</legend>
            {weekdays.map((day, index) => (
              <label className="check-label" key={day}>
                <input type="checkbox" checked={schedule.days.includes(index)} onChange={(event) => onDraftChange({ ...schedule, days: event.target.checked ? [...schedule.days, index].sort() : schedule.days.filter((value) => value !== index) })} />{day}
              </label>
            ))}
          </fieldset>
          <p className="schedule-preview">
            <strong>{schedule.enabled ? "Next run:" : "Next run when resumed:"}</strong>{" "}
            {preview === null ? "Choose a valid time, time zone, and at least one day." : formatRun(preview, schedule.timezone, now)}
            <small className="table-detail">Preview assumes the owner has access and the server binding is active.</small>
          </p>
          {schedule.action === "backup" && <p className="schedule-form-note">Each backup stops the server for the copy and restores its previous running state.</p>}
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
