// Package tarstream reads and writes the uncompressed USTAR/PAX archives
// Ludock stores as backups, one entry at a time. Game backups can be larger
// than memory. Nothing here extracts paths.
package tarstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrInvalid reports a malformed or truncated archive.
var ErrInvalid = errors.New("Invalid or truncated backup archive")

const (
	blockSize        = 512
	maxMetadataBytes = 16384
)

var types = map[byte]string{'0': "file", '1': "link", '2': "symlink", '3': "character-device", '4': "block-device",
	'5': "directory", '6': "fifo", '7': "contiguous-file", 'x': "pax-header", 'g': "pax-global-header"}

// Header is one archive entry. MtimeSeconds may be fractional (PAX).
type Header struct {
	Name         string
	Type         string
	Size         int64
	Mode         int64
	UID, GID     int64
	MtimeSeconds float64
	Linkname     string
	// PAX holds the entry's PAX records (local merged over global).
	PAX map[string]string
}

// Padding is the zero padding after a body of size bytes.
func Padding(size int64) []byte { return make([]byte, (blockSize-size%blockSize)%blockSize) }

// End is the two zero blocks that end an archive.
func End() []byte { return make([]byte, 2*blockSize) }

func textField(field []byte) (string, error) {
	if end := bytes.IndexByte(field, 0); end >= 0 {
		field = field[:end]
	}
	if !utf8.Valid(field) {
		return "", ErrInvalid
	}
	return string(field), nil
}

var octalPattern = regexp.MustCompile(`^[0-7]+$`)

func integer(field []byte) (int64, error) {
	var value uint64
	if field[0]&128 != 0 {
		// Positive base-256, used for large sizes and Linux owner IDs.
		if field[0] != 128 {
			return 0, ErrInvalid
		}
		for _, b := range field[1:] {
			if value > (1<<53-1)>>8 {
				return 0, ErrInvalid
			}
			value = value<<8 | uint64(b)
		}
	} else {
		text, err := textField(field)
		if err != nil {
			return 0, err
		}
		encoded := strings.TrimSpace(text)
		if encoded != "" {
			if !octalPattern.MatchString(encoded) {
				return 0, ErrInvalid
			}
			parsed, err := strconv.ParseUint(encoded, 8, 64)
			if err != nil {
				return 0, ErrInvalid
			}
			value = parsed
		}
	}
	if value > 1<<53-1 {
		return 0, ErrInvalid
	}
	return int64(value), nil
}

// DecodeHeader parses one header block. pathOverride (from PAX) replaces
// the USTAR name, which producers may truncate inside a UTF-8 sequence.
func DecodeHeader(block []byte, pathOverride *string) (*Header, error) {
	if len(block) != blockSize {
		return nil, ErrInvalid
	}
	checksum, err := integer(block[148:156])
	if err != nil {
		return nil, err
	}
	actual := int64(8 * 32)
	for index, b := range block {
		if index < 148 || index >= 156 {
			actual += int64(b)
		}
	}
	magic, err := textField(block[257:263])
	if err != nil || checksum != actual || strings.TrimSpace(magic) != "ustar" {
		return nil, ErrInvalid
	}
	flag := block[156]
	if flag == 0 {
		flag = '0'
	}
	kind, ok := types[flag]
	if !ok {
		return nil, ErrInvalid
	}
	override := pathOverride
	if override == nil && (flag == 'x' || flag == 'g') {
		paxName := "PaxHeader"
		override = &paxName
	}
	header := &Header{Type: kind}
	if override != nil {
		header.Name = *override
	} else {
		prefix, err := textField(block[345:500])
		if err != nil {
			return nil, err
		}
		name, err := textField(block[0:100])
		if err != nil {
			return nil, err
		}
		header.Name = name
		if prefix != "" {
			header.Name = prefix + "/" + name
		}
	}
	fields := []*int64{&header.Mode, &header.UID, &header.GID, &header.Size}
	for index, bounds := range [][2]int{{100, 108}, {108, 116}, {116, 124}, {124, 136}} {
		if *fields[index], err = integer(block[bounds[0]:bounds[1]]); err != nil {
			return nil, err
		}
	}
	mtime, err := integer(block[136:148])
	if err != nil {
		return nil, err
	}
	header.MtimeSeconds = float64(mtime)
	if header.Linkname, err = textField(block[157:257]); err != nil {
		return nil, err
	}
	return header, nil
}

