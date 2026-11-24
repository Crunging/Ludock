package games

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// The reference's two supported games tables say what Integrations says,
// game by game, label value included, so the docs and Settings → System can't
// drift apart.
func TestReferenceListsIntegrations(t *testing.T) {
	data, err := os.ReadFile("../../docs/REFERENCE.md")
	if err != nil {
		t.Fatal(err)
	}
	section, _, _ := strings.Cut(strings.SplitN(string(data), "## Supported games", 2)[1], "\n## ")
	tables := map[string]map[string]string{}
	var header string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") {
			header = ""
			continue
		}
		if header == "" {
			header = line
			tables[header] = map[string]string{}
			continue
		}
		if strings.HasPrefix(line, "| ---") {
			continue
		}
		cells := strings.Split(strings.TrimSpace(line), "|")
		cells = cells[1 : len(cells)-1]
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		for _, name := range strings.Split(cells[0], ", ") {
			tables[header][name] = strings.Join(cells[1:], " | ")
		}
	}
	consoles := tables["| Game | `ludock.game` | Images | Console (port) | Status check (port) |"]
	extras := tables["| Game | Who's online | Kick and ban | Warnings to players | Restart updates the game |"]
	if len(consoles) != len(Integrations) {
		t.Errorf("the reference lists %d games' images, Ludock %d", len(consoles), len(Integrations))
	}
	mark := map[bool]string{true: "✓"}
	for i := range Integrations {
		game := &Integrations[i]
		f := game.Features()
		images := "`" + strings.Join(game.Repositories, "`, `") + "`"
		if want := "`" + game.GameType + "` | " + images + " | " + referenceConsole(f) + " | " + referenceStatusCheck(f); consoles[game.Name] != want {
			t.Errorf("%s: the reference says %q, want %q", game.Name, consoles[game.Name], want)
		}
		want := strings.Join([]string{f.Players, f.Moderation, mark[f.Warnings], mark[f.RestartUpdates]}, " | ")
		if got, listed := extras[game.Name]; listed != f.Extras() || (listed && got != want) {
			t.Errorf("%s: the reference says %q, want %q", game.Name, got, want)
		}
	}
}

// referenceConsole is a console as the reference writes it.
func referenceConsole(f Features) string {
	switch {
	case f.Console == "":
		return "—"
	case f.ServerInput:
		return f.Console + " (server input)"
	case f.AnnouncePort != 0:
		return f.Console + " (" + strconv.Itoa(f.ConsolePort) + "), REST API (" + strconv.Itoa(f.AnnouncePort) + ") for warnings"
	}
	return f.Console + " (" + strconv.Itoa(f.ConsolePort) + ")"
}

// referenceStatusCheck is a status check as the reference writes it.
func referenceStatusCheck(f Features) string {
	if f.ImageHealthCheck {
		return "Health check"
	}
	if f.StatusCheck == "" {
		return "—"
	}
	ports := []string{strconv.Itoa(f.StatusPort)}
	for _, name := range f.StatusPortEnv {
		ports = append(ports, "`"+name+"`")
	}
	return f.StatusCheck + " (" + strings.Join(ports, ", ") + ")"
}
