// Package netconsole speaks the game console protocols Ludock supports over
// the network: Source RCON, Rust WebRCON, and Telnet. The same code runs in
// the backend and inside the network console helper.
package netconsole

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// Limits.
const (
	ConnectTimeout   = 5 * time.Second
	CommandTimeout   = 10 * time.Second
	MaxBytes         = 4 * 1024 * 1024
	maxPacket        = MaxBytes
	maxTCPReceived   = maxPacket + 64*1024
	responseQuiet    = 150 * time.Millisecond
	telnetQuiet      = 250 * time.Millisecond
	telnetCommandGap = 50 * time.Millisecond
)

// Control lets a caller reauthorize before each credential or command
// write, and cancel the command.
type Control struct {
	// Authorize, when set, is asked again before every network write.
	Authorize func() error
	// AssertAccess runs before writes as well.
	AssertAccess func() error
}

func (c Control) check() error {
	if c.Authorize != nil {
		if err := c.Authorize(); err != nil {
			return errors.New("Console access changed")
		}
	}
	if c.AssertAccess != nil {
		if err := c.AssertAccess(); err != nil {
			return errors.New("Console access changed")
		}
	}
	return nil
}

// Execute runs one command with the named transport.
func Execute(ctx context.Context, transport, host string, port int, password, command string, control Control) (string, error) {
	switch transport {
	case "source-rcon":
		return SourceRCON(ctx, host, port, password, command, control)
	case "rust-webrcon":
		return RustWebRCON(ctx, host, port, password, command, control)
	case "telnet":
		return Telnet(ctx, host, port, password, command, control)
	}
	return "", errors.New("Unsupported console transport")
}

func requestID() int32 {
	n, _ := rand.Int(rand.Reader, big.NewInt(2_000_000_000))
	return int32(n.Int64()) + 1
}

type session struct {
	conn     net.Conn
	control  Control
	mu       sync.Mutex
	received int
	limitErr string
	connErr  string
}

func dial(ctx context.Context, host string, port int, timeoutMessage, connectionMessage string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: ConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Console command cancelled")
		}
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, errors.New(timeoutMessage)
		}
		return nil, errors.New(connectionMessage)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	return conn, nil
}

func (s *session) write(data []byte) error {
	if len(data) > maxPacket+4 {
		return errors.New("Console command or credential is too large")
	}
	if err := s.control.check(); err != nil {
		return err
	}
	if _, err := s.conn.Write(data); err != nil {
		return errors.New(s.connErr)
	}
	return nil
}

// EncodeRCON frames one Source RCON packet.
func EncodeRCON(id, kind int32, body string) ([]byte, error) {
	if len(body) > maxPacket-10 {
		return nil, errors.New("RCON command or credential is too large")
	}
	packet := make([]byte, len(body)+14)
	binary.LittleEndian.PutUint32(packet[0:], uint32(len(body)+10))
	binary.LittleEndian.PutUint32(packet[4:], uint32(id))
	binary.LittleEndian.PutUint32(packet[8:], uint32(kind))
	copy(packet[12:], body)
	return packet, nil
}

// RCONPacket is one decoded packet.
type RCONPacket struct {
	ID, Type int32
	Body     []byte
}

// DecodeRCON splits complete packets from buffer and returns the rest.
func DecodeRCON(buffer []byte) ([]RCONPacket, []byte, error) {
	var packets []RCONPacket
	offset := 0
	for len(buffer)-offset >= 4 {
		size := int(int32(binary.LittleEndian.Uint32(buffer[offset:])))
		if size < 10 || size > maxPacket {
			return nil, nil, errors.New("The RCON server returned an invalid packet")
		}
		if len(buffer)-offset < size+4 {
			break
		}
		end := offset + size + 4
		packets = append(packets, RCONPacket{ID: int32(binary.LittleEndian.Uint32(buffer[offset+4:])),
			Type: int32(binary.LittleEndian.Uint32(buffer[offset+8:])), Body: buffer[offset+12 : end-2]})
		offset = end
	}
	return packets, buffer[offset:], nil
}

type readResult struct {
	data []byte
	err  error
}

func reader(conn net.Conn, limit int, limitMessage string) <-chan readResult {
	results := make(chan readResult, 16)
	go func() {
		defer close(results)
		buffer := make([]byte, 64*1024)
		total := 0
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				total += n
				if total > limit {
					results <- readResult{err: errors.New(limitMessage)}
					return
				}
				results <- readResult{data: append([]byte(nil), buffer[:n]...)}
			}
			if err != nil {
				results <- readResult{err: err}
				return
			}
		}
	}()
	return results
}

