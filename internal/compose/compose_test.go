package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

var ctx = context.Background()

func TestParseFileFollowsComposeRules(t *testing.T) {
	model, err := ParseFile([]byte(`
x-base: &base
  image: itzg/minecraft-server
  environment: {EULA: "TRUE"}
services:
  mc:
    <<: *base
    image: itzg/minecraft-server:java21
    ports: ["25565:25565"]
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := model["services"].(map[string]any)["mc"].(map[string]any)
	if mc["image"] != "itzg/minecraft-server:java21" || mc["environment"].(map[string]any)["EULA"] != "TRUE" {
		t.Fatal(mc)
	}
	for name, text := range map[string]string{
		"duplicate": "services:\n  a: {}\n  a: {}\n",
		"reset tag": "services:\n  a:\n    ports: !reset []\n",
		"binary":    "services:\n  a:\n    x: !!binary aGk=\n",
		"aliases":   "a: &a [1]\nb: [" + strings.TrimSuffix(strings.Repeat("*a,", 25), ",") + "]\n",
		"not yaml":  "services: [\n",
	} {
		if _, err := ParseFile([]byte(text)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestApprovedFilesNeverFollowLinks(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "stack"), 0o755)
	os.WriteFile(filepath.Join(root, "stack", "compose.yaml"), []byte("services: {}\n"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "compose.yaml"), []byte("secret"), 0o644)
	os.Symlink(outside, filepath.Join(root, "linked"))
	roots := []string{root}
	if data, _, err := ReadApprovedFile(filepath.Join(root, "stack", "compose.yaml"), roots, false); err != nil || string(data) != "services: {}\n" {
		t.Fatal(err)
	}
	if _, _, err := ReadApprovedFile(filepath.Join(root, "linked", "compose.yaml"), roots, false); !apperr.Is(err, "UNSAFE_CONFIG_PATH") {
		t.Fatal(err)
	}
	if _, _, err := ReadApprovedFile(filepath.Join(outside, "compose.yaml"), roots, false); !apperr.Is(err, "UNAPPROVED_PATH") {
		t.Fatal(err)
	}
	if _, present, err := ReadApprovedFile(filepath.Join(root, "stack", ".env"), roots, true); err != nil || present {
		t.Fatal(err)
	}
	if _, err := ConfiguredRoots("/etc"); err == nil {
		t.Fatal("accepted /etc as a root")
	}
}

func TestDiscoverSource(t *testing.T) {
	source, err := DiscoverSource("games", map[string]string{
		WorkingDirLabel: "/srv/games", ConfigFilesLabel: "/srv/games/compose.yaml,/srv/games/extra.yaml"})
	if err != nil || !source.LoadDefaultEnv || len(source.Project.ComposeFiles) != 2 {
		t.Fatal(source, err)
	}
	saved := `{"project":{"projectName":"games","projectDirectory":"/srv/games","composeFiles":["/srv/games/compose.yaml"],"envFiles":[]},"loadDefaultEnv":false}`
	if source, err := DiscoverSource("games", map[string]string{SourceLabel: saved}); err != nil || source.LoadDefaultEnv {
		t.Fatal(source, err)
	}
	for _, labels := range []map[string]string{
		{SourceLabel: "not json"},
		{SourceLabel: saved, WorkingDirLabel: "/x"},
		{WorkingDirLabel: "relative", ConfigFilesLabel: "/srv/compose.yaml"},
	} {
		if _, err := DiscoverSource(map[bool]string{true: "other", false: "games"}[labels[WorkingDirLabel] == "/x"], labels); err == nil {
			t.Errorf("%v accepted", labels)
		}
	}
}

// fakeDocker writes a docker CLI stand-in that prints the last -f file for
// `compose … config` and records its arguments.
func fakeDocker(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" >> ` + record + `
last=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-f" ]; then last="$2"; fi
  if [ "$1" = "sleep" ]; then sleep 30; fi
  shift
done
if [ -n "$last" ]; then cat "$last"; fi
`
	binary := filepath.Join(dir, "docker")
	os.WriteFile(binary, []byte(script), 0o755)
	return binary, record
}

func TestSnapshotsVetInputs(t *testing.T) {
	binary, record := fakeDocker(t)
	root := t.TempDir()
	stack := filepath.Join(root, "games")
	os.MkdirAll(stack, 0o755)
	os.WriteFile(filepath.Join(stack, "compose.yaml"), []byte(`services:
  mc:
    image: itzg/minecraft-server
    build: .
    env_file: [game.env]
`), 0o644)
	os.WriteFile(filepath.Join(stack, "game.env"), []byte("RCON_PASSWORD=secret\n"), 0o600)
	runner := &Runner{Binary: binary, Socket: "/var/run/docker.sock", Roots: root}
	snapshots := &Snapshots{Runner: runner, DB: dbtest.Open(t)}
	source := Source{Project: Project{ProjectName: "games", ProjectDirectory: stack, ComposeFiles: []string{filepath.Join(stack, "compose.yaml")}, EnvFiles: []string{}}, LoadDefaultEnv: true}
	snapshot, err := snapshots.Create(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Cleanup()
	service := snapshot.Model["services"].(map[string]any)["mc"].(map[string]any)
	if _, ok := service["build"]; ok {
		t.Fatal("build kept")
	}
	if !strings.Contains(service["labels"].(map[string]any)[SourceLabel].(string), `"projectName":"games"`) {
		t.Fatal(service["labels"])
	}
	envFile := service["env_file"].([]any)[0].(map[string]any)["path"].(string)
	if !strings.HasPrefix(envFile, snapshot.Directory) {
		t.Fatal("env file not snapshotted:", envFile)
	}
	again, _ := snapshots.Create(ctx, source)
	defer again.Cleanup()
	if again.Fingerprint != snapshot.Fingerprint || !strings.HasPrefix(snapshot.Fingerprint, "hmac-sha256:") {
		t.Fatal("fingerprint unstable")
	}
	os.WriteFile(filepath.Join(stack, "game.env"), []byte("RCON_PASSWORD=changed\n"), 0o600)
	changed, _ := snapshots.Create(ctx, source)
	defer changed.Cleanup()
	if changed.Fingerprint == snapshot.Fingerprint {
		t.Fatal("env change did not change the fingerprint")
	}
	args, _ := os.ReadFile(record)
	if strings.Contains(string(args), "secret") {
		t.Fatal("secret on the command line")
	}
	os.WriteFile(filepath.Join(stack, "compose.yaml"), []byte("services:\n  mc:\n    image: x\n    extends: base\n"), 0o644)
	if _, err := snapshots.Create(ctx, source); !apperr.Is(err, "UNSUPPORTED_COMPOSE") {
		t.Fatal(err)
	}
	if out, err := runner.Run(ctx, []string{"sleep"}, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "execution limit") {
		t.Fatal(out, err)
	}
}

func TestValidateService(t *testing.T) {
	if err := ValidateService(map[string]any{"image": "x"}); err != nil {
		t.Fatal(err)
	}
	for _, service := range []map[string]any{
		{"scale": float64(2)},
		{"deploy": map[string]any{"replicas": float64(3)}},
		{"network_mode": "service:db"},
	} {
		if err := ValidateService(service); err == nil {
			t.Errorf("%v accepted", service)
		}
	}
}
