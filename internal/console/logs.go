package console

import (
	"context"
	"encoding/binary"
	"io"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("console")

const maxLogFrame = 16 * 1024 * 1024

// LogDecoder reads Docker logs, which are multiplexed unless the container
// has a TTY. Only a partial header is buffered.
type LogDecoder struct {
	output    func(kind, data string)
	mode      int // 0 unknown, 1 raw, 2 multiplexed
	header    [8]byte
	headerLen int
	remaining int
	channel   string
	text      map[string]*utf8Stream
}

func NewLogDecoder(output func(kind, data string)) *LogDecoder {
	return &LogDecoder{output: output, channel: "stdout", text: map[string]*utf8Stream{"stdout": {}, "stderr": {}}}
}

func (d *LogDecoder) write(kind string, chunk []byte) {
	if value := d.text[kind].decode(chunk); value != "" {
		d.output(kind, value)
	}
}

func (d *LogDecoder) raw() {
	d.mode = 1
	if d.headerLen > 0 {
		d.write("stdout", d.header[:d.headerLen])
		d.headerLen = 0
	}
}

func (d *LogDecoder) Push(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if d.mode == 1 {
		d.write("stdout", chunk)
		return
	}
	for offset := 0; offset < len(chunk); {
		if d.remaining > 0 {
			count := min(d.remaining, len(chunk)-offset)
			d.write(d.channel, chunk[offset:offset+count])
			d.remaining -= count
			offset += count
			continue
		}
		count := min(8-d.headerLen, len(chunk)-offset)
		copy(d.header[d.headerLen:], chunk[offset:offset+count])
		d.headerLen += count
		offset += count
		if d.mode == 0 {
			if (d.header[0] != 1 && d.header[0] != 2) || (d.headerLen >= 4 && (d.header[1] != 0 || d.header[2] != 0 || d.header[3] != 0)) {
				d.raw()
				d.write("stdout", chunk[offset:])
				return
			}
			if d.headerLen >= 4 {
				d.mode = 2
			}
		}
		if d.headerLen < 8 {
			return
		}
		kind := d.header[0]
		size := binary.BigEndian.Uint32(d.header[4:])
		if (kind != 1 && kind != 2) || d.header[1] != 0 || d.header[2] != 0 || d.header[3] != 0 || size > maxLogFrame {
			d.raw()
			d.write("stdout", chunk[offset:])
			return
		}
		d.channel = "stdout"
		if kind == 2 {
			d.channel = "stderr"
		}
		d.remaining = int(size)
		d.headerLen = 0
	}
}

// End reports whether the stream ended on a frame boundary. Incomplete
// UTF-8 and redaction tails after a truncated frame are dropped: they may
// hold a fragment of a credential.
func (d *LogDecoder) End() bool {
	if d.mode == 0 && d.headerLen > 0 {
		d.raw()
	}
	if d.headerLen > 0 || d.remaining > 0 {
		return false
	}
	for _, kind := range []string{"stdout", "stderr"} {
		if rest := d.text[kind].flush(); rest != "" {
			d.output(kind, rest)
		}
	}
	return true
}

type FollowOptions struct {
	Tail       int
	Timestamps bool
	Secrets    []string
	// Allowed is checked before every frame; false stops the stream.
	Allowed      func() bool
	Send         func(kind, data string)
	EndedMessage string
}

// Follow streams redacted container logs until ctx ends, access ends, or
// the stream ends. It returns an error only when Docker logs fail.
func Follow(ctx context.Context, client *docker.Client, containerID string, options FollowOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := NewRedactor(options.Secrets, func(value string) { options.Send("stdout", value) })
	stderr := NewRedactor(options.Secrets, func(value string) { options.Send("stderr", value) })
	decoder := NewLogDecoder(func(kind, value string) {
		if kind == "stdout" {
			stdout.Push(value)
		} else {
			stderr.Push(value)
		}
	})
	stream, err := client.Logs(ctx, containerID, docker.LogsOptions{Follow: true, Stdout: true, Stderr: true,
		Tail: options.Tail, Timestamps: options.Timestamps})
	if err != nil {
		log.Warn("Failed to attach Docker log stream", logging.F("container", short(containerID)))
		options.Send("error", "Failed to open Docker logs")
		return err
	}
	defer stream.Close()
	buffer := make([]byte, 32*1024)
	for {
		if !options.Allowed() {
			return nil
		}
		n, readErr := stream.Read(buffer)
		if n > 0 {
			if !options.Allowed() {
				return nil
			}
			decoder.Push(buffer[:n])
		}
		if readErr == io.EOF {
			if !decoder.End() {
				break
			}
			stdout.End()
			stderr.End()
			options.Send("system", options.EndedMessage)
			return nil
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			break
		}
	}
	log.Warn("Docker log stream failed", logging.F("container", short(containerID)))
	options.Send("error", "Docker log stream failed")
	return io.ErrUnexpectedEOF
}

func short(id string) string { return id[:min(12, len(id))] }
