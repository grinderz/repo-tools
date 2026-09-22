// Package config loads and validates the repo-tools YAML config.
package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Confirm modes.
const (
	ConfirmDestructive = "destructive"
	ConfirmAlways      = "always"
	ConfirmNever       = "never"
)

// DepsNone is the deps kind that runs no dependency commands at all.
const DepsNone = "none"

// CI kinds: which system's pipelines to watch after a push, if any.
const (
	CINone   = "none"
	CIGitLab = "gitlab"
	CIGitHub = "github"
)

// FreezeToProject is the freeze_to value meaning "whatever branch the project
// that provides this submodule works on in this config": its release_branch
// when the config names one, its dev branch otherwise. It is also what a
// submodule gets when a project lists none, so the branch is written once, in
// the definition of the project it belongs to.
const FreezeToProject = "project"

// Defaults applied to projects that leave a field empty.
const (
	defaultDevBranch           = "develop"
	defaultReleaseBranchPrefix = "release-"
	// defaultPager keeps colours (-R) and nothing else: no -F, so even a short
	// diff opens and can be scrolled instead of flashing past into the prompt,
	// and no -X, so it opens on the alternate screen and quitting brings the
	// terminal back to the plan — the way a package manager shows a diff.
	defaultPager = "less -R"
	// defaultDiffLines caps a diff that is printed rather than paged: without
	// a terminal there is nothing to scroll, and a log should not swallow a
	// regenerated changelog whole.
	defaultDiffLines = 200
)

// Pipeline watch defaults: ask every ten seconds, give a pipeline half an
// hour to finish before calling the wait itself a failure, and give a failed
// one a single automatic second chance.
const (
	defaultCIPoll    = 10 * time.Second
	defaultCIWait    = 30 * time.Minute
	defaultCIRetries = 1
)

type Submodule struct {
	Path     string `yaml:"path"`
	FreezeTo string `yaml:"freeze_to"` // "release" or an explicit branch name
}

// Defaults holds the per-project settings that are usually the same
// everywhere. A project field that is set wins over its default here, which in
// turn wins over the built-in value.
type Defaults struct {
	Deps                string `yaml:"deps"`
	CI                  string `yaml:"ci"`
	DevBranch           string `yaml:"dev_branch"`
	ChangelogBranch     string `yaml:"changelog_branch"`
	Changelog           *bool  `yaml:"changelog"`
	DepsFreeze          *bool  `yaml:"deps_freeze"`
	ReleaseTags         *bool  `yaml:"release_tags"`
	Fetch               *bool  `yaml:"fetch"`
	ReleaseBranchPrefix string `yaml:"release_branch_prefix"`
}

type Project struct {
	Name string `yaml:"name"`
	Git  string `yaml:"git"`
	Deps string `yaml:"deps"`
	// CI names the system whose pipeline every push of a commit or a tag is
	// watched on afterwards: gitlab (through glab), github (through gh), or
	// none. Default none — the watch is opt-in; --no-ci skips it for one run.
	CI                  string            `yaml:"ci"`
	DevBranch           string            `yaml:"dev_branch"`
	ChangelogBranch     string            `yaml:"changelog_branch"` // empty = dev_branch
	Changelog           *bool             `yaml:"changelog"`
	ChangelogCmds       []string          `yaml:"changelog_cmds"`  // replaces the global list
	ChangelogEnv        map[string]string `yaml:"changelog_env"`   // merged over the global map
	DepsCmds            []string          `yaml:"deps_cmds"`       // replaces the list of its deps kind
	DepsCheckCmds       []string          `yaml:"deps_check_cmds"` // replaces the list of its deps kind
	DepsFreeze          *bool             `yaml:"deps_freeze"`
	ReleaseBranchPrefix string            `yaml:"release_branch_prefix"`
	// Fetch allows commands to refresh this project's remote refs. Set it to
	// false for a repository whose remote costs something to reach — another
	// credential, another hardware key — and every command will work with the
	// refs already on disk unless --fetch says otherwise.
	Fetch *bool `yaml:"fetch"`
	// ReleaseTags allows release rc and release tag to tag this project.
	// Unlike the other toggles it defaults to on: a library that is consumed
	// by branch rather than by version sets it to false.
	ReleaseTags *bool `yaml:"release_tags"`
	// ReleaseBranch names the release branch this config works with, for a
	// config that describes one release. It need not exist yet: release branch
	// creates exactly it. Empty means "the highest release branch on origin".
	ReleaseBranch string      `yaml:"release_branch"`
	Submodules    []Submodule `yaml:"submodules"`
	ProjectDir    string      `yaml:"project_dir"` // wins over projects_dir

	dir string // resolved absolute path
}

