package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrNotSent is a request whose connection to Docker never opened, so Docker
// can't have acted on it.
var ErrNotSent = fmt.Errorf("%w: never sent", ErrRequestFailed)

// dialError marks a failure to connect, before any request byte was sent.
type dialError struct{ error }

func (e dialError) Unwrap() error { return e.error }

// NotSent reports whether err proves the request never reached Docker.
func NotSent(err error) bool {
	var dial dialError
	var open *StreamOpenError
	return errors.As(err, &dial) || errors.Is(err, ErrNotSent) || (errors.As(err, &open) && !open.RequestStarted)
}

type rideOutKey struct{}

// RideOut returns a context under which reads, image pulls, and requests
// that never reached Docker wait for an unreachable Docker, up to the
// client's OutageLimit, instead of failing at once. waited is called each
// time a wait begins. Pages and background polling don't use it: they say
// Docker is unreachable right away.
func RideOut(ctx context.Context, waited func()) context.Context {
	if waited == nil {
		waited = func() {}
	}
	return context.WithValue(ctx, rideOutKey{}, waited)
}

// defaultOutageLimit covers a daemon restarted for an upgrade or a
// configuration change, which usually takes seconds.
const defaultOutageLimit = 90 * time.Second

func (c *Client) outageLimit() time.Duration {
	if c.OutageLimit > 0 {
		return c.OutageLimit
	}
	return defaultOutageLimit
}

// outage tracks one wait for Docker, which ends OutageLimit after its first
// failure.
type outage struct {
	client *Client
	since  *time.Time
}

func (c *Client) newOutage() outage { return outage{client: c, since: new(time.Time)} }

// attempt bounds an attempt made while waiting by what is left of the wait.
func (o outage) attempt(ctx context.Context) (context.Context, context.CancelFunc) {
	if o.since.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, o.since.Add(o.client.outageLimit()))
}

// retry waits a moment before another attempt and reports whether to make
// one: err found Docker unavailable, ctx rides out outages, and the limit
// hasn't passed.
func (o outage) retry(ctx context.Context, err error) bool {
	waited, ok := ctx.Value(rideOutKey{}).(func())
	if !ok || !Unavailable(err) || ctx.Err() != nil {
		return false
	}
	if o.since.IsZero() {
		*o.since = time.Now()
	}
	remaining := o.client.outageLimit() - time.Since(*o.since)
	if remaining <= time.Second/10 {
		return false
	}
	waited()
	sleep(ctx, min(time.Second, remaining/2))
	return ctx.Err() == nil
}

// WaitForDocker waits, under RideOut, for Docker to answer again after err
// showed it unavailable. True means it answers: the caller redoes a step
// whose request never reached Docker (NotSent) from its last check, or reads
// what became of one that may have. since holds when the caller's wait
// began, zero before its first failure; the wait ends OutageLimit after it.
func (c *Client) WaitForDocker(ctx context.Context, err error, since *time.Time) bool {
	if _, ok := ctx.Value(rideOutKey{}).(func()); !ok {
		return false
	}
	wait := outage{client: c, since: since}
	for {
		if !wait.retry(ctx, err) {
			return false
		}
		attempt, cancel := wait.attempt(Promptly(ctx))
		err = c.Ping(attempt)
		cancel()
		if err == nil {
			return true
		}
	}
}

// Promptly undoes RideOut, for work that is done on time or not at all.
func Promptly(ctx context.Context) context.Context {
	return context.WithValue(ctx, rideOutKey{}, nil)
}

func dial(ctx context.Context, socket string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, dialError{err}
	}
	return conn, nil
}
