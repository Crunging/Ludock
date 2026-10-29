package docker

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

var (
	ErrInvalidStream    = errors.New("Invalid Docker stream frame")
	ErrIncompleteStream = errors.New("Incomplete Docker stream frame")
)

const maxFrame = 64 * 1024 * 1024

// Demux splits a multiplexed Docker stream. Only the 8-byte header is
// buffered, never a whole frame. Completion includes the consumers' writes.
// A nil writer discards that channel.
func Demux(source io.Reader, stdout, stderr io.Writer) error {
	var header [8]byte
	for {
		n, err := io.ReadFull(source, header[:])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if n > 0 && (err == io.ErrUnexpectedEOF) {
				return ErrIncompleteStream
			}
			return err
		}
		channel := header[0]
		size := binary.BigEndian.Uint32(header[4:])
		if channel > 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 || size > maxFrame {
			return ErrInvalidStream
		}
		var target io.Writer = io.Discard
		if channel == 1 && stdout != nil {
			target = stdout
		} else if channel == 2 && stderr != nil {
			target = stderr
		}
		copied, err := io.CopyN(target, source, int64(size))
		if err == io.EOF && copied < int64(size) {
			return ErrIncompleteStream
		}
		if err != nil {
			return err
		}
	}
}

func Stdout(source io.Reader) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(Demux(source, writer, nil)) }()
	return reader
}

var ErrJSONLineTooLong = errors.New("JSON line exceeded its limit")

// DecodeJSONLines calls receive for each newline-delimited JSON value, with
// a per-record byte limit. Incomplete final records and malformed input fail
// unless skipInvalid is set (event feeds may skip bad records).
func DecodeJSONLines(source io.Reader, maxBytes int, skipInvalid bool, receive func(json.RawMessage) error) error {
	reader := bufio.NewReaderSize(source, 64*1024)
	var pending []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(pending)+len(chunk) > maxBytes+1 {
			return ErrJSONLineTooLong
		}
		pending = append(pending, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		line := bytes.TrimSuffix(pending, []byte("\n"))
		if len(line) > maxBytes {
			return ErrJSONLineTooLong
		}
		if len(bytes.TrimSpace(line)) > 0 {
			if !json.Valid(line) {
				if !skipInvalid {
					return errors.New("Invalid JSON line")
				}
			} else if receiveErr := receive(append(json.RawMessage(nil), line...)); receiveErr != nil {
				return receiveErr
			}
		}
		pending = pending[:0]
		if err == io.EOF {
			return nil
		}
	}
}
