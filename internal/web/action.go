package web

import (
	"context"
	"net/http"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/servers"
)

// serverAction is the context of a direct server action. The URL ID is a
// logical ID: Docker is reached only through the resolved binding, and the
// resource locks last until Done, after the response and helper cleanup.
type serverAction struct {
	*servers.Context
	Ctx    context.Context
	cancel context.CancelCauseFunc
	assert func(*identity.Observation) error
	done   func()
}

// Assert rechecks the session, the capability, and the binding.
func (a *serverAction) Assert() error { return a.assert(nil) }

// AssertObserved also confirms a fresh inspection.
func (a *serverAction) AssertObserved(observation identity.Observation) error {
	return a.assert(&observation)
}

// Done releases the locks and stops revalidation.
func (a *serverAction) Done() { a.done() }

var errRequestClosed = apperr.New("REQUEST_CLOSED", 409, "Request closed")

func (s *Server) startAction(r *http.Request, request *Request, serverID string, capability authz.Capability) (*serverAction, error) {
	app := s.App
	resolved, err := app.Servers.ResolveAuthorized(r.Context(), request.User(), serverID, capability, -1)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	assert := func(observation *identity.Observation) error {
		if ctx.Err() != nil {
			if cause := context.Cause(ctx); cause != nil && cause != context.Canceled {
				return cause
			}
			return errRequestClosed
		}
		current, err := app.Auth.Recheck(context.WithoutCancel(ctx), request.Principal)
		if err != nil {
			return err
		}
		if _, err := app.Authz.Assert(context.WithoutCancel(ctx), &current.User, resolved.Logical.ID, capability); err != nil {
			return err
		}
		if observation != nil {
			_, err = app.Identity.AssertObservedBinding(context.WithoutCancel(ctx), resolved.Logical.ID, *observation, resolved.Logical.BindingRevision)
		} else {
			_, err = app.Identity.ResolveBinding(context.WithoutCancel(ctx), resolved.Logical.ID, resolved.Logical.BindingRevision)
		}
		return err
	}
	if err := assert(nil); err != nil {
		cancel(nil)
		return nil, err
	}
	release, err := app.Locks.Acquire(resolved.LockKeys, nil)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := assert(nil); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	action := &serverAction{Context: resolved, Ctx: ctx, cancel: cancel, assert: assert}
	finished := false
	action.done = func() {
		if finished {
			return
		}
		finished = true
		close(stop)
		release()
		cancel(nil)
	}
	return action, nil
}