// ReportColumn is one column of repo report: the header cell and the value
// template, expanded per project. The dev config reports commits, a release
// config reports tags — same command, different table.
type ReportColumn struct {
	Column string `yaml:"column"`
	Value  string `yaml:"value"`
}

// DepsPin locates the internal dependency refs of one deps kind: the file that
// holds them and the variable listing them as module@ref. For go that is the
// GO_DEPS_UPDATE_INTERNAL of the project's Makefile, which
// make deps.update.internal walks.
type DepsPin struct {
	File string `yaml:"file"`
	Var  string `yaml:"var"`
}

type Config struct {
	ProjectsDir string `yaml:"projects_dir"`
	// ProductVersion names the release the whole set of projects belongs to,
	// e.g. "2026.06". It is available to every template as {product_version};
	// the per-project tags stay independent of it.
	ProductVersion string `yaml:"product_version"`

	RcTagMessage string `yaml:"rc_tag_message"`
	Confirm      string `yaml:"confirm"`

	// ChangelogCmds run in order in the project directory, each one
	// redirecting to the file it generates.
	ChangelogCmds []string          `yaml:"changelog_cmds"`
	ChangelogEnv  map[string]string `yaml:"changelog_env"`
	// ChangelogInitSubmodules checks submodules out before generating, which a
	// git-cliff config living in a submodule needs. Defaults to true.
	ChangelogInitSubmodules *bool `yaml:"changelog_init_submodules"`

	// DepsCmds holds the dependency commands of every deps kind, keyed by the
	// name a project puts in its deps field. The kinds are whatever the config
	// defines: nothing about go or uv is built in.
	DepsCmds map[string][]string `yaml:"deps_cmds"`

	// DepsPins says where a deps kind keeps the refs of its internal
	// dependencies when they are not submodules, keyed the same way as
	// DepsCmds. Go projects list them as module@ref in a make variable, and a
	// freeze has to move those refs too or the deps commands would resolve dev
	// heads on the release branch.
	DepsPins map[string]DepsPin `yaml:"deps_pins"`

	// DepsCheckCmds holds, per deps kind, the read-only commands deps check
	// runs to tell whether the dependency files are current — commands that
	// exit non-zero when running the real ones would change something, like
	// "go mod tidy -diff" or "uv lock --check".
	DepsCheckCmds map[string][]string `yaml:"deps_check_cmds"`

	// Disable refuses commands this config has no business running, by their
	// name under rt ("deps freeze", "release tag"). A group name disables
	// everything under it. A dev config uses it to keep release-only work out.
	Disable []string `yaml:"disable"`

	// Report is the table repo report prints, one entry per column. Empty
	// means the built-in project/branch/commit table.
	Report []ReportColumn `yaml:"report"`

	// Flows are named sequences of rt commands, run in order over one project
	// list by rt flow <name>. A step is a command name the way Disable writes
	// it ("repo sync"); the flow stops at the first failure or the first
	// declined confirmation, so a release is one command instead of six.
	Flows map[string][]string `yaml:"flows"`

	// Diff prints the staged diff and asks before every commit, the same
	// switch as --diff/--no-diff. Defaults to true.
	Diff *bool `yaml:"diff"`

	// Pager shows the review bodies on a terminal, the way a package manager
	// shows a diff. Unset means $PAGER, then less; an empty string turns it
	// off and falls back to printing with the DiffLines cap.
	Pager *string `yaml:"pager"`

	// DiffLines caps how much of a diff the review prints. 0 means no cap,
	// which a changelog commit usually wants; unset means the built-in 200.
	DiffLines *int `yaml:"diff_lines"`

	// CIPollSeconds is how often the pipeline watch asks again; unset means
	// the built-in 10.
	CIPollSeconds *int `yaml:"ci_poll_seconds"`
	// CIWaitMinutes is how long the watch waits for a pipeline to finish
	// before giving up with an error; unset means the built-in 30.
	CIWaitMinutes *int `yaml:"ci_wait_minutes"`
	// CIRetries is how many times a failed pipeline is retried — failed jobs
	// only, the way the retry button works — before the failure is final.
	// Unset means the built-in 1; 0 makes a failure final at once.
	CIRetries *int `yaml:"ci_retries"`

	// Color paints diffs, warnings and errors, the same switch as
	// --color/--no-color. Unset means colour when a terminal is attached.
	Color *bool `yaml:"color"`

	// DeriveSubmodules lets a project leave its submodules list out: every
	// submodule whose url belongs to another configured project is then frozen
	// to that project's branch. Defaults to true.
	DeriveSubmodules *bool `yaml:"derive_submodules"`

	// Direnv runs the project commands through "direnv exec" when the project
	// has an .envrc, so its own GOPROXY, GOPRIVATE or index URLs apply.
	// Defaults to true; it does nothing where direnv or .envrc is absent.
	Direnv *bool `yaml:"direnv"`

	ChangelogCommitMessage  string `yaml:"changelog_commit_message"`
	FreezeCommitMessage     string `yaml:"freeze_commit_message"`
	SubmodulesCommitMessage string `yaml:"submodules_commit_message"`
	// RebasePinCommitMessage is the commit git rebase --submodules adds when
	// a freshly pushed submodule head still has to be pinned by hand — the
	// pins no rebase conflict already refreshed.
	RebasePinCommitMessage string `yaml:"rebase_pin_commit_message"`
	ReleaseTagMessage      string `yaml:"release_tag_message"`

	// NotesHeader and NotesSectionTitle shape the release notes document:
	// the first line of the whole document, and the heading of each
	// project's section ({tag} is the tag the section describes). Unset
	// keeps the built-in headings.
	NotesHeader       string `yaml:"notes_header"`
	NotesSectionTitle string `yaml:"notes_section_title"`

	Defaults Defaults   `yaml:"defaults"`
	Projects []*Project `yaml:"projects"`
}

