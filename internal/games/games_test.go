package games

import (
	"strings"
	"testing"
)

func TestParseAddress(t *testing.T) {
	for input, want := range map[string]Address{
		" Play.Example.com ": {"play.example.com", 0},
		"203.0.113.10:30000": {"203.0.113.10", 30000},
		"[2001:db8::1]:2456": {"2001:db8::1", 2456},
		"[2001:db8::1]":      {"2001:db8::1", 0},
		"2001:db8::1":        {"2001:db8::1", 0},
	} {
		if got := ParseAddress(input); got == nil || *got != want {
			t.Errorf("%q: %+v", input, got)
		}
	}
	for _, invalid := range []string{"", "https://play.example.com", "play.example.com:0", "play.example.com:65536",
		"play.example.com:25565/udp", "[play.example.com]:25565", "two words", "host:port:1"} {
		if got := ParseAddress(invalid); got != nil {
			t.Errorf("%q: %+v", invalid, got)
		}
	}
}

func TestEligibility(t *testing.T) {
	for _, c := range []struct {
		image  string
		labels map[string]string
		reason string
	}{
		{"itzg/minecraft-server:java21", nil, ReasonRecognizedImage},
		{"ghcr.io/itzg/minecraft-server@sha256:abc", nil, ReasonRecognizedImage},
		{"example/custom", nil, ReasonUnrecognizedImage},
		{"example/custom", map[string]string{LabelEnable: " TRUE "}, ReasonExplicitlyEnabled},
		{"itzg/minecraft-server", map[string]string{LabelEnable: "false"}, ReasonOptedOut},
		{"itzg/minecraft-server", map[string]string{LabelEnable: "yes"}, ReasonInvalidEnableLabel},
		{"itzg/minecraft-server", map[string]string{"com.docker.compose.oneoff": "True"}, ReasonComposeOneoff},
	} {
		if got := Evaluate(c.image, c.labels); got.Reason != c.reason {
			t.Errorf("%s %v: %s", c.image, c.labels, got.Reason)
		}
	}
	if NormalizeImageRepository("localhost:5000/Itzg/Minecraft-Server:latest") != "itzg/minecraft-server" {
		t.Fatal(NormalizeImageRepository("localhost:5000/Itzg/Minecraft-Server:latest"))
	}
}

func TestConsoleAdapters(t *testing.T) {
	if adapter := ResolveAdapter("ark", nil); adapter == nil || adapter.ID != SourceRCON || adapter.DefaultPort != 27020 || adapter.Name != "ARK RCON" {
		t.Fatalf("%+v", adapter)
	}
	if BroadcastTemplate("ark", nil) != "Broadcast {message}" {
		t.Fatal("ark broadcast")
	}
	// A transport override alone cannot establish broadcast syntax.
	if BroadcastTemplate("ark", map[string]string{LabelConsole: "telnet-console"}) != "" {
		t.Fatal("override kept broadcast")
	}
	if ResolveAdapter("minecraft", map[string]string{LabelConsole: "Disabled"}) != nil {
		t.Fatal("disabled console resolved")
	}
	if adapter := ResolveAdapter("valheim", nil); adapter != nil {
		t.Fatal("valheim has no console")
	}
}

func TestComposeIdentity(t *testing.T) {
	complete := map[string]string{"com.docker.compose.project": "games", "com.docker.compose.service": "mc", "com.docker.compose.container-number": "1"}
	if InvalidComposeIdentity(complete) {
		t.Fatal("complete identity rejected")
	}
	for _, change := range []func(map[string]string){
		func(l map[string]string) { delete(l, "com.docker.compose.service") },
		func(l map[string]string) { l["com.docker.compose.container-number"] = "01" },
		func(l map[string]string) { l["com.docker.compose.project"] = "a\nb" },
	} {
		labels := map[string]string{}
		for k, v := range complete {
			labels[k] = v
		}
		change(labels)
		if !InvalidComposeIdentity(labels) {
			t.Errorf("accepted %v", labels)
		}
	}
}

// The list command's wording differs across Minecraft versions and plugins;
// anything else is not a player list.
func TestParseMinecraftList(t *testing.T) {
	for response, want := range map[string]Players{
		"There are 0 of a max of 20 players online: ":           {0, 20, nil},
		"There are 2 of a max of 20 players online: Mara, Theo": {2, 20, []string{"Mara", "Theo"}},
		"There are 1/10 players online:Steve":                   {1, 10, []string{"Steve"}},
		"There are 2/10 players online:\nAlex, Steve\n":         {2, 10, []string{"Alex", "Steve"}},
		"§6There are §c3§6 out of maximum §c50§6 players online.\n§6Admins§r: §fMara\n§6Default§r: Theo, Juno": {3, 50,
			[]string{"Mara", "Theo", "Juno"}},
		// Some servers hide names; the count still stands.
		"There are 4 of a max of 8 players online:": {4, 8, nil},
	} {
		got, ok := ParseMinecraftList(response)
		if !ok || got.Online != want.Online || got.Max != want.Max || strings.Join(got.Names, "|") != strings.Join(want.Names, "|") {
			t.Errorf("%q: %+v %v", response, got, ok)
		}
	}
	for _, response := range []string{"", "Unknown command", "Command completed with no response", "There are many players online"} {
		if got, ok := ParseMinecraftList(response); ok {
			t.Errorf("%q parsed as %+v", response, got)
		}
	}
	long, _ := ParseMinecraftList("There are 300 of a max of 500 players online: " + strings.Repeat(strings.Repeat("x", 80)+", ", 300))
	if len(long.Names) != maxPlayerNames || len([]rune(long.Names[0])) != maxNameRunes {
		t.Fatalf("%d names, first %d runes", len(long.Names), len([]rune(long.Names[0])))
	}
}
