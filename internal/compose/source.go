// Package compose updates Compose-managed servers through `docker compose`.
package compose

// Compose records temporary snapshot paths after recreation. Ludock carries
// the original source on the replacement so later updates survive cleanup and
// restarts. Like Docker's labels, this is untrusted input, never permission
// to read a path.
const (
	SourceLabel         = "ludock.compose.source"
	WorkingDirLabel     = "com.docker.compose.project.working_dir"
	ConfigFilesLabel    = "com.docker.compose.project.config_files"
	EnvironmentFilesLbl = "com.docker.compose.project.environment_file"
)

// SourceLabels keeps the labels that locate a Compose project.
func SourceLabels(labels map[string]string) map[string]string {
	result := map[string]string{}
	for _, key := range []string{SourceLabel, WorkingDirLabel, ConfigFilesLabel, EnvironmentFilesLbl} {
		if value, ok := labels[key]; ok {
			result[key] = value
		}
	}
	return result
}
