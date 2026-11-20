package gamequery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// a2sInfoPacket builds a Source A2S_INFO reply like a Valheim server's.
func a2sInfoPacket(players, capacity, bots byte) []byte {
	packet := append([]byte{0xff, 0xff, 0xff, 0xff, 'I', 17}, "My Valheim\x00Dedicated\x00valheim\x00Valheim\x00"...)
	packet = binary.LittleEndian.AppendUint16(packet, 0)
	packet = append(packet, players, capacity, bots, 'd', 'l', 1, 0)
	packet = append(packet, "0.217.46\x00"...)
	// Extra data flag with the game port.
	packet = append(packet, 0x80)
	return binary.LittleEndian.AppendUint16(packet, 2456)
}

func minecraftStatus(json string) []byte {
	payload := append([]byte{0x00}, appendVarInt(nil, int32(len(json)))...)
	return append(payload, json...)
}

// Realistic replies parse, and malformed ones are no answer rather than a
// guess.
func TestParseA2S(t *testing.T) {
	valid := a2sInfoPacket(3, 10, 0)
	cases := []struct {
		name      string
		packet    []byte
		ok        bool
		online    int
		max       int
		challenge bool
	}{
		{"valheim", valid, true, 3, 10, false},
		{"bots are not players", a2sInfoPacket(5, 10, 2), true, 3, 10, false},
		{"more bots than players", a2sInfoPacket(1, 10, 4), true, 0, 10, false},
		{"empty server", a2sInfoPacket(0, 0, 0), true, 0, 0, false},
		{"no extra data", valid[:bytes.Index(valid, []byte("0.217.46\x00"))+9], true, 3, 10, false},
		{"no version", valid[:bytes.Index(valid, []byte("0.217"))], false, 0, 0, false},
		{"challenge", []byte{0xff, 0xff, 0xff, 0xff, 'A', 1, 2, 3, 4}, false, 0, 0, true},
		{"short challenge", []byte{0xff, 0xff, 0xff, 0xff, 'A', 1, 2, 3}, false, 0, 0, false},
		{"long challenge", []byte{0xff, 0xff, 0xff, 0xff, 'A', 1, 2, 3, 4, 5}, false, 0, 0, false},
		{"split packet", append([]byte{0xfe, 0xff, 0xff, 0xff}, valid[4:]...), false, 0, 0, false},
		{"goldsource", append([]byte{0xff, 0xff, 0xff, 0xff, 'm'}, valid[5:]...), false, 0, 0, false},
		{"player list", append([]byte{0xff, 0xff, 0xff, 0xff, 'D'}, valid[5:]...), false, 0, 0, false},
		{"empty", nil, false, 0, 0, false},
		{"header only", []byte{0xff, 0xff, 0xff, 0xff}, false, 0, 0, false},
		{"type only", []byte{0xff, 0xff, 0xff, 0xff, 'I'}, false, 0, 0, false},
		{"unterminated name", append([]byte{0xff, 0xff, 0xff, 0xff, 'I', 17}, "My Valheim"...), false, 0, 0, false},
		{"truncated counts", valid[:bytes.Index(valid, []byte("Valheim\x00"))+10], false, 0, 0, false},
		{"bad server type", bytes.Replace(valid, []byte{'d', 'l', 1, 0}, []byte{'x', 'l', 1, 0}, 1), false, 0, 0, false},
		{"bad environment", bytes.Replace(valid, []byte{'d', 'l', 1, 0}, []byte{'d', 'z', 1, 0}, 1), false, 0, 0, false},
		{"bad visibility", bytes.Replace(valid, []byte{'d', 'l', 1, 0}, []byte{'d', 'l', 7, 0}, 1), false, 0, 0, false},
		{"bad vac", bytes.Replace(valid, []byte{'d', 'l', 1, 0}, []byte{'d', 'l', 1, 9}, 1), false, 0, 0, false},
		{"oversized", append(valid, make([]byte, maxA2SPacket)...), false, 0, 0, false},
	}
	for _, c := range cases {
		reply := ParseA2S(c.packet)
		if reply.OK != c.ok || (reply.Challenge != nil) != c.challenge {
			t.Errorf("%s: got %+v", c.name, reply)
			continue
		}
		if c.ok && (!reply.Status.Counted || reply.Status.Online != c.online || reply.Status.Max != c.max) {
			t.Errorf("%s: got %+v", c.name, reply.Status)
		}
	}
}

