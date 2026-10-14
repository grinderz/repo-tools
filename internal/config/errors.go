package config

import "errors"

// The fixed part of each validation message, wrapped with %w where it sits in
// the sentence: the text reads as before, and errors.Is can tell the problems
// apart.
var (
	errNoProjects            = errors.New("no projects defined")
	errRequired              = errors.New("is required")
	errDuplicateName         = errors.New("duplicate name")
	errNotAMapping           = errors.New("the document is not a mapping of keys")
	errKeyTwice              = errors.New("already defined")
	errNotOneOf              = errors.New("is not one of")
	errReportEntry           = errors.New("needs both column and value")
	errNoProductVersion      = errors.New("product_version is not set")
	errNotDepsKind           = errors.New("is not a cmds.deps kind")
	errMovedTo               = errors.New("moved to")
	errBadEnvName            = errors.New("is not an environment variable name")
	errRenovateReserved      = errors.New("is set by deps renovate itself")
	errRenovateRulesNotAList = errors.New("must be a list of rules")
	errBadToolchainKey       = errors.New("is not a toolchain key (letters, digits, underscores, lowercase)")
	errNoNextMinor           = errors.New("has no minor number to step to the next one")
	errNotAVersion           = errors.New("is not a version (one word, not latest)")
	errNoToolchainVersion    = errors.New("has no version in toolchain")
	errBadPin                = errors.New("needs file, match and value")
	errExtendsCycle          = errors.New("extends cycle")
	errBadRenovateUpdate     = errors.New("is not an update level (patch, minor, major)")
	errBadRenovateGroup      = errors.New("is not a grouping (all, separate)")
	errBadRenovateInternal   = errors.New("is not an internal mode (dev, always, never)")
	errExtendsNotAPath       = errors.New("is not a file path")
	errPinGroup              = errors.New("needs exactly one capture group, the version")
	errPinFields             = errors.New("file and var are both required")
	errFlowName              = errors.New("flows: a flow needs a name")
	errNoFlowCommands        = errors.New("lists no commands")
	errEmptyFlowCommand      = errors.New("has an empty command")
	errNoDepsKinds           = errors.New("but cmds.deps defines no kinds")
	errNotInDepsCmds         = errors.New("is not in cmds.deps")
	errNoChangelogCmds       = errors.New("changelog is enabled but no cmds.changelog are set")
	errBadPrefix             = errors.New("does not start with")
	errNoProjectDir          = errors.New("no project_dir and no global projects_dir")
)