// ChangelogCommands returns the generator commands for this project: its own
// list when it has one, the global list otherwise.
func (c *Config) ChangelogCommands(p *Project) []string {
	if len(p.ChangelogCmds) > 0 {
		return p.ChangelogCmds
	}

	return c.ChangelogCmds
}

// DepsCommands returns the dependency commands to run for this project: its
// own list when it has one, otherwise the list its deps kind names.
func (c *Config) DepsCommands(p *Project) []string {
	if len(p.DepsCmds) > 0 {
		return p.DepsCmds
	}

	if p.Deps == DepsNone {
		return nil
	}

	return c.DepsCmds[p.Deps]
}

// DepsCheckCommands returns the freshness checks to run for this project: its
// own list when it has one, otherwise the list its deps kind names.
func (c *Config) DepsCheckCommands(p *Project) []string {
	if len(p.DepsCheckCmds) > 0 {
		return p.DepsCheckCmds
	}

	if p.Deps == DepsNone {
		return nil
	}

	return c.DepsCheckCmds[p.Deps]
}

// DepsKinds lists the configured deps kinds, sorted, for error messages.
func (c *Config) DepsKinds() []string {
	kinds := slices.Sorted(maps.Keys(c.DepsCmds))

	return kinds
}

// ChangelogEnvironment merges the project's environment over the global one.
func (c *Config) ChangelogEnvironment(p *Project) map[string]string {
	env := make(map[string]string, len(c.ChangelogEnv)+len(p.ChangelogEnv))
	maps.Copy(env, c.ChangelogEnv)
	maps.Copy(env, p.ChangelogEnv)

	return env
}

