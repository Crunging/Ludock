package fakedocker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Crunging/Ludock/internal/gamequery"
)

// GameStatus is what a fake game says about itself; ok false keeps it
// silent, as a hung game is.
type GameStatus func() (online, capacity int, ok bool)

// ServeMinecraftStatus answers Minecraft's Server List Ping on 127.0.0.1
// until the process ends, and returns the port.
func ServeMinecraftStatus(status GameStatus) (int, error) {
	return ServeMinecraftPlayers(func() ([]string, int, int, bool) {
		online, capacity, ok := status()
		return nil, online, capacity, ok
	})
}

// ServeMinecraftPlayers is ServeMinecraftStatus with a player sample, as
// the real game sends: names, each with a UUID derived from it.
func ServeMinecraftPlayers(status func() (sample []string, online, capacity int, ok bool)) (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(gamequery.Timeout + time.Second))
				// The handshake and status request, whose exact bytes don't matter.
				if _, err := conn.Read(make([]byte, 512)); err != nil {
					return
				}
				sample, online, capacity, ok := status()
				if !ok {
					io.Copy(io.Discard, conn)
					return
				}
				players := map[string]any{"max": capacity, "online": online}
				if len(sample) > 0 {
					entries := []map[string]string{}
					for _, name := range sample {
						entries = append(entries, map[string]string{"name": name, "id": fmt.Sprintf("00000000-0000-4000-8000-%012x", len(name))})
					}
					players["sample"] = entries
				}
				encoded, _ := json.Marshal(map[string]any{"version": map[string]any{"name": "1.21.4", "protocol": 769}, "players": players,
					"description": map[string]string{"text": "Fake"}})
				payload := append([]byte{0x00}, binary.AppendUvarint(nil, uint64(len(encoded)))...)
				conn.Write(gamequery.MinecraftPacket(append(payload, encoded...)))
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// ServeA2S answers Valve's A2S_INFO on 127.0.0.1 until the process ends,
// asking for a challenge first as current Source servers do, and returns
// the port.
func ServeA2S(name string, status GameStatus) (int, error) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	challenge := []byte{0x4c, 0x55, 0x44, 0x4b}
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			online, capacity, ok := status()
			switch {
			case !ok:
			case bytes.Equal(buffer[:n], gamequery.A2SRequest(challenge)):
				reply := append([]byte{0xff, 0xff, 0xff, 0xff, 'I', 17}, name+"\x00Fake map\x00fake\x00"+name+"\x00"...)
				reply = binary.LittleEndian.AppendUint16(reply, 0)
				reply = append(reply, byte(online), byte(capacity), 0, 'd', 'l', 0, 0)
				conn.WriteTo(append(reply, "1.0\x00"...), from)
			case bytes.Equal(buffer[:n], gamequery.A2SRequest(nil)):
				conn.WriteTo(append([]byte{0xff, 0xff, 0xff, 0xff, 'A'}, challenge...), from)
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port, nil
}