// Status responses parse when they are well formed and fill their packet;
// anything else is no answer.
func TestParseMinecraftStatus(t *testing.T) {
	vanilla := `{"version":{"name":"1.21.4","protocol":769},"players":{"max":20,"online":2,"sample":[{"name":"Mara","id":"4566e69f-c907-48ee-8d71-d7ba5aa00d20"}]},"description":{"text":"Hello"},"enforcesSecureChat":true}`
	cases := []struct {
		name    string
		payload []byte
		ok      bool
		counted bool
		online  int
		max     int
	}{
		{"vanilla", minecraftStatus(vanilla), true, true, 2, 20},
		{"legacy description", minecraftStatus(`{"description":"§aHi","players":{"max":10,"online":0},"version":{"name":"1.8","protocol":47}}`), true, true, 0, 10},
		{"no players", minecraftStatus(`{"version":{"name":"Paper","protocol":769},"description":"Hi"}`), true, false, 0, 0},
		{"null players", minecraftStatus(`{"players":null}`), true, false, 0, 0},
		{"missing online", minecraftStatus(`{"players":{"max":20}}`), false, false, 0, 0},
		{"fractional count", minecraftStatus(`{"players":{"max":20,"online":1.5}}`), false, false, 0, 0},
		{"negative count", minecraftStatus(`{"players":{"max":20,"online":-1}}`), false, false, 0, 0},
		{"huge count", minecraftStatus(`{"players":{"max":20,"online":99999999999}}`), false, false, 0, 0},
		{"string count", minecraftStatus(`{"players":{"max":"20","online":"1"}}`), false, false, 0, 0},
		{"players not an object", minecraftStatus(`{"players":[1,2]}`), false, false, 0, 0},
		{"array", minecraftStatus(`[{"players":{"max":1,"online":1}}]`), false, false, 0, 0},
		{"null", minecraftStatus(`null`), false, false, 0, 0},
		{"broken json", minecraftStatus(`{"players":{"max":20,`), false, false, 0, 0},
		{"invalid utf-8", minecraftStatus("{\"description\":\"\xff\xfe\"}"), false, false, 0, 0},
		{"wrong packet id", append([]byte{0x01}, minecraftStatus(vanilla)[1:]...), false, false, 0, 0},
		{"string longer than packet", append([]byte{0x00, 0xff, 0x01}, `{"players":{}}`...), false, false, 0, 0},
		{"trailing bytes", append(minecraftStatus(vanilla), 0), false, false, 0, 0},
		{"overlong varint", append([]byte{0x00, 0xff, 0xff, 0xff, 0xff, 0x7f}, `{}`...), false, false, 0, 0},
		{"empty", nil, false, false, 0, 0},
	}
	for _, c := range cases {
		status, ok := ParseMinecraftStatus(c.payload)
		if ok != c.ok || status.Counted != c.counted || status.Online != c.online || status.Max != c.max {
			t.Errorf("%s: got %+v, %v", c.name, status, ok)
		}
	}
}

