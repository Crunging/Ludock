package launch

import (
	"context"
	"errors"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("launch")

// StepLimit bounds a Docker call that should answer at once, such as
// starting a helper; tests shorten it.
var StepLimit = time.Minute

var ErrDockerTimeout = apperr.New("DOCKER_TIMEOUT", 504, "Docker didn’t answer in time. Try again in a moment.")

// Step runs one Docker call within StepLimit. A call that runs out of time
// is logged by step, which must name no container or path, and fails with
// ErrDockerTimeout unless it already has a public error.
func Step(ctx context.Context, step string, call func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, StepLimit)
	defer cancel()
	err := call(bounded)
	if err == nil || ctx.Err() != nil || !errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return err
	}
	log.Warn("Docker didn't answer in time", logging.F("step", step))
	if _, public := apperr.As(err); public {
		return err
	}
	return ErrDockerTimeout
}
