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

// MaxSelection is how many entries one selection can act on.
const MaxSelection = 500

// selectionBytes bounds a selection's names, which reach the helper as one
// command argument.
const selectionBytes = 48 << 10

// selection checks the names of entries in one folder of a root and returns
// their paths.
func selection(target Target, rootID, folder string, names []string) (Root, string, []string, error) {
	root, normalized, err := resolve(target, rootID, folder)
	if err != nil {
		return Root{}, "", nil, err
	}
	if len(names) == 0 || len(names) > MaxSelection {
		return Root{}, "", nil, Error("INVALID_SELECTION", 400)
	}
	size := 0
	paths := make([]string, 0, len(names))
	for index, name := range names {
		if err := ValidateName(name); err != nil {
			return Root{}, "", nil, err
		}
		if size += len(name); size > selectionBytes || slices.Contains(names[:index], name) {
			return Root{}, "", nil, Error("INVALID_SELECTION", 400)
		}
		paths = append(paths, joinRelative(normalized, name))
	}
	return root, normalized, paths, nil
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
	for _, item := range paths {
		if Within(destination, item) {
			return Error("MOVE_INTO_ITSELF", 400)
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
	access, err := s.Acquire(ctx, target, root, true, assertAccess)
	if err != nil {
		return nil, err
	}
	name := root.Name
	if normalized != "" {
		name = path.Base(normalized)
	}
	download, err := s.openDownload(ctx, access, helper.FileRequest{Operation: "download-paths", Root: root.Path, Path: normalized,
		Paths: names}, name+" selection", assertAccess, target)
	if err != nil {
		access.Cleanup(false)
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
	stage := identity.NewUUID()
	request := helper.FileRequest{Operation: "unpack", Root: root.Path, Path: normalized, UploadID: stage, Reserve: &reserve,
		Blocked: access.blocked, Mounts: access.mounts}
	out, err := launch.Exec(ctx, s.Docker, access.container, launch.ExecOptions{Cmd: helper.Command("file", request),
		AssertAccess: assertAccess, Limit: helper.UnpackDuration + time.Minute})
	if err != nil {
		access.Cleanup(true)
		// A helper that refused the archive removed its staging folder; one cut
		// off may not have, so once it is gone a new helper removes it.
		if !slices.ContainsFunc([]string{"ARCHIVE_UNSAFE", "ARCHIVE_TOO_LARGE", "ARCHIVE_NO_SPACE", "NOT_AN_ARCHIVE", "FILE_NAME_TAKEN"},
			func(code string) bool { return apperr.Is(err, code) }) {
			s.removeStage(context.WithoutCancel(ctx), target, root, normalized, stage)
		}
		return nil, err
	}
	access.Cleanup(false)
	var result helper.UnpackResult
	if err := json.Unmarshal(out, &result); err != nil || result.Name == "" {
		return nil, Error("FILE_OPERATION_FAILED", 409)
	}
	return &result, nil
}

func (s *Service) removeStage(ctx context.Context, target Target, root Root, archive, stage string) {
	access, err := s.Acquire(ctx, target, root, false, nil)
	if err == nil {
		_, err = access.request(ctx, helper.FileRequest{Operation: "unpack-cleanup", Root: root.Path, Path: archive, UploadID: stage}, nil, nil)
		access.Cleanup(err != nil)
	}
	if err != nil {
		log.Warn("Failed to remove an unpack staging folder", logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
	}
}