var (
	decimalPattern  = regexp.MustCompile(`^[1-9]\d*$`)
	unsignedPattern = regexp.MustCompile(`^\d+$`)
	mtimePattern    = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`)
)

func parsePAX(data []byte) (map[string]string, error) {
	result := map[string]string{}
	for offset := 0; offset < len(data); {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 || space > 8 {
			return nil, ErrInvalid
		}
		space += offset
		encoded := string(data[offset:space])
		if !decimalPattern.MatchString(encoded) {
			return nil, ErrInvalid
		}
		length, _ := strconv.Atoi(encoded)
		end := offset + length
		if end > len(data) || end <= space+3 || data[end-1] != '\n' {
			return nil, ErrInvalid
		}
		recordBytes := data[space+1 : end-1]
		if !utf8.Valid(recordBytes) {
			return nil, ErrInvalid
		}
		record := string(recordBytes)
		equals := strings.Index(record, "=")
		if equals < 1 || strings.Contains(record, "\x00") {
			return nil, ErrInvalid
		}
		key := record[:equals]
		if _, exists := result[key]; exists || strings.HasPrefix(key, "GNU.sparse") || key == "SCHILY.filetype" {
			return nil, ErrInvalid
		}
		result[key] = record[equals+1:]
		offset = end
	}
	return result, nil
}

func applyPAX(header *Header, pax map[string]string) error {
	header.PAX = pax
	if value, ok := pax["path"]; ok {
		header.Name = value
	}
	if value, ok := pax["linkpath"]; ok {
		header.Linkname = value
	}
	for key, target := range map[string]*int64{"size": &header.Size, "uid": &header.UID, "gid": &header.GID} {
		value, ok := pax[key]
		if !ok {
			continue
		}
		if !unsignedPattern.MatchString(value) {
			return ErrInvalid
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed > 1<<53-1 {
			return ErrInvalid
		}
		*target = int64(parsed)
	}
	if value, ok := pax["mtime"]; ok {
		if !mtimePattern.MatchString(value) {
			return ErrInvalid
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsInf(parsed, 0) || math.Abs(parsed*1000) > 8.64e15 {
			return ErrInvalid
		}
		header.MtimeSeconds = parsed
	}
	return nil
}

type source struct {
	reader  io.Reader
	pending []byte
	buffer  []byte
	eof     bool
	onChunk func([]byte) error
}

func (s *source) take(limit int) ([]byte, error) {
	for len(s.pending) == 0 && !s.eof {
		n, err := s.reader.Read(s.buffer)
		if n > 0 {
			chunk := s.buffer[:n]
			if s.onChunk != nil {
				if err := s.onChunk(chunk); err != nil {
					return nil, err
				}
			}
			s.pending = chunk
		}
		if err == io.EOF {
			s.eof = true
		} else if err != nil {
			return nil, err
		}
	}
	part := s.pending[:min(limit, len(s.pending))]
	s.pending = s.pending[len(part):]
	return part, nil
}

func (s *source) exact(size int64) ([]byte, error) {
	result := make([]byte, 0, size)
	for int64(len(result)) < size {
		part, err := s.take(int(size) - len(result))
		if err != nil {
			return nil, err
		}
		if len(part) == 0 {
			return nil, ErrInvalid
		}
		result = append(result, part...)
	}
	return result, nil
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

type body struct {
	source    *source
	remaining int64
}

func (b *body) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	part, err := b.source.take(int(min(int64(len(p)), b.remaining, 65536)))
	if err != nil {
		return 0, err
	}
	if len(part) == 0 {
		return 0, ErrInvalid
	}
	b.remaining -= int64(len(part))
	return copy(p, part), nil
}

// Walk visits every entry. The visitor must finish with body before
// returning; unread bytes are drained before the next header. onChunk sees
// every raw input chunk, including terminal padding, so callers can hash and
// budget the exact archive bytes.
func Walk(input io.Reader, visit func(*Header, io.Reader) error, onChunk func([]byte) error) error {
	s := &source{reader: input, buffer: make([]byte, 64*1024), onChunk: onChunk}
	global := map[string]string{}
	var local map[string]string
	metadata := 0
	for {
		block, err := s.exact(blockSize)
		if err != nil {
			return err
		}
		if allZero(block) {
			second, err := s.exact(blockSize)
			if err != nil {
				return err
			}
			if local != nil || !allZero(second) {
				return ErrInvalid
			}
			trailing := 0
			for !s.eof || len(s.pending) > 0 {
				part, err := s.take(65536)
				if err != nil {
					return err
				}
				trailing += len(part)
				if !allZero(part) {
					return ErrInvalid
				}
				if len(part) == 0 {
					break
				}
			}
			if trailing%blockSize != 0 {
				return ErrInvalid
			}
			return nil
		}
		var override *string
		if value, ok := local["path"]; ok {
			override = &value
		} else if value, ok := global["path"]; ok {
			override = &value
		}
		header, err := DecodeHeader(block, override)
		if err != nil {
			return err
		}
		if header.Type == "pax-header" || header.Type == "pax-global-header" {
			metadata++
			if header.Size > maxMetadataBytes || metadata > 100000 || local != nil {
				return ErrInvalid
			}
			data, err := s.exact(header.Size)
			if err != nil {
				return err
			}
			pax, err := parsePAX(data)
			if err != nil {
				return err
			}
			padding, err := s.exact(int64(len(Padding(header.Size))))
			if err != nil {
				return err
			}
			if !allZero(padding) {
				return ErrInvalid
			}
			if header.Type == "pax-global-header" {
				for key, value := range pax {
					global[key] = value
				}
				if encoded, _ := json.Marshal(global); len(encoded) > maxMetadataBytes {
					return ErrInvalid
				}
			} else {
				local = pax
			}
			continue
		}
		merged := map[string]string{}
		for key, value := range global {
			merged[key] = value
		}
		for key, value := range local {
			merged[key] = value
		}
		if err := applyPAX(header, merged); err != nil {
			return err
		}
		local = nil
		if header.Type != "file" && header.Size != 0 {
			return ErrInvalid
		}
		entry := &body{source: s, remaining: header.Size}
		if err := visit(header, entry); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, entry); err != nil {
			return err
		}
		padding, err := s.exact(int64(len(Padding(header.Size))))
		if err != nil {
			return err
		}
		if !allZero(padding) {
			return ErrInvalid
		}
	}
}

func paxRecord(key, value string) []byte {
	record := " " + key + "=" + value + "\n"
	size := len(record) + 1
	for len(strconv.Itoa(size))+len(record) != size {
		size = len(strconv.Itoa(size)) + len(record)
	}
	return []byte(strconv.Itoa(size) + record)
}

func rawHeader(header Header, flag byte) ([]byte, error) {
	block := make([]byte, blockSize)
	text := func(value string, offset, length int) error {
		if len(value) > length || strings.Contains(value, "\x00") {
			return ErrInvalid
		}
		copy(block[offset:], value)
		return nil
	}
	number := func(value int64, offset, length int) error {
		if value < 0 || value > 1<<53-1 {
			return ErrInvalid
		}
		octal := strconv.FormatInt(value, 8)
		if len(octal) < length {
			return text(strings.Repeat("0", length-1-len(octal))+octal, offset, length)
		}
		wide := uint64(value)
		for index := offset + length - 1; index > offset; index-- {
			block[index] = byte(wide)
			wide >>= 8
		}
		if wide != 0 {
			return ErrInvalid
		}
		block[offset] = 128
		return nil
	}
	mtime := int64(math.Max(0, math.Floor(header.MtimeSeconds)))
	for _, step := range []func() error{
		func() error { return text(header.Name, 0, 100) },
		func() error { return number(header.Mode, 100, 8) },
		func() error { return number(header.UID, 108, 8) },
		func() error { return number(header.GID, 116, 8) },
		func() error { return number(header.Size, 124, 12) },
		func() error { return number(mtime, 136, 12) },
	} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	for index := 148; index < 156; index++ {
		block[index] = ' '
	}
	block[156] = flag
	if err := text(header.Linkname, 157, 100); err != nil {
		return nil, err
	}
	copy(block[257:], "ustar")
	copy(block[263:], "00")
	sum := 0
	for _, b := range block {
		sum += int(b)
	}
	encoded := strconv.FormatInt(int64(sum), 8)
	copy(block[148:], strings.Repeat("0", max(0, 6-len(encoded)))+encoded)
	block[154] = 0
	return block, nil
}

// FormatSeconds writes a timestamp like JavaScript's String(number).
func FormatSeconds(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

// EncodeHeader writes an entry header, preceded by a PAX header when the
// name is long or extra records are given. Mode defaults to 0644 for files.
func EncodeHeader(header Header) ([]byte, error) {
	var flag byte
	for candidate, kind := range types {
		if kind == header.Type {
			flag = candidate
		}
	}
	if header.Type == "" {
		flag = '0'
	}
	if flag == 0 || len(header.Name) > 4096 {
		return nil, ErrInvalid
	}
	pax := map[string]string{}
	for key, value := range header.PAX {
		pax[key] = value
	}
	if _, ok := pax["path"]; !ok && len(header.Name) > 100 {
		pax["path"] = header.Name
	}
	if _, ok := pax["mtime"]; !ok && (header.MtimeSeconds < 0 || header.MtimeSeconds != math.Trunc(header.MtimeSeconds)) {
		pax["mtime"] = FormatSeconds(header.MtimeSeconds)
	}
	var result []byte
	if len(pax) > 0 {
		keys := make([]string, 0, len(pax))
		for key := range pax {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			return paxOrder(keys[i]) < paxOrder(keys[j]) || (paxOrder(keys[i]) == paxOrder(keys[j]) && keys[i] < keys[j])
		})
		var records []byte
		for _, key := range keys {
			records = append(records, paxRecord(key, pax[key])...)
		}
		if len(records) > maxMetadataBytes {
			return nil, ErrInvalid
		}
		paxHeader, err := rawHeader(Header{Name: "PaxHeader", Size: int64(len(records)), Mode: 0o644}, 'x')
		if err != nil {
			return nil, err
		}
		result = append(append(append(result, paxHeader...), records...), Padding(int64(len(records)))...)
	}
	entry := header
	if _, ok := pax["path"]; ok {
		entry.Name = "entry"
	}
	raw, err := rawHeader(entry, flag)
	if err != nil {
		return nil, err
	}
	return append(result, raw...), nil
}

// paxOrder writes records in a fixed order, so encoding is deterministic.
func paxOrder(key string) int {
	switch key {
	case "uid":
		return 0
	case "gid":
		return 1
	case "mtime":
		return 2
	case "path":
		return 3
	}
	return 4
}