// DeriveSubmodulesEnabled reports whether a project without a submodules list
// gets one from the other projects in the config.
func (c *Config) DeriveSubmodulesEnabled() bool {
	return c.DeriveSubmodules == nil || *c.DeriveSubmodules
}

// ProjectByGit finds the project a git url belongs to, which is how a
// submodule is recognised as one of the configured projects. Matching is by
// remote, not by path or by .gitmodules name: the same library sits at a
// different path in different projects, and its section name is local to each
// .gitmodules.
func (c *Config) ProjectByGit(url string) *Project {
	for _, p := range c.Projects {
		if SameRemote(p.Git, url) {
			return p
		}
	}

	return nil
}

// SameRemote reports whether two git urls name the same repository. The same
// remote is written in more than one way — ssh or https, with or without the
// .git suffix — and a config that says one of them should still recognise the
// other in a .gitmodules.
func SameRemote(a, b string) bool {
	return normalizeRemote(a) == normalizeRemote(b)
}

func normalizeRemote(url string) string {
	out := strings.ToLower(strings.TrimSpace(url))

	for _, scheme := range []string{"ssh://", "git://", "https://", "http://"} {
		out = strings.TrimPrefix(out, scheme)
	}

	// user@host:path and user@host/path are the same place.
	if _, rest, ok := strings.Cut(out, "@"); ok {
		out = rest
	}

	out = strings.Replace(out, ":", "/", 1)
	out = strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(out, "/"), ".git"), "/")

	return out
}

// DepsPinsFor is where this project keeps the refs of its internal
// dependencies, if its deps kind names such a place at all.
func (c *Config) DepsPinsFor(p *Project) (DepsPin, bool) {
	pin, ok := c.DepsPins[p.Deps]

	return pin, ok
}

// RebasePinMessage is the commit message of the re-pin commit git rebase
// --submodules makes; an accessor rather than a Load-time default, so a Ctx
// built without Load still commits with something sensible.
func (c *Config) RebasePinMessage() string {
	if c.RebasePinCommitMessage != "" {
		return c.RebasePinCommitMessage
	}

	return "chore(deps): pin rebased submodule branches"
}

// PagerCommand is the pager to show reviews through, empty for none.
func (c *Config) PagerCommand() string {
	if c.Pager != nil {
		return *c.Pager
	}

	if fromEnv := os.Getenv("PAGER"); fromEnv != "" {
		return fromEnv
	}

	return defaultPager
}

// DiffLimit is how many lines of a diff the review prints, 0 for all of them.
func (c *Config) DiffLimit() int {
	if c.DiffLines == nil {
		return defaultDiffLines
	}

	if *c.DiffLines < 0 {
		return 0
	}

	return *c.DiffLines
}

// CIRetryLimit is how many times a failed pipeline gets another chance.
func (c *Config) CIRetryLimit() int {
	switch {
	case c.CIRetries == nil:
		return defaultCIRetries
	case *c.CIRetries < 0:
		return 0
	}

	return *c.CIRetries
}

// CIPoll is the pause between two pipeline queries.
func (c *Config) CIPoll() time.Duration {
	if c.CIPollSeconds != nil && *c.CIPollSeconds > 0 {
		return time.Duration(*c.CIPollSeconds) * time.Second
	}

	return defaultCIPoll
}

// CIWait is how long a pipeline gets to finish before the watch gives up.
func (c *Config) CIWait() time.Duration {
	if c.CIWaitMinutes != nil && *c.CIWaitMinutes > 0 {
		return time.Duration(*c.CIWaitMinutes) * time.Minute
	}

	return defaultCIWait
}

// DirenvEnabled reports whether project commands go through direnv.
func (c *Config) DirenvEnabled() bool { return c.Direnv == nil || *c.Direnv }

