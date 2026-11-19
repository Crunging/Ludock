package gamequery

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"unicode/utf8"
)

// maxMinecraftPacket bounds a status response. Vanilla caps its JSON at
// 32767 UTF-16 units, at most three bytes each; mod lists can add more.
const maxMinecraftPacket = 256 * 1024

// minecraft runs a Server List Ping (Java Edition 1.7 and later): a
// handshake for the status state, then a status request. Older servers,
// which only take the legacy 0xFE ping, don't answer it.
func minecraft(ctx context.Context, host string, port int, authorize func() error) (Status, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Status{}, ErrNoAnswer
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := authorize(); err != nil {
		return Status{}, err
	}
	if _, err := conn.Write(MinecraftRequest(host, port)); err != nil {
		return Status{}, ErrNoAnswer
	}
	payload, err := readMinecraftPacket(bufio.NewReader(conn))
	if err != nil {
		return Status{}, ErrNoAnswer
	}
	status, ok := ParseMinecraftStatus(payload)
	if !ok {
		return Status{}, ErrNoAnswer
	}
	return status, nil
}

// MinecraftRequest is a handshake for the status state followed by a status
// request. Protocol version -1 is the convention for a ping that doesn't
// know the server's version.
func MinecraftRequest(host string, port int) []byte {
	var handshake []byte
	handshake = appendVarInt(handshake, 0x00)
	handshake = appendVarInt(handshake, -1)
	handshake = appendVarInt(handshake, int32(len(host)))
	handshake = append(handshake, host...)
	handshake = binary.BigEndian.AppendUint16(handshake, uint16(port))
	handshake = appendVarInt(handshake, 1)
	request := appendVarInt(nil, int32(len(handshake)))
	request = append(request, handshake...)
	return append(request, 0x01, 0x00)
}

// MinecraftPacket frames a packet payload with its length.
func MinecraftPacket(payload []byte) []byte {
	return append(appendVarInt(nil, int32(len(payload))), payload...)
}

func appendVarInt(data []byte, value int32) []byte {
	unsigned := uint32(value)
	for unsigned >= 0x80 {
		data = append(data, byte(unsigned)|0x80)
		unsigned >>= 7
	}
	return append(data, byte(unsigned))
}

var errMalformed = errors.New("malformed status reply")

// readVarInt reads a VarInt of at most five bytes.
func readVarInt(reader io.ByteReader) (int32, error) {
	var value uint32
	for index := range 5 {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		if index == 4 && b > 0x0f {
			return 0, errMalformed
		}
		value |= uint32(b&0x7f) << (7 * index)
		if b&0x80 == 0 {
			return int32(value), nil
		}
	}
	return 0, errMalformed
}

func readMinecraftPacket(reader *bufio.Reader) ([]byte, error) {
	length, err := readVarInt(reader)
	if err != nil {
		return nil, err
	}
	if length < 1 || length > maxMinecraftPacket {
		return nil, errMalformed
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// ParseMinecraftStatus reads a status response packet's payload: packet ID
// 0, then the status JSON as a length-prefixed UTF-8 string that fills the
// rest of the packet.
func ParseMinecraftStatus(payload []byte) (Status, bool) {
	reader := bytes.NewReader(payload)
	id, err := readVarInt(reader)
	if err != nil || id != 0x00 {
		return Status{}, false
	}
	length, err := readVarInt(reader)
	if err != nil || length < 2 || int(length) != reader.Len() {
		return Status{}, false
	}
	text := payload[len(payload)-int(length):]
	if !utf8.Valid(text) || text[0] != '{' {
		return Status{}, false
	}
	var reply struct {
		Players *struct {
			Online *int64 `json:"online"`
			Max    *int64 `json:"max"`
		} `json:"players"`
	}
	if json.Unmarshal(text, &reply) != nil {
		return Status{}, false
	}
	// A server may leave out its players; it still answered.
	if reply.Players == nil {
		return Status{}, true
	}
	if reply.Players.Online == nil || reply.Players.Max == nil {
		return Status{}, false
	}
	return counts(*reply.Players.Online, *reply.Players.Max)
}
