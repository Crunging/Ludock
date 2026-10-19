package helper

import (
	"encoding/json"
	"fmt"
	"io"
)

// Binary is where the Ludock binary lives in its image. Helper containers
// run this image, so the backend launches helpers with this path.
const Binary = "/usr/local/bin/ludock"

// Command builds the argument vector for a helper subcommand.
func Command(name string, arguments ...any) []string {
	command := []string{Binary, "helper", name}
	for _, argument := range arguments {
		switch value := argument.(type) {
		case string:
			command = append(command, value)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				panic(err)
			}
			command = append(command, string(encoded))
		}
	}
	return command
}

// Main runs `ludock helper <name> …` and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	argument := func(index int) string {
		if index < len(args) {
			return args[index]
		}
		return ""
	}
	switch argument(0) {
	case "file":
		return RunFile(argument(1), stdin, stdout, stderr)
	case "idle":
		return RunIdle(argument(1))
	case "mount-proof":
		return RunMountProof(argument(1), argument(2), stdout)
	case "mount-identities":
		return RunMountIdentities(argument(1))
	case "restore":
		return RunRestore(argument(1), stdout, stderr)
	case "restore-extract":
		return RunRestoreExtract(argument(1), stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, "Usage: ludock helper <file|idle|mount-proof|mount-identities|restore|restore-extract> …")
	return 2
}
