package helper

import (
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// accessACL is the extended attribute that holds a file's POSIX access ACL.
const accessACL = "system.posix_acl_access"

var errACLNotKept = errors.New("the file's access control list could not be kept")

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
	for {
		size, err := unix.Fgetxattr(int(file), accessACL, nil)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		if err != nil {
			return nil, errACLNotKept
		}
		value := make([]byte, size)
		if size, err = unix.Fgetxattr(int(file), accessACL, value); errors.Is(err, unix.ERANGE) {
			continue
		} else if err != nil {
			return nil, errACLNotKept
		}
		return value[:size], nil
	}
}

// narrowACL caps an ACL's mask and its other entry at the owner's rights, as
// the mode is when the owner changes: until then the future owner holds
// those. It returns nil for a value it can't read.
func narrowACL(value []byte) []byte {
	const (
		header  = 4
		entry   = 8
		userObj = 0x01
		mask    = 0x10
		other   = 0x20
	)
	if len(value) < header || (len(value)-header)%entry != 0 {
		return nil
	}
	narrowed := append([]byte(nil), value...)
	owner := uint16(0)
	for offset := header; offset < len(narrowed); offset += entry {
		if binary.LittleEndian.Uint16(narrowed[offset:]) == userObj {
			owner = binary.LittleEndian.Uint16(narrowed[offset+2:])
		}
	}
	for offset := header; offset < len(narrowed); offset += entry {
		if tag := binary.LittleEndian.Uint16(narrowed[offset:]); tag == mask || tag == other {
			binary.LittleEndian.PutUint16(narrowed[offset+2:], binary.LittleEndian.Uint16(narrowed[offset+2:])&owner)
		}
	}
	return narrowed
}
