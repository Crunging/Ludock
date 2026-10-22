package games

import "testing"

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
