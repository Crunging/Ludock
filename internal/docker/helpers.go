package docker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/logging"
)

// CreateHelper creates a helper container from Ludock's own image. Helpers
// never start the panel or its healthcheck. A missing pinned image is pulled
// once. The caller owns its mounts, privileges, labels, and lifetime.
func (c *Client) CreateHelper(ctx context.Context, options ContainerCreate) (string, error) {
	options.Healthcheck = &Healthcheck{Test: []string{"NONE"}}
	id, err := c.CreateContainer(ctx, options)
	if err == nil || StatusCode(err) != http.StatusNotFound || !strings.Contains(options.Image, "@sha256:") {
		return id, err
	}
	if err := c.Pull(ctx, options.Image, "", ""); err != nil {
		return "", err
	}
	return c.CreateContainer(ctx, options)
}

var errHelperCleanup = apperr.New("HELPER_CLEANUP_FAILED", 409,
	"A temporary container Ludock started for this couldn’t be removed. Check Docker, then try again.")

// sleep is replaced in tests.
var sleep = func(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// RemoveHelper removes a helper and its anonymous volumes. A conflict is not
// proof of removal, so callers keep their locks until Docker confirms it is
// gone; a failed removal stops the helper before a final attempt.
func (c *Client) RemoveHelper(ctx context.Context, id string) error {
	remove := func() error {
		for attempt := 0; ; attempt++ {
			err := c.Remove(ctx, id, true, true)
			status := StatusCode(err)
			if err == nil || status == http.StatusNotFound {
				return nil
			}
			if status != http.StatusConflict || attempt >= 20 {
				return err
			}
			sleep(ctx, 100*time.Millisecond)
		}
	}
	if remove() == nil {
		return nil
	}
	zero := 0
	if err := c.Stop(ctx, id, &zero); err != nil {
		if status := StatusCode(err); status != http.StatusNotModified && status != http.StatusNotFound {
			return errHelperCleanup
		}
	}
	if remove() != nil {
		return errHelperCleanup
	}
	return nil
}

// RemoveOperationHelper retries until the helper is gone. Persisted data
// jobs cannot release their locks or restart a game while an unreachable
// helper may still be writing. It ignores ctx cancellation on purpose; an
// attempt Docker doesn't answer is retried like a failed one.
func (c *Client) RemoveOperationHelper(id string) {
	ctx := context.WithoutCancel(context.Background())
	for {
		attempt, cancel := context.WithTimeout(ctx, removeAttemptLimit)
		err := c.RemoveHelper(attempt, id)
		timedOut := errors.Is(attempt.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			return
		}
		if timedOut {
			log.Warn("Docker didn't answer in time", logging.F("step", "removing a helper that may still be writing; retrying"))
		}
		sleep(ctx, time.Second)
	}
}

// removeAttemptLimit bounds one attempt of RemoveOperationHelper; tests
// shorten it.
var removeAttemptLimit = time.Minute

var log = logging.New("docker")
