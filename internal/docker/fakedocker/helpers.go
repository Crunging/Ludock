package fakedocker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Crunging/Ludock/internal/helper"
)

// HelperHook sees each exec before RunHelpers runs it, and answers in its
// place when handled is true. args are a helper command's arguments after
// "helper", with mount targets rewritten to their host sources, or nil for
// any other command.
type HelperHook func(c *Container, cmd, args []string, stdin *bufio.Reader, stdout, stderr func([]byte)) (code int, handled bool)

// RunHelpers runs Ludock's helpers in this process, as their containers
// would: exec'd `ludock helper` commands, and the mount proof a
// mount-validator helper prints when it starts. Other commands exit 127
// unless hook answers them. It replaces Exec and OnStart.
func (e *Engine) RunHelpers(hook HelperHook) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.OnStart = func(c *Container) {
		if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
			var out bytes.Buffer
			helper.RunMountProof(c.CreateRequest.Cmd[2], "1", &out)
			e.Update(c.ID, func(c *Container) { c.Logs = []string{strings.TrimSpace(out.String())} })
		}
	}
	e.Exec = func(c *Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		var args []string
		if len(cmd) > 2 && cmd[0] == helper.Binary && cmd[1] == "helper" {
			args = hostArgs(c, cmd[2:])
		}
		if hook != nil {
			if code, handled := hook(c, cmd, args, stdin, stdout, stderr); handled {
				return code
			}
		}
		if args == nil {
			return 127
		}
		var out, errs bytes.Buffer
		code := helper.Main(args, stdin, &out, &errs)
		// Docker sends no empty frames.
		if out.Len() > 0 {
			stdout(out.Bytes())
		}
		if errs.Len() > 0 {
			stderr(errs.Bytes())
		}
		return code
	}
}

// hostArgs rewrites the paths a helper sees under its mounts' targets to
// their host sources, since the helper runs here rather than in its
// container. Each JSON string that is an absolute path maps through the
// deepest mount holding it, as the container's file system would.
func hostArgs(c *Container, args []string) []string {
	args = append([]string(nil), args...)
	if c.CreateRequest.HostConfig == nil {
		return args
	}
	mounts := c.CreateRequest.HostConfig.Mounts
	for index := range args {
		args[index] = jsonString.ReplaceAllStringFunc(args[index], func(quoted string) string {
			var value string
			if json.Unmarshal([]byte(quoted), &value) != nil || !strings.HasPrefix(value, "/") {
				return quoted
			}
			deepest := -1
			for i, mount := range mounts {
				if (value == mount.Target || strings.HasPrefix(value, mount.Target+"/")) &&
					(deepest < 0 || len(mount.Target) > len(mounts[deepest].Target)) {
					deepest = i
				}
			}
			if deepest < 0 {
				return quoted
			}
			encoded, _ := json.Marshal(mounts[deepest].Source + value[len(mounts[deepest].Target):])
			return string(encoded)
		})
	}
	return args
}

var jsonString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