// DiffEnabled reports whether commits show their diff and ask first.
func (c *Config) DiffEnabled() bool {
	return c.Diff == nil || *c.Diff
}

// InitSubmodulesForChangelog reports whether changelog update checks submodules out.
func (c *Config) InitSubmodulesForChangelog() bool {
	return c.ChangelogInitSubmodules == nil || *c.ChangelogInitSubmodules
}

func (p *Project) Dir() string { return p.dir }

// ChangelogEnabled reports whether changelog update should run for this project.
func (p *Project) ChangelogEnabled() bool { return p.Changelog != nil && *p.Changelog }

// DepsFreezeEnabled reports whether deps freeze should run for this project.
func (p *Project) DepsFreezeEnabled() bool { return p.DepsFreeze != nil && *p.DepsFreeze }

// FetchAllowed reports whether commands may fetch this project. Unset means
// yes: the command's own default then decides.
func (p *Project) FetchAllowed() bool { return p.Fetch == nil || *p.Fetch }

// ReleaseTagsEnabled reports whether the two tagging commands, release rc and
// release tag, cover this project. Unset means yes, which most projects want.
func (p *Project) ReleaseTagsEnabled() bool { return p.ReleaseTags == nil || *p.ReleaseTags }

// TargetBranch is the branch this project works on in this config: the release
// branch when the config names one, its dev branch otherwise.
func (p *Project) TargetBranch() string {
	if p.ReleaseBranch != "" {
		return p.ReleaseBranch
	}

	return p.DevBranch
}

func (p *Project) ChangelogBranchOrDev() string {
	if p.ChangelogBranch != "" {
		return p.ChangelogBranch
	}

	return p.DevBranch
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w%s", path, err, quotingHint(err))
	}

	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	if len(cfg.Projects) == 0 {
		return nil, fmt.Errorf("%s: %w", path, errNoProjects)
	}

	seen := make(map[string]bool, len(cfg.Projects))

	for i, p := range cfg.Projects {
		if p.Name == "" {
			return nil, fmt.Errorf("projects[%d]: name %w", i, errRequired)
		}

		if seen[p.Name] {
			return nil, fmt.Errorf("projects: %w %q", errDuplicateName, p.Name)
		}

		seen[p.Name] = true

		if err := cfg.prepareProject(p); err != nil {
			return nil, err
		}
	}

	return &cfg, nil
}

// quotingHint explains the one YAML error this config invites: a message
// template like "chore(deps): update ..." is a mapping to the parser unless it
// is quoted.
func quotingHint(err error) string {
	if !strings.Contains(err.Error(), "mapping values are not allowed") {
		return ""
	}

	return "\n  (a value containing \": \" has to be quoted, e.g. " +
		`submodules_commit_message: "chore(deps): update submodules on {branch}")`
}

func (c *Config) applyDefaults() error {
	// Asking about everything is the safe default; a config that knows better
	// says so, and CI says never.
	if c.Confirm == "" {
		c.Confirm = ConfirmAlways
	}

	switch c.Confirm {
	case ConfirmDestructive, ConfirmAlways, ConfirmNever:
	default:
		return fmt.Errorf("confirm: %q %w destructive|always|never", c.Confirm, errNotOneOf)
	}

	if c.RcTagMessage == "" {
		c.RcTagMessage = "release candidate {version}"
	}

	if c.ReleaseTagMessage == "" {
		c.ReleaseTagMessage = "release {version}"
	}

	// [skip ci] keeps the changelog commit out of the next changelog and off
	// the CI pipeline, the way the CI job writes it.
	if c.ChangelogCommitMessage == "" {
		c.ChangelogCommitMessage = "chore(changelog): update changelog [skip ci]"
	}

	if c.FreezeCommitMessage == "" {
		c.FreezeCommitMessage = "chore(deps): freeze dependencies for {branch}"
	}

	if c.SubmodulesCommitMessage == "" {
		c.SubmodulesCommitMessage = "chore(deps): update submodules on {branch}"
	}

	if err := c.validateDeps(c.Defaults.Deps, "defaults"); err != nil {
		return err
	}

	if err := c.validateDepsPins(); err != nil {
		return err
	}

	if err := c.validateDepsCheckCmds(); err != nil {
		return err
	}

	if err := c.validateProductVersion(); err != nil {
		return err
	}

	if err := c.validateFlows(); err != nil {
		return err
	}

	for i, col := range c.Report {
		if col.Column == "" || col.Value == "" {
			return fmt.Errorf("report: entry %d %w", i+1, errReportEntry)
		}
	}

	c.ProjectsDir = expandHome(c.ProjectsDir)

	return nil
}

