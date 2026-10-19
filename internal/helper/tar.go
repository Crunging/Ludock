package helper

import (
	"errors"
	"strconv"
	"strings"
)

// tarInfo is the metadata stored in an archive header.
type tarInfo struct {
	size, uid, gid, mode int64
	mtimeMs              float64
}

const (
	typeFile      = '0'
	typeDirectory = '5'
	typePax       = 'x'
)

var errArchiveValue = errors.New("an archive value is too large")

// tarHeader builds a ustar header like v0.5.0's helper. A name that does not
// fit the ustar name/prefix fields gets a per-entry PAX path record, which
// keeps long and Unicode names intact. Ownership is kept only for backups.
func tarHeader(name string, info tarInfo, directory bool, kind byte, keepOwner bool) ([]byte, error) {
	if kind == 0 {
		kind = typeFile
		if directory {
			kind = typeDirectory
		}
	}
	header := make([]byte, 512)
	if len(name) > 4096 {
		return nil, errors.New("an archive path is too long")
	}
	if len(name) > 100 {
		searchEnd := len(name)
		if strings.HasSuffix(name, "/") {
			searchEnd = len(name) - 1
		}
		slash := strings.LastIndex(name[:max(0, searchEnd)], "/")
		if slash < 1 || len(name[:slash]) > 155 || len(name[slash+1:]) > 100 {
			record := " path=" + name + "\n"
			size := len(record) + 1
			for len(strconv.Itoa(size))+len(record) != size {
				size = len(strconv.Itoa(size)) + len(record)
			}
			payload := []byte(strconv.Itoa(size) + record)
			paxInfo := info
			paxInfo.size = int64(len(payload))
			pax, err := tarHeader("PaxHeader", paxInfo, false, typePax, keepOwner)
			if err != nil {
				return nil, err
			}
			entry, err := tarHeader("entry", info, directory, kind, keepOwner)
			if err != nil {
				return nil, err
			}
			result := append(pax, payload...)
			result = append(result, make([]byte, (512-len(payload)%512)%512)...)
			return append(result, entry...), nil
		}
		copy(header[345:345+155], name[:slash])
		name = name[slash+1:]
	}
	copy(header[0:100], name)
	octal := func(value int64, offset, length int) error {
		if value < 0 {
			value = 0
		}
		encoded := strconv.FormatInt(value, 8)
		if len(encoded) >= length {
			// GNU/POSIX readers accept positive base-256 fields.
			header[offset] = 128
			remaining := uint64(value)
			for index := offset + length - 1; index > offset; index-- {
				header[index] = byte(remaining)
				remaining >>= 8
			}
			if remaining != 0 {
				return errArchiveValue
			}
			return nil
		}
		copy(header[offset:offset+length], strings.Repeat("0", length-1-len(encoded))+encoded+"\x00")
		return nil
	}
	uid, gid, size := int64(0), int64(0), info.size
	if keepOwner {
		uid, gid = info.uid, info.gid
	}
	if directory {
		size = 0
	}
	mtime := int64(info.mtimeMs / 1000)
	if info.mtimeMs < 0 {
		mtime = 0
	}
	for _, field := range []struct {
		value          int64
		offset, length int
	}{{info.mode & 0o777, 100, 8}, {uid, 108, 8}, {gid, 116, 8}, {size, 124, 12}, {mtime, 136, 12}} {
		if err := octal(field.value, field.offset, field.length); err != nil {
			return nil, err
		}
	}
	for index := 148; index < 156; index++ {
		header[index] = ' '
	}
	header[156] = kind
	copy(header[257:263], "ustar\x00")
	copy(header[263:265], "00")
	checksum := 0
	for _, value := range header {
		checksum += int(value)
	}
	encoded := strconv.FormatInt(int64(checksum), 8)
	copy(header[148:156], strings.Repeat("0", max(0, 6-len(encoded)))+encoded+"\x00 ")
	return header, nil
}
