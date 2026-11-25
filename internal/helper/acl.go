package helper

import (
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The extended attributes that hold a file's POSIX ACLs, in the kernel's
// binary form.
const (
	accessACL  = "system.posix_acl_access"
	defaultACL = "system.posix_acl_default"
)

// The PAX records that hold an archive entry's access and default ACLs, as
// GNU tar and bsdtar name them.
const (
	ACLAccessRecord  = "SCHILY.acl.access"
	ACLDefaultRecord = "SCHILY.acl.default"
)

var errACLNotKept = errors.New("the file's access control list could not be kept")

// MaxACLEntries bounds an ACL a backup keeps, so an entry's archive metadata
// stays within what restore reads.
const MaxACLEntries = 1024

const (
	aclVersion   = 2
	aclHeader    = 4
	aclEntrySize = 8
	aclNoID      = 0xffffffff

	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
)

type aclEntry struct {
	tag, perm uint16
	id        uint32
}

var errInvalidACL = errors.New("invalid access control list")

// getxattr reads an extended attribute through a descriptor. An attribute
// that isn't set, or a filesystem without them, gives nil.
func getxattr(file fd, name string) ([]byte, error) {
	for {
		size, err := unix.Fgetxattr(int(file), name, nil)
		if unsupportedXattr(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		value := make([]byte, size)
		if size, err = unix.Fgetxattr(int(file), name, value); errors.Is(err, unix.ERANGE) {
			continue
		} else if unsupportedXattr(err) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		return value[:size], nil
	}
}

func unsupportedXattr(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

// readAccessACL reads the access ACL of the regular file name in parent,
// which must be original, through its descriptor. A file without one, or a
// filesystem without ACLs, gives nil.
func (h *fileHelper) readAccessACL(parent fd, name string, original *unix.Stat_t) ([]byte, error) {
	file, err := h.t.openAt(parent, name, fileFlags, 0)
	if err != nil {
		return nil, err
	}
	defer h.t.close(file)
	if info, err := fstat(file); err != nil || !sameInode(info, original) {
		return nil, errors.New("the upload target changed")
	}
	value, err := getxattr(file, accessACL)
	if err != nil {
		return nil, errACLNotKept
	}
	return value, nil
}

// readACL reads an ACL through a descriptor, or nil without one.
func readACL(file fd, attribute string) ([]aclEntry, error) {
	value, err := getxattr(file, attribute)
	if err != nil || value == nil {
		return nil, err
	}
	return decodeACL(value)
}

// decodeACL reads an ACL in the kernel's form into the kernel's order and
// checks it.
func decodeACL(value []byte) ([]aclEntry, error) {
	if len(value) < aclHeader || (len(value)-aclHeader)%aclEntrySize != 0 ||
		binary.LittleEndian.Uint32(value) != aclVersion {
		return nil, errInvalidACL
	}
	var entries []aclEntry
	for offset := aclHeader; offset < len(value); offset += aclEntrySize {
		entries = append(entries, aclEntry{binary.LittleEndian.Uint16(value[offset:]), binary.LittleEndian.Uint16(value[offset+2:]),
			binary.LittleEndian.Uint32(value[offset+4:])})
	}
	sortACL(entries)
	return entries, checkACL(entries)
}

// sortACL puts entries in the kernel's order: by tag, then ID.
func sortACL(entries []aclEntry) {
	slices.SortStableFunc(entries, func(a, b aclEntry) int {
		if a.tag != b.tag {
			return int(a.tag) - int(b.tag)
		}
		return int(int64(a.id) - int64(b.id))
	})
}

func encodeACL(entries []aclEntry) []byte {
	value := binary.LittleEndian.AppendUint32(nil, aclVersion)
	for _, entry := range entries {
		value = binary.LittleEndian.AppendUint16(value, entry.tag)
		value = binary.LittleEndian.AppendUint16(value, entry.perm)
		value = binary.LittleEndian.AppendUint32(value, entry.id)
	}
	return value
}

// checkACL requires sorted entries without a repeated one, one owner, owning
// group, and other entry, and a mask once there is a named entry.
func checkACL(entries []aclEntry) error {
	if len(entries) > MaxACLEntries {
		return errInvalidACL
	}
	count := map[uint16]int{}
	for index, entry := range entries {
		named := entry.tag == aclUser || entry.tag == aclGroup
		switch {
		case entry.perm&^7 != 0, named == (entry.id == aclNoID),
			!slices.Contains([]uint16{aclUserObj, aclUser, aclGroupObj, aclGroup, aclMask, aclOther}, entry.tag):
			return errInvalidACL
		}
		if index > 0 {
			previous := entries[index-1]
			if previous.tag > entry.tag || (previous.tag == entry.tag && (!named || previous.id >= entry.id)) {
				return errInvalidACL
			}
		}
		count[entry.tag]++
	}
	if count[aclUserObj] != 1 || count[aclGroupObj] != 1 || count[aclOther] != 1 ||
		(count[aclUser]+count[aclGroup] > 0 && count[aclMask] != 1) {
		return errInvalidACL
	}
	return nil
}

// aclPerm is the permissions of an ACL's first entry with tag, if any.
func aclPerm(entries []aclEntry, tag uint16) (uint32, bool) {
	for _, entry := range entries {
		if entry.tag == tag {
			return uint32(entry.perm), true
		}
	}
	return 0, false
}

// aclMode is the mode's permission bits a file with the ACL has: the group
// bits are its mask, when it has one.
func aclMode(entries []aclEntry) uint32 {
	owner, _ := aclPerm(entries, aclUserObj)
	group, _ := aclPerm(entries, aclGroupObj)
	if mask, ok := aclPerm(entries, aclMask); ok {
		group = mask
	}
	other, _ := aclPerm(entries, aclOther)
	return owner<<6 | group<<3 | other
}

// ownGroupMode replaces the group bits of a file's mode, which are the mask
// of its ACL, with the owning group's own rights: a reader that ignores the
// ACL must not give the whole group what only named entries had.
func ownGroupMode(mode uint32, entries []aclEntry) uint32 {
	group, _ := aclPerm(entries, aclGroupObj)
	return mode&^0o070 | group<<3
}

// ACLText writes an ACL in the kernel's form as Ludock stores it in an
// archive's SCHILY.acl.access and SCHILY.acl.default records: libacl's short
// text form with numeric IDs, which GNU tar and bsdtar both read.
func ACLText(value []byte) (string, error) {
	entries, err := decodeACL(value)
	if err != nil {
		return "", err
	}
	return aclText(entries), nil
}

func aclText(entries []aclEntry) string {
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		var tag, qualifier string
		switch entry.tag {
		case aclUserObj, aclUser:
			tag = "user"
		case aclGroupObj, aclGroup:
			tag = "group"
		case aclMask:
			tag = "mask"
		default:
			tag = "other"
		}
		if entry.id != aclNoID {
			qualifier = strconv.FormatUint(uint64(entry.id), 10)
		}
		perm := []byte("rwx")
		for bit, index := uint16(4), 0; bit > 0; bit, index = bit>>1, index+1 {
			if entry.perm&bit == 0 {
				perm[index] = '-'
			}
		}
		parts = append(parts, tag+":"+qualifier+":"+string(perm))
	}
	return strings.Join(parts, ",")
}

// ParseACLText reads an ACL's text, as ACLText writes it or as GNU tar or
// bsdtar store it, into the kernel's form. A named entry needs a numeric ID,
// as its qualifier or the fourth field bsdtar adds after a name: names
// differ between the host and containers.
func ParseACLText(text string) ([]byte, error) {
	var entries []aclEntry
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		fields := strings.Split(part, ":")
		if len(fields) != 3 && len(fields) != 4 {
			return nil, errInvalidACL
		}
		tags := map[string][2]uint16{"user": {aclUserObj, aclUser}, "group": {aclGroupObj, aclGroup}, "mask": {aclMask, 0}, "other": {aclOther, 0}}
		kind, ok := tags[fields[0]]
		if !ok {
			return nil, errInvalidACL
		}
		entry := aclEntry{tag: kind[0], id: aclNoID}
		qualifier := fields[1]
		if len(fields) == 4 {
			if qualifier == "" {
				return nil, errInvalidACL
			}
			qualifier = fields[3]
		}
		if qualifier != "" {
			id, err := strconv.ParseUint(qualifier, 10, 32)
			if kind[1] == 0 || err != nil || id == aclNoID || qualifier[0] == '+' {
				return nil, errInvalidACL
			}
			entry.tag, entry.id = kind[1], uint32(id)
		}
		if len(fields[2]) != 3 {
			return nil, errInvalidACL
		}
		for index, letter := range "rwx" {
			switch fields[2][index] {
			case byte(letter):
				entry.perm |= 4 >> index
			case '-':
			default:
				return nil, errInvalidACL
			}
		}
		entries = append(entries, entry)
	}
	sortACL(entries)
	if err := checkACL(entries); err != nil {
		return nil, err
	}
	return encodeACL(entries), nil
}

// narrowACL caps an ACL's mask and its other entry at the owner's rights, as
// the mode is when the owner changes: until then the future owner holds
// those. It returns nil for a value it can't read.
func narrowACL(value []byte) []byte {
	entries, err := decodeACL(value)
	if err != nil {
		return nil
	}
	owner, _ := aclPerm(entries, aclUserObj)
	for index, entry := range entries {
		if entry.tag == aclMask || entry.tag == aclOther {
			entries[index].perm &= uint16(owner)
		}
	}
	return encodeACL(entries)
}
