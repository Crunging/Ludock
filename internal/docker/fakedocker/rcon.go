package fakedocker

import (
	"net"

	"github.com/Crunging/Ludock/internal/netconsole"
)

// ServeRCON serves a Source RCON console, as Minecraft's, on 127.0.0.1 until
// the process ends. respond answers each command after the password is
// accepted. It returns the port.
func ServeRCON(password string, respond func(command string) string) (int, error) {
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
			go serveRCON(conn, password, respond)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func serveRCON(conn net.Conn, password string, respond func(string) string) {
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
			default:
				reply, _ = netconsole.EncodeRCON(packet.ID, 0, respond(string(packet.Body)))
			}
			conn.Write(reply)
		}
	}
}
