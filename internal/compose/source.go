// Package compose updates Compose-managed servers through `docker compose`.
package compose

import "github.com/Crunging/Ludock/internal/games"

// Labels that locate a Compose project.
const (
	SourceLabel         = games.ComposeSourceLabel
	WorkingDirLabel     = games.ComposeWorkingDirLabel
	ConfigFilesLabel    = games.ComposeConfigFilesLabel
	EnvironmentFilesLbl = games.ComposeEnvFilesLabel
)
