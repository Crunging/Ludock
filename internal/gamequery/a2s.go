package gamequery

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"time"
)

const (
	// maxA2SPacket is larger than the 1400 bytes Source servers send in one
	// packet, so a reply is never cut short by the buffer.
	maxA2SPacket = 4096
	// a2sAttempts covers a lost datagram; a2sChallenges bounds a server that
	// keeps sending challenges.
	a2sAttempts   = 2
	a2sWait       = 2 * time.Second
	a2sChallenges = 2
)

var (
	singlePacket = []byte{0xff, 0xff, 0xff, 0xff}
	a2sInfo      = append(append([]byte(nil), singlePacket...), append([]byte{'T'}, "Source Engine Query\x00"...)...)
)

// A2SRequest is an A2S_INFO request, with the challenge a server sent, if
// any, appended.
func A2SRequest(challenge []byte) []byte {
	return append(append([]byte(nil), a2sInfo...), challenge...)
}

// a2s runs Valve's A2S_INFO server query over UDP. Split replies and the
// obsolete GoldSource reply are refused as no answer.
func a2s(ctx context.Context, host string, port int, authorize func() error) (Status, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Status{}, ErrNoAnswer
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	buffer := make([]byte, maxA2SPacket+1)
	var challenge []byte
	challenges := 0
	for attempt := 0; attempt < a2sAttempts; {
		if err := authorize(); err != nil {
			return Status{}, err
		}
		if _, err := conn.Write(A2SRequest(challenge)); err != nil {
			return Status{}, ErrNoAnswer
		}
		wait := time.Now().Add(a2sWait)
		if deadline.Before(wait) {
			wait = deadline
		}
		conn.SetReadDeadline(wait)
		n, err := conn.Read(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return Status{}, ErrNoAnswer
			}
			attempt++
			continue
		}
		reply := ParseA2S(buffer[:n])
		switch {
		case reply.Challenge != nil && challenges < a2sChallenges:
			challenge, challenges = reply.Challenge, challenges+1
		case reply.OK:
			return reply.Status, nil
		default:
			return Status{}, ErrNoAnswer
		}
	}
	return Status{}, ErrNoAnswer
}

// A2SReply is one parsed datagram: a challenge to resend with, or a status.
type A2SReply struct {
	Challenge []byte
	OK        bool
	Status    Status
}

// ParseA2S reads one A2S_INFO reply datagram.
func ParseA2S(packet []byte) A2SReply {
	if len(packet) > maxA2SPacket || len(packet) < 5 || !bytes.Equal(packet[:4], singlePacket) {
		return A2SReply{}
	}
	body := packet[5:]
	switch packet[4] {
	case 'A':
		if len(body) != 4 {
			return A2SReply{}
		}
		return A2SReply{Challenge: append([]byte(nil), body...)}
	case 'I':
		status, ok := parseA2SInfo(body)
		return A2SReply{OK: ok, Status: status}
	}
	return A2SReply{}
}

// parseA2SInfo reads a Source A2S_INFO reply after its header: protocol,
// name, map, folder, game, app ID, players, max players, bots, server type,
// environment, visibility, VAC, and version. Later fields aren't needed.
func parseA2SInfo(body []byte) (Status, bool) {
	if len(body) < 1 {
		return Status{}, false
	}
	rest := body[1:]
	for range 4 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return Status{}, false
		}
		rest = rest[end+1:]
	}
	if len(rest) < 9 || bytes.IndexByte(rest[9:], 0) < 0 {
		return Status{}, false
	}
	players, capacity, bots := int64(rest[2]), int64(rest[3]), int64(rest[4])
	serverType, environment, visibility, vac := rest[5], rest[6], rest[7], rest[8]
	if !bytes.ContainsRune([]byte("dlp"), rune(serverType|0x20)) || !bytes.ContainsRune([]byte("lwmo"), rune(environment|0x20)) ||
		visibility > 1 || vac > 1 {
		return Status{}, false
	}
	// The player count includes bots, which are nobody online.
	return counts(max(0, players-bots), capacity)
}
