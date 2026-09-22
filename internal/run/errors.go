package run

import "errors"

var (
	errUnknownProject = errors.New("is not in the config")
	errNoProjects     = errors.New("no projects selected")
	errYesAndConfirm  = errors.New("--yes and --confirm are mutually exclusive")
	errStopped        = errors.New("stopped at")
	errFailedProjects = errors.New("failed projects")
)
