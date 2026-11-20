package fakedocker

import (
	"net"
	"sync/atomic"

	"github.com/Crunging/Ludock/internal/netconsole"
)

// rconConnections counts the connections every fake console took, each of
// which a real Minecraft server logs twice.
var rconConnections atomic.Int64

// ServeRCON serves a Source RCON console, as Minecraft's, on 127.0.0.1 until
// the process ends. respond answers each command after the password is
// accepted. It returns the port.
func ServeRCON(password string, respond func(command string) string) (int, error) {
	return serve(password, respond, false)
}

// ServePalworldRCON is ServeRCON answering every command with ID 0, as
// Palworld does.
func ServePalworldRCON(password string, respond func(command string) string) (int, error) {
	return serve(password, respond, true)
}

func serve(password string, respond func(command string) string, zeroIDs bool) (int, error) {
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
			rconConnections.Add(1)
			go serveRCON(conn, password, respond, zeroIDs)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func serveRCON(conn net.Conn, password string, respond func(string) string, zeroIDs bool) {
	defer conn.Close()
	var buffer []byte
	authenticated := false
	chunk := make([]byte, 4096)
	for {
		n, err := conn.Read(chunk)
		if err != nil {
			return
		}
		packets, rest, err := netconsole.DecodeRCON(append(buffer, chunk[:n]...))
		if err != nil {
			return
		}
		buffer = append([]byte(nil), rest...)
		for _, packet := range packets {
			var reply []byte
			switch {
			case packet.Type == 3 && string(packet.Body) != password:
				reply, _ = netconsole.EncodeRCON(-1, 2, "")
			case packet.Type == 3:
				authenticated = true
				reply, _ = netconsole.EncodeRCON(packet.ID, 2, "")
			case !authenticated:
				return
			case zeroIDs:
				reply, _ = netconsole.EncodeRCON(0, 0, respond(string(packet.Body)))
			default:
				reply, _ = netconsole.EncodeRCON(packet.ID, 0, respond(string(packet.Body)))
			}
			conn.Write(reply)
		}
	}
}
