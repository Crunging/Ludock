import type { Schedule, ScheduleInput } from "@ludock/shared";
const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

interface Props {
  schedules: Schedule[];
  draft: ScheduleInput;
  onDraftChange: (value: ScheduleInput) => void;
  scheduleActions: ScheduleInput["action"][];
  busy: boolean;
  blocked: boolean;
  onCreate: () => void;
  onDelete: (schedule: Schedule) => void;
}

export default function SchedulesPanel(props: Props) {
  const {
    schedules,
    draft: schedule,
    onDraftChange,
    scheduleActions,
    busy,
    blocked,
    onCreate,
    onDelete,
  } = props;
  return (
    <>
      <h2>Schedules</h2>
      <p className="section-note">
        Schedules run in the selected time zone and only while their owner still
        has the required access.
      </p>
      <div className="table-scroll">
        <table className="data-table">
          <thead>
            <tr>
              <th>Action</th>
              <th>When</th>
              <th>State</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {schedules.length === 0 && (
              <tr>
                <td colSpan={4} className="muted">
                  No schedules.
                </td>
              </tr>
            )}
            {schedules.map((item) => (
              <tr key={item.id}>
                <td className="capitalize">{item.action}</td>
                <td>
                  {item.time} ·{" "}
                  {item.days.map((day) => weekdays[day]).join(", ")}
                  <small className="table-detail">{item.timezone}</small>
                </td>
                <td>
                  {item.lastResult || (item.enabled ? "Enabled" : "Disabled")}
                </td>
                <td>
                  <button
                    className="secondary-btn secondary-btn--danger"
                    disabled={busy}
                    onClick={() => {
                      if (window.confirm("Delete this schedule?"))
                        onDelete(item);
                    }}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {scheduleActions.length > 0 ? (
        <form
          className="stack-form settings-section"
          onSubmit={(event) => {
            event.preventDefault();
            onCreate();
          }}
        >
          <h3>Add schedule</h3>
          <div className="form-columns">
            <label>
              Action
              <select
                value={
                  scheduleActions.includes(schedule.action)
                    ? schedule.action
                    : ""
                }
                onChange={(event) =>
                  onDraftChange({
                    ...schedule,
                    action: event.target.value as ScheduleInput["action"],
                  })
                }
                required
              >
                <option value="" disabled>
                  Select action
                </option>
                {scheduleActions.map((action) => (
                  <option key={action} value={action}>
                    {action.charAt(0).toUpperCase() + action.slice(1)}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Time
              <input
                type="time"
                required
                value={schedule.time}
                onChange={(event) =>
                  onDraftChange({ ...schedule, time: event.target.value })
                }
              />
            </label>
            <label>
              Time zone
              <input
                required
                value={schedule.timezone}
                onChange={(event) =>
                  onDraftChange({
                    ...schedule,
                    timezone: event.target.value,
                  })
                }
              />
            </label>
          </div>
          <fieldset className="weekday-input">
            <legend>Days</legend>
            {weekdays.map((day, index) => (
              <label className="check-label" key={day}>
                <input
                  type="checkbox"
                  checked={schedule.days.includes(index)}
                  onChange={(event) =>
                    onDraftChange({
                      ...schedule,
                      days: event.target.checked
                        ? [...schedule.days, index].sort()
                        : schedule.days.filter((value) => value !== index),
                    })
                  }
                />
                {day}
              </label>
            ))}
          </fieldset>
          {schedule.action === "backup" && (
            <p className="section-note">
              Each backup stops the server for the copy and restores its
              previous running state.
            </p>
          )}
          <button
            className="primary-btn"
            disabled={
              busy ||
              blocked ||
              schedule.days.length === 0 ||
              !scheduleActions.includes(schedule.action)
            }
          >
            Add schedule
          </button>
        </form>
      ) : (
        <p className="section-note">
          No schedule actions are granted. An administrator must also grant the
          specific start, stop, restart, or backup action.
        </p>
      )}
    </>
  );
}
