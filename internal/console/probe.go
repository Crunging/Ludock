package console

import (
	"context"
	"errors"
	"strings"

	"github.com/Crunging/Ludock/internal/gamequery"
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
}

type ProbeResult struct {
	// Response has the console password removed; ResponseErr is set when
	// the command was asked for and has none.
	Response    string
	ResponseErr error
	// Status is nil when the query wasn't asked for or never ran, which
	// says nothing about the game.
	Status *helper.StatusResult
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
	console := probe.Adapter != nil && probe.Adapter.Transport != games.TransportContainerStdin
	if console {
		var port int
		host, port, password, err = r.networkTarget(ctx, target, probe.Adapter, env)
		switch {
		case err != nil:
			result.ResponseErr, console = err, false
		case host == "":
			request = helper.NetworkConsoleRequest{Transport: string(probe.Adapter.Transport), Port: port, Password: password,
				Command: probe.Adapter.Command(probe.Command)}
		default:
			result.Response, result.ResponseErr = netconsole.Execute(ctx, string(probe.Adapter.Transport), host, port, password,
				probe.Adapter.Command(probe.Command), netconsole.Control{AssertAccess: assertAccess})
		}
	} else if probe.Adapter != nil {
		result.ResponseErr = errors.New("this game console does not return responses")
	}
	if probe.Query != nil {
		if port := probe.Query.QueryPort(env); port != 0 {
			request.Query = &helper.StatusRequest{Protocol: string(probe.Query.Protocol), Port: port}
		}
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
				result.Response = *reply.Response
			}
		}
		if err == nil && reply.Status != nil {
			result.Status = bounded(reply.Status)
		}
	}
	if err := check(); err != nil {
		return ProbeResult{}, err
	}
	if console && result.ResponseErr == nil && password != "" {
		// A password read from a file is not among the server's redacted
		// environment values.
		result.Response = strings.ReplaceAll(result.Response, password, "[redacted]")
	}
	return result, nil
}

// bounded drops a helper's status that a real query couldn't produce.
func bounded(status *helper.StatusResult) *helper.StatusResult {
	if status.Online < 0 || status.Max < 0 || status.Online > 1_000_000 || status.Max > 1_000_000 ||
		(!status.Answered && status.Status != (gamequery.Status{})) {
		return nil
	}
	return status
}