// SourceRCON runs a Source RCON command.
func SourceRCON(ctx context.Context, host string, port int, password, command string, control Control) (string, error) {
	if err := control.check(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	conn, err := dial(ctx, host, port, "RCON connection timed out", "Could not connect to the server's RCON endpoint")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	s := &session{conn: conn, control: control, connErr: "Could not connect to the server's RCON endpoint"}
	authID, commandID := requestID(), requestID()
	auth, err := EncodeRCON(authID, 3, password)
	if err != nil {
		return "", err
	}
	if err := s.write(auth); err != nil {
		return "", err
	}
	results := reader(conn, maxTCPReceived, "RCON response is too large")
	var buffer []byte
	authenticated, commandSent, received := false, false, false
	var response bytes.Buffer
	quiet := time.NewTimer(time.Hour)
	quiet.Stop()
	fail := func() error {
		if ctx.Err() == context.DeadlineExceeded {
			return errors.New("RCON connection timed out")
		}
		if ctx.Err() != nil {
			return errors.New("Console command cancelled")
		}
		return nil
	}
	for {
		select {
		case <-quiet.C:
			return decodeText(response.Bytes()), nil
		case <-ctx.Done():
			return "", fail()
		case result, ok := <-results:
			if !ok || result.err != nil {
				if err := fail(); err != nil {
					return "", err
				}
				if result.err != nil && result.err.Error() == "RCON response is too large" {
					return "", result.err
				}
				switch {
				case !authenticated:
					return "", errors.New("RCON authentication did not complete")
				case !commandSent:
					return "", errors.New("RCON connection closed before the command was sent")
				case len(buffer) > 0:
					return "", errors.New("The RCON server returned an incomplete packet")
				}
				return decodeText(response.Bytes()), nil
			}
			buffer = append(buffer, result.data...)
			packets, rest, err := DecodeRCON(buffer)
			if err != nil {
				return "", err
			}
			buffer = append([]byte(nil), rest...)
			for _, packet := range packets {
				if !authenticated {
					if packet.ID == -1 {
						return "", errors.New("RCON authentication failed")
					}
					if packet.ID == authID && packet.Type == 2 {
						authenticated = true
						frame, err := EncodeRCON(commandID, 2, command)
						if err != nil {
							return "", err
						}
						if err := s.write(frame); err != nil {
							return "", err
						}
						commandSent = true
					}
					continue
				}
				if !commandSent || packet.ID != commandID {
					continue
				}
				received = true
				if response.Len()+len(packet.Body) > maxPacket {
					return "", errors.New("RCON response is too large")
				}
				response.Write(packet.Body)
			}
			quiet.Stop()
			if received && len(buffer) == 0 {
				quiet.Reset(responseQuiet)
			}
		}
	}
}

func decodeText(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return strings.ToValidUTF8(string(data), "�")
}

func formatHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// RustWebRCON runs a Rust WebRCON command. The password is part of the URL,
// so connection errors are never passed on.
func RustWebRCON(ctx context.Context, host string, port int, password, command string, control Control) (string, error) {
	if err := control.check(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	handshake, cancelHandshake := context.WithTimeout(ctx, ConnectTimeout)
	defer cancelHandshake()
	address := "ws://" + formatHost(host) + ":" + strconv.Itoa(port) + "/" + url.PathEscape(password)
	conn, _, err := websocket.Dial(handshake, address, nil)
	if err != nil {
		if ctx.Err() != nil && ctx.Err() != context.DeadlineExceeded {
			return "", errors.New("Console command cancelled")
		}
		if handshake.Err() == context.DeadlineExceeded {
			return "", errors.New("WebRCON connection timed out")
		}
		return "", errors.New("Could not connect or authenticate with Rust WebRCON")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxPacket)
	if err := control.check(); err != nil {
		return "", err
	}
	identifier := requestID()
	message, _ := json.Marshal(map[string]any{"Identifier": identifier, "Message": command, "Name": "Ludock"})
	if err := conn.Write(ctx, websocket.MessageText, message); err != nil {
		return "", errors.New("Could not connect or authenticate with Rust WebRCON")
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "", errors.New("WebRCON command timed out")
			}
			if ctx.Err() != nil {
				return "", errors.New("Console command cancelled")
			}
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) {
				return "", errors.New("WebRCON connection closed before the command response")
			}
			if strings.Contains(err.Error(), "read limited") {
				return "", errors.New("WebRCON response is too large")
			}
			return "", errors.New("WebRCON connection closed before the command response")
		}
		var reply struct {
			Identifier json.RawMessage `json:"Identifier"`
			Message    json.RawMessage `json:"Message"`
		}
		if json.Unmarshal(data, &reply) != nil {
			return "", errors.New("The WebRCON server returned an invalid response")
		}
		var id float64
		if json.Unmarshal(reply.Identifier, &id) != nil || int32(id) != identifier || id != float64(identifier) {
			continue
		}
		var text string
		if json.Unmarshal(reply.Message, &text) == nil {
			return text, nil
		}
		if len(reply.Message) == 0 || string(reply.Message) == "null" {
			return `""`, nil
		}
		return string(reply.Message), nil
	}
}

