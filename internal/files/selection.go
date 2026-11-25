package files

import (
	"context"
	"encoding/json"
	"path"
	"slices"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/logging"
)

// errUnconfirmed has a 5xx status, which pages report as an unconfirmed
// result.
var errUnconfirmed = apperr.New("FILE_UNCONFIRMED", 500, "The unpack was interrupted")

// MaxSelection is how many entries one selection can act on.
const MaxSelection = 500

// selectionBytes bounds a selection's names, which reach the helper as one
// command argument.
// JSON may encode a byte as six; a form, as three.
const selectionBytes = 16 << 10

// selection checks the names of entries in one folder of a root and returns
// their paths.
func selection(target Target, rootID, folder string, names []string) (Root, string, []string, error) {
	root, normalized, err := resolve(target, rootID, folder)
	if err != nil {
		return Root{}, "", nil, err
	}
	paths, err := selectionPaths(normalized, names)
	if err != nil {
		return Root{}, "", nil, err
	}
	return root, normalized, paths, nil
}

// CheckSelection refuses a selection of names in folder that a delete or a
// move of it would refuse, for a dialog to check before it opens.
func CheckSelection(folder string, names []string) error {
	normalized, err := NormalizeRelativePath(folder)
	if err == nil {
		_, err = selectionPaths(normalized, names)
	}
	return err
}

func selectionPaths(normalized string, names []string) ([]string, error) {
	if len(names) == 0 || len(names) > MaxSelection {
		return nil, Error("INVALID_SELECTION", 400)
	}
	size := 0
	paths := make([]string, 0, len(names))
	for index, name := range names {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
		if size += len(name); size > selectionBytes || slices.Contains(names[:index], name) {
			return nil, Error("INVALID_SELECTION", 400)
		}
		joined, err := joinChecked(normalized, name)
		if err != nil {
			return nil, err
		}
		paths = append(paths, joined)
	}
	return paths, nil
}

// DeleteSelection removes entries of one folder, after checking them all.
func (s *Service) DeleteSelection(ctx context.Context, target Target, rootID, folder string, names []string, assertAccess func() error) error {
	root, normalized, _, err := selection(target, rootID, folder, names)
	if err != nil {
		return err
	}
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return err
	}
	_, err = access.request(ctx, helper.FileRequest{Operation: "delete-paths", Root: root.Path, Path: normalized, Paths: names}, nil, nil)
	access.Cleanup(err != nil)
	return err
}

// Move moves entries of one folder into another folder of the same root,
// never replacing a name there.
func (s *Service) Move(ctx context.Context, target Target, rootID, folder string, names []string, destination string, assertAccess func() error) error {
	root, normalized, paths, err := selection(target, rootID, folder, names)
	if err != nil {
		return err
	}
	destination, err = NormalizeRelativePath(destination)
	if err != nil {
		return err
	}
	if destination == normalized {
		return Error("MOVE_SAME_FOLDER", 400)
	}
	for index, item := range paths {
		if Within(destination, item) {
			return Error("MOVE_INTO_ITSELF", 400)
		}
		if _, err := joinChecked(destination, names[index]); err != nil {
			return err
		}
	}
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return err
	}
	_, err = access.request(ctx, helper.FileRequest{Operation: "move", Root: root.Path, Path: normalized, Paths: names,
		Destination: destination}, nil, nil)
	access.Cleanup(err != nil)
	return err
}

// OpenSelectionDownload streams entries of one folder as one tar archive.
func (s *Service) OpenSelectionDownload(ctx context.Context, target Target, rootID, folder string, names []string, assertAccess func() error) (*Download, error) {
	root, normalized, _, err := selection(target, rootID, folder, names)
	if err != nil {
		return nil, err
	}
	access, release, err := s.acquireLister(ctx, target, root, assertAccess)
	if err != nil {
		return nil, err
	}
	name := root.Name
	if normalized != "" {
		name = path.Base(normalized)
	}
	download, err := s.openDownload(ctx, access, release, helper.FileRequest{Operation: "download-paths", Root: root.Path,
		Path: normalized, Paths: names}, name+" selection", assertAccess)
	if err != nil {
		release(false)
		return nil, err
	}
	return download, nil
}

// Unpack extracts an archive into a new folder beside it, keeping reserve
// bytes free, and returns the folder's name.
func (s *Service) Unpack(ctx context.Context, target Target, rootID, relative string, reserve int64, assertAccess func() error) (*helper.UnpackResult, error) {
	root, normalized, err := resolve(target, rootID, relative)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return nil, Error("UNSAFE_OR_CHANGED_FILE_PATH", 409)
	}
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return nil, err
	}
	// Recorded before the helper starts, so startup removes a staging folder
	// that Ludock stopping cut off.
	stage := Stage{Server: target.ServerID, Root: root.Path, Archive: normalized, ID: identity.NewUUID()}
	if err := s.recordStage(ctx, stage); err != nil {
		access.Cleanup(false)
		return nil, err
	}
	request := helper.FileRequest{Operation: "unpack", Root: root.Path, Path: normalized, UploadID: stage.ID, Reserve: &reserve,
		Blocked: access.blocked, Mounts: access.mounts}
	out, err := launch.Exec(ctx, s.Docker, access.container, launch.ExecOptions{Cmd: helper.Command("file", request),
		AssertAccess: assertAccess, Limit: helper.UnpackDuration + time.Minute})
	if err != nil {
		access.Cleanup(true)
		// A helper that refused the archive removed its staging folder; one cut
		// off may not have, so once it is gone a new helper removes it.
		if slices.ContainsFunc([]string{"ARCHIVE_UNSAFE", "ARCHIVE_TOO_LARGE", "ARCHIVE_NO_SPACE", "NOT_AN_ARCHIVE", "FILE_NAME_TAKEN"},
			func(code string) bool { return apperr.Is(err, code) }) {
			s.forgetStage(ctx, stage.ID)
			return nil, err
		}
		cleanup := context.WithoutCancel(ctx)
		if removeErr := s.removeStage(cleanup, target, root, normalized, stage.ID); removeErr == nil {
			s.forgetStage(cleanup, stage.ID)
		} else {
			log.Warn("Failed to remove an unpack staging folder; it is removed at the next start",
				logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
		}
		// A helper cut off by a timeout or revoked access, or one whose exit
		// is unknown, may have published its result.
		if public, ok := apperr.As(err); ok && public.Status == 401 {
			return nil, err
		}
		return nil, errUnconfirmed
	}
	access.Cleanup(false)
	s.forgetStage(ctx, stage.ID)
	var result helper.UnpackResult
	if err := json.Unmarshal(out, &result); err != nil || result.Name == "" {
		return nil, errUnconfirmed
	}
	return &result, nil
}
