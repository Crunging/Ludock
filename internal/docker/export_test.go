package docker

import "time"

// SetRemoveAttemptLimit shortens one attempt of RemoveOperationHelper.
func SetRemoveAttemptLimit(limit time.Duration) (restore func()) {
	previous := removeAttemptLimit
	removeAttemptLimit = limit
	return func() { removeAttemptLimit = previous }
}
