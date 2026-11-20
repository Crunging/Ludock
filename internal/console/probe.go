package console

import (
	"context"
	"errors"

	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/netconsole"
)

// Probe asks a running game for a console command's response, its status
// query, or both.
type Probe struct {
	// Adapter and Command are the console command, or nil and "".
	Adapter *games.Adapter
	Command string
	Query   *games.StatusQuery
	// Known is the list the command last returned in this container run, or
	// nil. A query that lists exactly these players skips the command.
	Known *[]string
}

type ProbeResult struct {
	// Response is the raw reply, so parsers see IDs that contain a secret;
	// anything shown from it is redacted with the server's secrets and
	// Secrets. ResponseErr is set when the command was asked for and has none.
	Response    string
	ResponseErr error
	// Secrets holds the console password, which can come from a file and so
	// be missing from the server's redacted environment values.
	Secrets []string
	// Status is nil when the query wasn't asked for or never ran, which
	// says nothing about the game.
	Status *helper.StatusResult
	// Unchanged: the query listed exactly Known, so the command, if asked
	// for, wasn't sent and Response is empty.
	Unchanged bool
}

// Probe runs both through one helper in the game's network namespace,
// except a console set to connect directly. A query only ever goes to the
// game's loopback. It returns an error only when nothing was asked.
func (r *Runtime) Probe(ctx context.Context, target Target, probe Probe, assertAccess func() error) (ProbeResult, error) {
	var result ProbeResult
	check := func() error {
		if assertAccess != nil {
			if err := assertAccess(); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return errCancelled
		}
		return nil
	}
	if err := check(); err != nil {
		return result, err
	}
	if target.State != "running" {
		return result, errNotRunning
	}
	env, err := r.environment(ctx, target.ContainerID)
	if err != nil {
		return result, err
	}
	var request helper.NetworkConsoleRequest
	var password, host string
	var port int
	console := probe.Adapter != nil && probe.Adapter.Transport != games.TransportContainerStdin
	if console {
		host, port, password, err = r.networkTarget(ctx, target, probe.Adapter, env)
		switch {
		case err != nil:
			result.ResponseErr, console = err, false
		case host == "":
			request = helper.NetworkConsoleRequest{Transport: string(probe.Adapter.Transport), Port: port, Password: password,
				Command: probe.Adapter.Command(probe.Command)}
		}
	} else if probe.Adapter != nil {
		result.ResponseErr = errors.New("this game console does not return responses")
	}
	if probe.Query != nil {
		if port := probe.Query.QueryPort(env); port != 0 {
			request.Query = &helper.StatusRequest{Protocol: string(probe.Query.Protocol), Port: port}
			request.Known = probe.Known
		}
	}
	direct := console && host != ""
	// A console reply the helper did get is kept over the query's word.
	answered := false
	// A direct console waits for the query when the query may make the
	// command unnecessary.
	if direct && request.Known == nil {
		result.Response, result.ResponseErr = netconsole.Execute(ctx, string(probe.Adapter.Transport), host, port, password,
			probe.Adapter.Command(probe.Command), netconsole.Control{AssertAccess: assertAccess})
		direct = false
	}
	if request.Transport != "" || request.Query != nil {
		if err := check(); err != nil {
			return ProbeResult{}, err
		}
		reply, err := r.networkHelper(ctx, target.ContainerID, request, assertAccess)
		if request.Transport != "" {
			switch {
			case err != nil:
				result.ResponseErr = err
			case reply.Response == nil:
				result.ResponseErr = errors.New("game console command failed")
			default:
				result.Response, answered = *reply.Response, true
			}
		}
		if err == nil && reply.Status != nil {
			result.Status = bounded(reply.Status)
		}
	}
	result.Unchanged = request.Known != nil && result.Status != nil && result.Status.Answered && result.Status.Lists(*request.Known) &&
		!answered
	if result.Unchanged && request.Transport != "" {
		result.Response, result.ResponseErr = "", nil
	}
	if direct && !result.Unchanged {
		if err := check(); err != nil {
			return ProbeResult{}, err
		}
		result.Response, result.ResponseErr = netconsole.Execute(ctx, string(probe.Adapter.Transport), host, port, password,
			probe.Adapter.Command(probe.Command), netconsole.Control{AssertAccess: assertAccess})
	}
	if err := check(); err != nil {
		return ProbeResult{}, err
	}
	if password != "" {
		result.Secrets = []string{password}
	}
	return result, nil
}

// bounded drops a helper's status that a real query couldn't produce.
func bounded(status *helper.StatusResult) *helper.StatusResult {
	if status.Online < 0 || status.Max < 0 || status.Online > 1_000_000 || status.Max > 1_000_000 ||
		(!status.Answered && (status.Counted || status.Online != 0 || status.Max != 0 || status.Listed || len(status.Names) > 0)) ||
		(status.Listed && len(status.Names) != status.Online) || len(status.Names) > 100 {
		return nil
	}
	for _, name := range status.Names {
		if len(name) > 64 {
			return nil
		}
	}
	return status
}
