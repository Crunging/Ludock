package ops

// Result is an operation's public outcome, which Activity, Needs attention and
// schedules show. Its JSON names are stored, so each one is read forever.
type Result struct {
	// Skipped and AlreadyCurrent end a job that succeeded with that status.
	Skipped        bool   `json:"skipped,omitempty"`
	AlreadyCurrent bool   `json:"alreadyCurrent,omitempty"`
	Guidance       string `json:"guidance,omitempty"`
	// UndoNote says what finishing an earlier restore's undo left; Activity
	// shows it before Guidance.
	UndoNote string `json:"undoNote,omitempty"`
	// CancelledBy is the account that cancelled the job.
	CancelledBy     string `json:"cancelledBy,omitempty"`
	WaitedForDocker bool   `json:"waitedForDocker,omitempty"`

	SaveStatus           string `json:"saveStatus,omitempty"`
	SaveReason           string `json:"saveReason,omitempty"`
	ForceStopped         bool   `json:"forceStopped,omitempty"`
	StopRecoveryTimedOut bool   `json:"stopRecoveryTimedOut,omitempty"`
	WarnSkipped          bool   `json:"warnSkipped,omitempty"`
	// WarnFailedNote says players couldn't be warned.
	WarnFailedNote string `json:"warnFailedNote,omitempty"`
	// LeftOut names the folder holding a restore's stage that a backup left
	// out.
	LeftOut string `json:"leftOut,omitempty"`
	// ACLsLost says files that had ACLs were restored without them.
	ACLsLost bool `json:"aclsLost,omitempty"`

	// BackupID is the backup a backup, or a restart with one, made.
	BackupID string `json:"backupId,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Checksum string `json:"checksum,omitempty"`

	RestoredBackupID string `json:"restoredBackupId,omitempty"`
	SafetyBackupID   string `json:"safetyBackupId,omitempty"`
	RestoredRoot     string `json:"restoredRoot,omitempty"`
	RestoredItem     string `json:"restoredItem,omitempty"`
	DataMoved        bool   `json:"dataMoved,omitempty"`

	ImageChanged  bool   `json:"imageChanged,omitempty"`
	Recreated     bool   `json:"recreated,omitempty"`
	ForceRecreate bool   `json:"forceRecreate,omitempty"`
	Running       bool   `json:"running,omitempty"`
	Verification  string `json:"verification,omitempty"`

	Sent bool `json:"sent,omitempty"`
	// Reply is a command's redacted reply, console output.
	Reply string `json:"reply,omitempty"`
}

// JSON paths of the Result fields that SQL reads or sets.
const (
	PathCancelledBy     = "$.cancelledBy"
	PathForceStopped    = "$.forceStopped"
	PathWaitedForDocker = "$.waitedForDocker"
)