// A player sample lists everyone online only when it holds as many valid
// entries as are online; a partial sample, a hidden player, or a sample of
// text lines lists nobody, and a server with nobody online lists them all.
func TestMinecraftSampleLists(t *testing.T) {
	entry := func(name, id string) string { return `{"name":"` + name + `","id":"` + id + `"}` }
	mara, theo := entry("Mara", "4566e69f-c907-48ee-8d71-d7ba5aa00d20"), entry("Theo", "5566e69f-c907-48ee-8d71-d7ba5aa00d21")
	status := func(online int, sample ...string) []byte {
		return minecraftStatus(fmt.Sprintf(`{"players":{"max":20,"online":%d,"sample":[%s]}}`, online, strings.Join(sample, ",")))
	}
	cases := []struct {
		name    string
		payload []byte
		listed  []string
	}{
		{"everyone", status(2, theo, mara), []string{"Theo", "Mara"}},
		{"nobody", status(0), []string{}},
		{"partial", status(3, mara, theo), nil},
		{"hidden player", status(2, mara, entry("Anonymous Player", "00000000-0000-0000-0000-000000000000")), nil},
		{"text line", status(2, mara, entry("§aWelcome!", "not-a-uuid")), nil},
		{"empty name", status(1, entry("", "4566e69f-c907-48ee-8d71-d7ba5aa00d20")), nil},
		{"control character", status(1, entry(`Ma\nra`, "4566e69f-c907-48ee-8d71-d7ba5aa00d20")), nil},
		{"no sample", minecraftStatus(`{"players":{"max":20,"online":1}}`), nil},
		{"sample in another shape", minecraftStatus(`{"players":{"max":20,"online":1,"sample":[{"name":"Mara","id":5}]}}`), nil},
		{"sample not a list", minecraftStatus(`{"players":{"max":20,"online":1,"sample":"Mara"}}`), nil},
	}
	for _, c := range cases {
		got, ok := ParseMinecraftStatus(c.payload)
		if !ok || got.Listed != (c.listed != nil) || (c.listed != nil && !got.Lists(c.listed)) {
			t.Errorf("%s: got %+v, %v", c.name, got, ok)
		}
	}
	listed, _ := ParseMinecraftStatus(status(2, mara, theo))
	for _, other := range [][]string{{"Mara"}, {"Mara", "Mara"}, {"Mara", "Ada"}, {"Mara", "Theo", "Ada"}} {
		if listed.Lists(other) {
			t.Errorf("listed %v as %v", listed.Names, other)
		}
	}
	if (Status{Counted: true, Online: 0}).Lists(nil) {
		t.Error("a status that doesn't say it lists everyone listed nobody")
	}
}

// VarInts follow the protocol's encoding, including negative values, and
// refuse more than five bytes.
func TestVarInt(t *testing.T) {
	for _, value := range []int32{0, 1, 127, 128, 255, 25565, 2097151, 2147483647, -1, -2147483648} {
		encoded := appendVarInt(nil, value)
		decoded, err := readVarInt(bytes.NewReader(encoded))
		if err != nil || decoded != value || len(encoded) > 5 {
			t.Errorf("%d: encoded %x, decoded %d, %v", value, encoded, decoded, err)
		}
	}
	if !bytes.Equal(appendVarInt(nil, -1), []byte{0xff, 0xff, 0xff, 0xff, 0x0f}) {
		t.Error("-1 is not encoded as the protocol documents")
	}
	for _, bad := range [][]byte{{0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, {0xff, 0xff, 0xff, 0xff, 0x1f}, {0x80}} {
		if _, err := readVarInt(bytes.NewReader(bad)); err == nil {
			t.Errorf("%x was accepted", bad)
		}
	}
}

func FuzzParseA2S(f *testing.F) {
	f.Add(a2sInfoPacket(3, 10, 1))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 'A', 1, 2, 3, 4})
	f.Fuzz(func(t *testing.T, packet []byte) {
		reply := ParseA2S(packet)
		if reply.OK && (reply.Status.Online < 0 || reply.Status.Online > maxCount || reply.Status.Max > maxCount) {
			t.Errorf("out of bounds: %+v", reply.Status)
		}
	})
}

func FuzzParseMinecraftStatus(f *testing.F) {
	f.Add(minecraftStatus(`{"players":{"max":20,"online":2}}`))
	f.Add([]byte{0x00, 0x02, '{', '}'})
	f.Fuzz(func(t *testing.T, payload []byte) {
		status, ok := ParseMinecraftStatus(payload)
		if ok && (status.Online < 0 || status.Online > maxCount || status.Max < 0 || status.Max > maxCount) {
			t.Errorf("out of bounds: %+v", status)
		}
	})
}