// productVersionVar is how a template asks for the product version.
const productVersionVar = "{product_version}"

// validateProductVersion refuses a template that asks for a product version
// the config does not set: the alternative is a tag message with a literal
// {product_version} in it, discovered after the tag is pushed.
func (c *Config) validateProductVersion() error {
	if c.ProductVersion != "" {
		return nil
	}

	templates := map[string][]string{
		"rc_tag_message":            {c.RcTagMessage},
		"release_tag_message":       {c.ReleaseTagMessage},
		"changelog_commit_message":  {c.ChangelogCommitMessage},
		"freeze_commit_message":     {c.FreezeCommitMessage},
		"submodules_commit_message": {c.SubmodulesCommitMessage},
		"rebase_pin_commit_message": {c.RebasePinCommitMessage},
		"notes_header":              {c.NotesHeader},
		"notes_section_title":       {c.NotesSectionTitle},
		"changelog_cmds":            c.ChangelogCmds,
		"changelog_env":             slices.Sorted(maps.Values(c.ChangelogEnv)),
	}

	for _, name := range slices.Sorted(maps.Keys(templates)) {
		for _, value := range templates[name] {
			if strings.Contains(value, productVersionVar) {
				return fmt.Errorf("%s uses %s but %w", name, productVersionVar, errNoProductVersion)
			}
		}
	}

	return nil
}

// validateDepsPins keeps deps_pins keyed by kinds that exist: a pin under a
// misspelled kind would silently freeze nothing, which is exactly the failure
// the release branch cannot afford.
func (c *Config) validateDepsPins() error {
	for _, kind := range slices.Sorted(maps.Keys(c.DepsPins)) {
		pin := c.DepsPins[kind]

		if _, ok := c.DepsCmds[kind]; !ok {
			return fmt.Errorf("deps_pins: %q %w (defined: %s)",
				kind, errNotDepsKind, strings.Join(c.DepsKinds(), ", "))
		}

		if pin.File == "" || pin.Var == "" {
			return fmt.Errorf("deps_pins: %s: %w", kind, errPinFields)
		}
	}

	return nil
}

// validateDepsCheckCmds keeps deps_check_cmds keyed by kinds that exist: a
// check under a misspelled kind would silently never run.
func (c *Config) validateDepsCheckCmds() error {
	for _, kind := range slices.Sorted(maps.Keys(c.DepsCheckCmds)) {
		if _, ok := c.DepsCmds[kind]; !ok {
			return fmt.Errorf("deps_check_cmds: %q %w (defined: %s)",
				kind, errNotDepsKind, strings.Join(c.DepsKinds(), ", "))
		}
	}

	return nil
}

// validateFlows refuses an empty flow and an empty step: both would make
// rt flow quietly do less than the config promises. Whether each step names a
// real command is checked against the actual command tree when the flow runs,
// the same way the disable list is.
func (c *Config) validateFlows() error {
	for _, name := range slices.Sorted(maps.Keys(c.Flows)) {
		if strings.TrimSpace(name) == "" {
			return errFlowName
		}

		steps := c.Flows[name]
		if len(steps) == 0 {
			return fmt.Errorf("flows: %s %w", name, errNoFlowCommands)
		}

		for _, step := range steps {
			if strings.TrimSpace(step) == "" {
				return fmt.Errorf("flows: %s %w", name, errEmptyFlowCommand)
			}
		}
	}

	return nil
}

