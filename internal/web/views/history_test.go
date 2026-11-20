package views

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/servers"
)

// Every action the code can record has plain words on Activity: the literal
// actions in the source, and server.<kind> for every operation kind.
func TestAuditLabelsCoverRecordedActions(t *testing.T) {
	literal := regexp.MustCompile(`"((?:auth|authorization|availability|backup|checklist|notifications|schedule|server|settings|user)\.[a-z0-9_.-]+)"`)
	kind := regexp.MustCompile(`\.Register\("([a-z]+)"`)
	// Literals that look like actions but name capabilities or game files.
	ignored := []string{"server.properties", "server.save"}
	for _, capability := range authz.All {
		ignored = append(ignored, string(capability))
	}
	actions := map[string]bool{"server." + string(servers.Start): true, "server." + string(servers.Stop): true}
	err := filepath.WalkDir("../../..", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.HasSuffix(path, "_templ.go") || !strings.Contains(path, "internal/") && !strings.Contains(path, "cmd/") {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range literal.FindAllStringSubmatch(string(source), -1) {
			if !slices.Contains(ignored, match[1]) {
				actions[match[1]] = true
			}
		}
		for _, match := range kind.FindAllStringSubmatch(string(source), -1) {
			actions["server."+match[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !actions["server.backup"] || !actions["auth.login"] {
		t.Fatal("the source scan found no actions", len(actions))
	}
	for action := range actions {
		if auditLabels[action] != "" {
			continue
		}
		// Server work is recorded with its result as a suffix.
		base := action
		for _, status := range append([]string{"started"}, operationStatuses...) {
			base = strings.TrimSuffix(base, "."+status)
		}
		if auditLabels[base] == "" {
			t.Errorf("%s has no label in auditLabels", action)
		}
	}
}
