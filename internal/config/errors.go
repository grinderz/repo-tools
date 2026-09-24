package config

import "errors"

// The fixed part of each validation message, wrapped with %w where it sits in
// the sentence: the text reads as before, and errors.Is can tell the problems
// apart.
var (
	errNoProjects       = errors.New("no projects defined")
	errRequired         = errors.New("is required")
	errDuplicateName    = errors.New("duplicate name")
	errNotOneOf         = errors.New("is not one of")
	errReportEntry      = errors.New("needs both column and value")
	errNoProductVersion = errors.New("product_version is not set")
	errNotDepsKind      = errors.New("is not a cmds.deps kind")
	errMovedTo          = errors.New("moved to")
	errPinFields        = errors.New("file and var are both required")
	errFlowName         = errors.New("flows: a flow needs a name")
	errNoFlowCommands   = errors.New("lists no commands")
	errEmptyFlowCommand = errors.New("has an empty command")
	errNoDepsKinds      = errors.New("but cmds.deps defines no kinds")
	errNotInDepsCmds    = errors.New("is not in cmds.deps")
	errNoChangelogCmds  = errors.New("changelog is enabled but no cmds.changelog are set")
	errBadPrefix        = errors.New("does not start with")
	errNoProjectDir     = errors.New("no project_dir and no global projects_dir")
)