// validateDeps accepts an empty value: the caller falls back to a default.
// Every other kind has to be one deps_cmds defines, so a typo is caught here
// instead of silently running no dependency command at all.
func (c *Config) validateDeps(deps, where string) error {
	if deps == "" || deps == DepsNone {
		return nil
	}

	if _, ok := c.DepsCmds[deps]; ok {
		return nil
	}

	if len(c.DepsCmds) == 0 {
		return fmt.Errorf("%s: deps: %q %w", where, deps, errNoDepsKinds)
	}

	return fmt.Errorf("%s: deps: %q %w (defined: %s)",
		where, deps, errNotInDepsCmds, strings.Join(c.DepsKinds(), ", "))
}

// applyProjectDefaults resolves every unset project field against the defaults
// block and, failing that, the built-in value.
func (c *Config) applyProjectDefaults(p *Project) {
	def := c.Defaults

	p.DevBranch = firstNonEmpty(p.DevBranch, def.DevBranch, defaultDevBranch)
	p.ReleaseBranchPrefix = firstNonEmpty(p.ReleaseBranchPrefix, def.ReleaseBranchPrefix, defaultReleaseBranchPrefix)
	p.Deps = firstNonEmpty(p.Deps, def.Deps, DepsNone)
	p.CI = firstNonEmpty(p.CI, def.CI, CINone)
	p.ChangelogBranch = firstNonEmpty(p.ChangelogBranch, def.ChangelogBranch)

	if p.Changelog == nil {
		p.Changelog = def.Changelog
	}

	if p.DepsFreeze == nil {
		p.DepsFreeze = def.DepsFreeze
	}

	if p.ReleaseTags == nil {
		p.ReleaseTags = def.ReleaseTags
	}

	if p.Fetch == nil {
		p.Fetch = def.Fetch
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// prepareProject fills in per-project defaults and resolves its directory.
func (c *Config) prepareProject(p *Project) error {
	if p.Git == "" {
		return fmt.Errorf("project %s: git url %w", p.Name, errRequired)
	}

	c.applyProjectDefaults(p)

	// A project with its own deps_cmds names the commands outright, so its
	// deps kind no longer has to resolve to anything.
	if len(p.DepsCmds) == 0 {
		if err := c.validateDeps(p.Deps, "project "+p.Name); err != nil {
			return err
		}
	}

	switch p.CI {
	case CINone, CIGitLab, CIGitHub:
	default:
		return fmt.Errorf("project %s: ci: %q %w %s, %s, %s",
			p.Name, p.CI, errNotOneOf, CINone, CIGitLab, CIGitHub)
	}

	if p.ChangelogEnabled() && len(c.ChangelogCommands(p)) == 0 {
		return fmt.Errorf("project %s: %w", p.Name, errNoChangelogCmds)
	}

	// The branch itself may not exist yet, but its name has to be the shape the
	// version arithmetic reads, or rc and tag would have nothing to count from.
	if p.ReleaseBranch != "" && !strings.HasPrefix(p.ReleaseBranch, p.ReleaseBranchPrefix) {
		return fmt.Errorf("project %s: release_branch %q %w %q",
			p.Name, p.ReleaseBranch, errBadPrefix, p.ReleaseBranchPrefix)
	}

	for _, s := range p.Submodules {
		if s.Path == "" {
			return fmt.Errorf("project %s: submodule path %w", p.Name, errRequired)
		}

		if s.FreezeTo == "" {
			return fmt.Errorf("project %s: submodule %s: freeze_to %w", p.Name, s.Path, errRequired)
		}
	}

	switch {
	case p.ProjectDir != "":
		p.dir = expandHome(p.ProjectDir)
	case c.ProjectsDir != "":
		p.dir = filepath.Join(c.ProjectsDir, p.Name)
	default:
		return fmt.Errorf("project %s: %w", p.Name, errNoProjectDir)
	}

	return nil
}