// serveA2S answers on a local UDP port with respond's datagrams, and
// returns the port and the requests it received.
func serveA2S(t *testing.T, respond func(request []byte) [][]byte) (int, func() [][]byte) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var mu sync.Mutex
	var requests [][]byte
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			request := append([]byte(nil), buffer[:n]...)
			mu.Lock()
			requests = append(requests, request)
			mu.Unlock()
			for _, reply := range respond(request) {
				conn.WriteTo(reply, from)
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

// A server that asks for a challenge gets the request again with it, and
// one that keeps asking, sends a split reply, or stays silent is no answer.
func TestA2SQuery(t *testing.T) {
	t.Parallel()
	challenge := []byte{0xff, 0xff, 0xff, 0xff, 'A', 9, 8, 7, 6}
	port, requests := serveA2S(t, func(request []byte) [][]byte {
		if bytes.HasSuffix(request, []byte{9, 8, 7, 6}) {
			return [][]byte{a2sInfoPacket(2, 16, 0)}
		}
		return [][]byte{challenge}
	})
	status, err := Query(context.Background(), ValveA2S, "127.0.0.1", port, nil)
	if err != nil || status.Online != 2 || status.Max != 16 {
		t.Fatalf("challenged query: %+v, %v", status, err)
	}
	if got := requests(); len(got) != 2 || !bytes.Equal(got[1], A2SRequest([]byte{9, 8, 7, 6})) {
		t.Fatalf("requests: %x", got)
	}

	endless, _ := serveA2S(t, func([]byte) [][]byte { return [][]byte{challenge} })
	split, _ := serveA2S(t, func([]byte) [][]byte {
		return [][]byte{append([]byte{0xfe, 0xff, 0xff, 0xff, 1, 0, 0, 0, 2, 0}, a2sInfoPacket(1, 2, 0)...)}
	})
	silent, _ := serveA2S(t, func([]byte) [][]byte { return nil })
	for name, port := range map[string]int{"endless challenges": endless, "split": split, "silent": silent} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			started := time.Now()
			if _, err := Query(context.Background(), ValveA2S, "127.0.0.1", port, nil); !errors.Is(err, ErrNoAnswer) {
				t.Error(err)
			}
			if time.Since(started) > Timeout+time.Second {
				t.Errorf("took %s", time.Since(started))
			}
		})
	}
}

// A Minecraft server gets a status handshake and request, and a reply cut
// short, oversized, or never sent is no answer.
func TestMinecraftQuery(t *testing.T) {
	t.Parallel()
	serve := func(t *testing.T, reply []byte, keepOpen bool) (int, chan []byte) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		received := make(chan []byte, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			request := make([]byte, 256)
			n, _ := conn.Read(request)
			received <- request[:n]
			conn.Write(reply)
			if keepOpen {
				time.Sleep(Timeout + time.Second)
			}
		}()
		return listener.Addr().(*net.TCPAddr).Port, received
	}
	port, received := serve(t, MinecraftPacket(minecraftStatus(`{"players":{"max":10,"online":4}}`)), false)
	status, err := Query(context.Background(), MinecraftPing, "127.0.0.1", port, nil)
	if err != nil || status.Online != 4 || status.Max != 10 {
		t.Fatalf("query: %+v, %v", status, err)
	}
	if request := <-received; !bytes.Equal(request, MinecraftRequest("127.0.0.1", port)) {
		t.Fatalf("request: %x", request)
	}

	full := MinecraftPacket(minecraftStatus(`{"players":{"max":10,"online":4}}`))
	huge := appendVarInt(nil, maxMinecraftPacket+1)
	for name, reply := range map[string][]byte{"truncated": full[:len(full)-3], "oversized": huge, "nothing": nil,
		"garbage": []byte(strings.Repeat("\xff", 8))} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			port, _ := serve(t, reply, name == "nothing")
			if _, err := Query(context.Background(), MinecraftPing, "127.0.0.1", port, nil); !errors.Is(err, ErrNoAnswer) {
				t.Error(err)
			}
		})
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	unused := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if _, err := Query(context.Background(), MinecraftPing, "127.0.0.1", unused, nil); !errors.Is(err, ErrNoAnswer) {
		t.Errorf("closed port: %v", err)
	}
}
