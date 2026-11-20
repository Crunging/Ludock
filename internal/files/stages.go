package files

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Crunging/Ludock/internal/helper"
)

// stagePrefix keys an unpack's staging folder in the settings table from
// before its helper starts until the folder is confirmed gone.
const stagePrefix = "unpack-stage:"

// stageAttempts is how many startups try to remove a recorded staging folder
// before its record is dropped.
const stageAttempts = 3

// Stage is an unpack's staging folder: the server's logical ID, the data
// folder's container path, the archive's path in it, and the staging ID.
type Stage struct {
	Server   string `json:"server"`
	Root     string `json:"root"`
	Archive  string `json:"archive"`
	ID       string `json:"id"`
	Attempts int    `json:"attempts,omitempty"`
}

func (s *Service) recordStage(ctx context.Context, stage Stage) error {
	if s.DB == nil {
		return errors.New("no database for unpack records")
	}
	return s.DB.SetSetting(ctx, nil, stagePrefix+stage.ID, stage)
}

func (s *Service) forgetStage(ctx context.Context, id string) error {
	return s.DB.DeleteSetting(context.WithoutCancel(ctx), stagePrefix+id)
}

// Stages lists the recorded staging folders.
func (s *Service) Stages(ctx context.Context) ([]Stage, error) {
	saved, err := s.DB.SettingsWithPrefix(ctx, stagePrefix)
	if err != nil {
		return nil, err
	}
	var stages []Stage
	for _, setting := range saved {
		var stage Stage
		if json.Unmarshal([]byte(setting.Value), &stage) == nil {
			stages = append(stages, stage)
		}
	}
	return stages, nil
}

// RemoveStages removes the staging folders unpacks interrupted by Ludock
// stopping left behind, and then their records. resolve returns the server's
// current target, or nil when it is no longer known. A record whose folder
// can't be removed is kept for the next startup, up to stageAttempts, and
// never fails startup.
func (s *Service) RemoveStages(ctx context.Context, resolve func(ctx context.Context, server string) (*Target, error)) error {
	stages, err := s.Stages(ctx)
	if err != nil {
		return err
	}
	for _, stage := range stages {
		target, err := resolve(ctx, stage.Server)
		if err == nil && target != nil {
			err = s.removeRecorded(ctx, *target, stage)
		}
		switch {
		case err == nil:
			err = s.forgetStage(ctx, stage.ID)
		case stage.Attempts+1 >= stageAttempts:
			log.Warn("Gave up removing an interrupted unpack's staging folder")
			err = s.forgetStage(ctx, stage.ID)
		default:
			log.Warn("Could not remove an interrupted unpack's staging folder; trying again at the next start")
			stage.Attempts++
			err = s.recordStage(ctx, stage)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// removeRecorded removes a recorded staging folder through a helper. A data
// folder the server no longer offers leaves nothing it can reach.
func (s *Service) removeRecorded(ctx context.Context, target Target, stage Stage) error {
	for _, root := range target.Roots {
		if root.Path == stage.Root {
			return s.removeStage(ctx, target, root, stage.Archive, stage.ID)
		}
	}
	return nil
}

// removeStage removes the staging folder named for id beside archive.
func (s *Service) removeStage(ctx context.Context, target Target, root Root, archive, id string) error {
	access, err := s.Acquire(ctx, target, root, false, nil)
	if err != nil {
		return err
	}
	_, err = access.request(ctx, helper.FileRequest{Operation: "unpack-cleanup", Root: root.Path, Path: archive, UploadID: id}, nil, nil)
	access.Cleanup(err != nil)
	return err
}