var (
	passwordPrompt = regexp.MustCompile(`(?i)password\s*[:>]?`)
	authFailed     = regexp.MustCompile(`(?i)incorrect|invalid password|authentication failed`)
	beforePassword = regexp.MustCompile(`(?is).*password\s*[:>]?\s*`)
)

type telnetDecoder struct {
	state   int
	command byte
}

// decode strips Telnet commands and returns refusal replies for options.
func (d *telnetDecoder) decode(chunk []byte) ([]byte, []byte) {
	var output, replies []byte
	for _, b := range chunk {
		switch d.state {
		case 0:
			if b == 255 {
				d.state = 1
			} else {
				output = append(output, b)
			}
		case 1:
			switch {
			case b == 255:
				output = append(output, b)
				d.state = 0
			case b >= 251 && b <= 254:
				d.command, d.state = b, 2
			case b == 250:
				d.state = 3
			default:
				d.state = 0
			}
		case 2:
			reply := byte(252)
			if d.command <= 252 {
				reply = 254
			}
			replies = append(replies, 255, reply, b)
			d.state = 0
		case 3:
			if b == 255 {
				d.state = 4
			}
		case 4:
			if b == 240 {
				d.state = 0
			} else {
				d.state = 3
			}
		}
	}
	return output, replies
}

// CleanTelnet removes the prompt and the echoed command.
func CleanTelnet(output, command string) string {
	output = beforePassword.ReplaceAllString(output, "")
	echo := regexp.MustCompile(`(?i)^\s*` + regexp.QuoteMeta(command) + `\s*`)
	return strings.TrimSpace(echo.ReplaceAllString(output, ""))
}

// Telnet runs a command on a password-protected Telnet console.
func Telnet(ctx context.Context, host string, port int, password, command string, control Control) (string, error) {
	if err := control.check(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	conn, err := dial(ctx, host, port, "Telnet console connection timed out", "Could not connect to the server's Telnet console")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	s := &session{conn: conn, control: control, connErr: "Could not connect to the server's Telnet console"}
	results := reader(conn, maxTCPReceived, "Telnet console response is too large")
	var decoder telnetDecoder
	var raw []byte
	passwordSent, commandSent := false, false
	quiet := time.NewTimer(time.Hour)
	quiet.Stop()
	commandTimer := time.NewTimer(time.Hour)
	commandTimer.Stop()
	finish := func() string { return CleanTelnet(decodeText(raw), command) }
	for {
		select {
		case <-quiet.C:
			return finish(), nil
		case <-commandTimer.C:
			if err := s.write([]byte(command + "\n")); err != nil {
				return "", err
			}
			commandSent = true
			quiet.Reset(telnetQuiet)
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return "", errors.New("Telnet console command timed out")
			}
			return "", errors.New("Console command cancelled")
		case result, ok := <-results:
			if !ok || result.err != nil {
				if ctx.Err() == context.DeadlineExceeded {
					return "", errors.New("Telnet console command timed out")
				}
				if result.err != nil && result.err.Error() == "Telnet console response is too large" {
					return "", result.err
				}
				if commandSent {
					return finish(), nil
				}
				return "", errors.New("Telnet console closed before the command was sent")
			}
			text, replies := decoder.decode(result.data)
			if len(replies) > 0 {
				if err := s.write(replies); err != nil {
					return "", err
				}
			}
			raw = append(raw, text...)
			output := decodeText(raw)
			if !passwordSent && passwordPrompt.MatchString(output) {
				passwordSent = true
				if err := s.write([]byte(password + "\n")); err != nil {
					return "", err
				}
				commandTimer.Reset(telnetCommandGap)
				continue
			}
			if passwordSent {
				if authFailed.MatchString(output) {
					return "", errors.New("Telnet console authentication failed")
				}
				if commandSent {
					quiet.Reset(telnetQuiet)
				}
			}
		}
	}
}
