package gitx

import "errors"

var (
	errRevListOutput  = errors.New("unexpected rev-list output")
	errNoSubmoduleURL = errors.New("has no url in .gitmodules")
	errNoSubmodule    = errors.New("not found in .gitmodules")
)
