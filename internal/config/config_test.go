package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

const minimal = `
projects_dir: /srv/projects
projects:
  - name: api
    git: git@example.com:api.git
`

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Confirm != ConfirmAlways {
		t.Errorf("confirm = %q, want %q", cfg.Confirm, ConfirmAlways)
	}

	p := cfg.Projects[0]
	if p.DevBranch != "develop" {
		t.Errorf("dev_branch = %q, want develop", p.DevBranch)
	}

	if p.ReleaseBranchPrefix != "release-" {
		t.Errorf("release_branch_prefix = %q, want release-", p.ReleaseBranchPrefix)
	}

	if p.Deps != DepsNone {
		t.Errorf("deps = %q, want none", p.Deps)
	}

	if want := filepath.Join("/srv/projects", "api"); p.Dir() != want {
		t.Errorf("dir = %q, want %q", p.Dir(), want)
	}

	if cfg.RcTagMessage == "" || cfg.ReleaseTagMessage == "" {
		t.Error("tag message defaults are empty")
	}
}

func TestChangelogBranchFallsBackToDev(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.Projects[0].ChangelogBranchOrDev(); got != "develop" {
		t.Errorf("got %q, want develop", got)
	}

	cfg.Projects[0].ChangelogBranch = "main"
	if got := cfg.Projects[0].ChangelogBranchOrDev(); got != "main" {
		t.Errorf("got %q, want main", got)
	}
}

func TestProjectDirWinsOverGlobal(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /srv/projects
projects:
  - name: api
    git: git@example.com:api.git
    project_dir: /elsewhere/api
`))
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.Projects[0].Dir(); got != "/elsewhere/api" {
		t.Errorf("dir = %q, want /elsewhere/api", got)
	}
}

const withDefaults = `
projects_dir: /srv/projects
cmds:
  changelog:
    - "git-cliff > CHANGELOG-cliff.md"
  deps:
    uv:
      - "uv sync"
    go:
      - "make deps.update.internal"
defaults:
  dev_branch: main
  release_branch_prefix: rel/
  deps: uv
  changelog: true
  deps_freeze: true
projects:
  - name: inherits
    git: git@example.com:inherits.git
  - name: overrides
    git: git@example.com:overrides.git
    dev_branch: develop
    release_branch_prefix: release-
    deps: go
    changelog: false
    deps_freeze: false
`

func TestDefaultsApplyToProjectsThatOmitFields(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, withDefaults))
	if err != nil {
		t.Fatal(err)
	}

	p := cfg.Projects[0]
	if p.DevBranch != "main" {
		t.Errorf("dev_branch = %q, want main", p.DevBranch)
	}

	if p.ReleaseBranchPrefix != "rel/" {
		t.Errorf("release_branch_prefix = %q, want rel/", p.ReleaseBranchPrefix)
	}

	if p.Deps != "uv" {
		t.Errorf("deps = %q, want uv", p.Deps)
	}

	if !p.ChangelogEnabled() || !p.DepsFreezeEnabled() {
		t.Error("changelog and deps_freeze should be inherited as enabled")
	}
}

func TestProjectFieldsWinOverDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, withDefaults))
	if err != nil {
		t.Fatal(err)
	}

	p := cfg.Projects[1]
	if p.DevBranch != "develop" {
		t.Errorf("dev_branch = %q, want develop", p.DevBranch)
	}

	if p.ReleaseBranchPrefix != "release-" {
		t.Errorf("release_branch_prefix = %q, want release-", p.ReleaseBranchPrefix)
	}

	if p.Deps != "go" {
		t.Errorf("deps = %q, want go", p.Deps)
	}
	// An explicit false must survive a default of true.
	if p.ChangelogEnabled() || p.DepsFreezeEnabled() {
		t.Error("explicit false should override a default of true")
	}
}

func TestBuiltinDefaultsWhenNoDefaultsBlock(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}

	p := cfg.Projects[0]
	if p.DevBranch != "develop" || p.ReleaseBranchPrefix != "release-" || p.Deps != DepsNone {
		t.Errorf("built-in defaults not applied: %+v", p)
	}

	if p.ChangelogEnabled() || p.DepsFreezeEnabled() {
		t.Error("changelog and deps_freeze should stay off unless enabled")
	}
}

const withChangelog = `
projects_dir: /srv/projects
cmds:
  changelog:
    - "git-cliff > CHANGELOG-cliff.md"
    - "{rt} changelog gen > CHANGELOG-git.md"
changelog_env:
  GIT_CLIFF_CONFIG: ".dev-include/config/cliff.toml"
  GIT_CLIFF__CHANGELOG__HEADER: "# Changelog of {project}"
defaults:
  changelog: true
projects:
  - name: inherits
    git: git@example.com:inherits.git
  - name: overrides
    git: git@example.com:overrides.git
    cmds:
      changelog:
        - "make changelog"
    changelog_env:
      GIT_CLIFF_CONFIG: "cliff.toml"
`

func TestChangelogCommandsAndEnvInheritance(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, withChangelog))
	if err != nil {
		t.Fatal(err)
	}

	inherits, overrides := cfg.Projects[0], cfg.Projects[1]

	if got := cfg.ChangelogCommands(inherits); len(got) != 2 {
		t.Errorf("global commands not inherited: %v", got)
	}

	// A project list replaces the global one instead of extending it.
	got := cfg.ChangelogCommands(overrides)
	if len(got) != 1 || got[0] != "make changelog" {
		t.Errorf("project commands did not replace the global list: %v", got)
	}

	env := cfg.ChangelogEnvironment(overrides)
	if env["GIT_CLIFF_CONFIG"] != "cliff.toml" {
		t.Errorf("project env did not win: %q", env["GIT_CLIFF_CONFIG"])
	}

	if env["GIT_CLIFF__CHANGELOG__HEADER"] == "" {
		t.Error("global env keys should survive the merge")
	}

	// The merge must not write back into the global map.
	if cfg.ChangelogEnv["GIT_CLIFF_CONFIG"] != ".dev-include/config/cliff.toml" {
		t.Errorf("global env was mutated: %q", cfg.ChangelogEnv["GIT_CLIFF_CONFIG"])
	}
}

func TestInitSubmodulesForChangelogDefaultsToTrue(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, withChangelog))
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.InitSubmodulesForChangelog() {
		t.Error("submodule checkout should be on unless disabled")
	}

	off := false
	cfg.ChangelogInitSubmodules = &off

	if cfg.InitSubmodulesForChangelog() {
		t.Error("changelog_init_submodules: false should turn it off")
	}
}

func TestSkipCiIsInTheDefaultChangelogMessage(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(cfg.ChangelogCommitMessage, "[skip ci]") {
		t.Errorf("default message %q lost [skip ci]", cfg.ChangelogCommitMessage)
	}
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body, wantErr string
	}{
		{"unknown confirm mode", `
projects_dir: /p
confirm: maybe
projects:
  - {name: a, git: g}
`, "confirm"},
		{"unknown deps kind", `
projects_dir: /p
projects:
  - {name: a, git: g, deps: cargo}
`, "deps"},
		{"duplicate project", `
projects_dir: /p
projects:
  - {name: a, git: g}
  - {name: a, git: g}
`, "duplicate"},
		{"missing git url", `
projects_dir: /p
projects:
  - {name: a}
`, "git url"},
		{"unknown ci kind", `
projects_dir: /p
projects:
  - {name: a, git: g, ci: jenkins}
`, "ci"},
		{"empty flow", `
projects_dir: /p
flows:
  rel: []
projects:
  - {name: a, git: g}
`, "rel lists no commands"},
		{"empty flow step", `
projects_dir: /p
flows:
  rel:
    - "repo sync"
    - ""
projects:
  - {name: a, git: g}
`, "rel has an empty command"},
		{"no projects", `projects_dir: /p`, "no projects"},
		{"changelog enabled without commands", `
projects_dir: /p
projects:
  - {name: a, git: g, changelog: true}
`, "no cmds.changelog"},
		{"unknown deps kind in defaults", `
projects_dir: /p
defaults:
  deps: cargo
projects:
  - {name: a, git: g}
`, "defaults: deps"},
		{"no dir anywhere", `
projects:
  - {name: a, git: g}
`, "projects_dir"},
		{"submodule without freeze_to", `
projects_dir: /p
projects:
  - name: a
    git: g
    submodules:
      - path: sub
`, "freeze_to"},
		{"unknown field", `
projects_dir: /p
projekts:
  - {name: a, git: g}
`, "field"},
		{"cmds.deps_check under a kind that does not exist", `
projects_dir: /p
cmds:
  deps_check:
    cargo: ["cargo check"]
projects:
  - {name: a, git: g}
`, "cmds.deps_check"},
		{"a pre-cmds key", `
projects_dir: /p
deps_cmds:
  uv: ["uv sync"]
projects:
  - {name: a, git: g}
`, "deps_cmds moved to cmds.deps"},
		{"a pre-cmds key on a project", `
projects_dir: /p
projects:
  - {name: a, git: g, changelog_cmds: ["make changelog"]}
`, "project a: changelog_cmds moved to cmds.changelog"},
	}
	for _, c := range cases {
		_, err := Load(write(t, c.body))
		if err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}

		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.wantErr)
		}
	}
}

// The dependency commands live in the config: a kind resolves to its list, a
// project may replace it, and deps: none runs nothing.
func TestDepsCommands(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /p
cmds:
  deps:
    uv:
      - "uv sync"
    go:
      - "make deps.update.internal"
projects:
  - {name: py, git: g, deps: uv}
  - {name: go, git: g, deps: go, cmds: {deps: ["go mod tidy"]}}
  - {name: plain, git: g}
`))
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.DepsCommands(cfg.Projects[0]); len(got) != 1 || got[0] != "uv sync" {
		t.Errorf("kind list: got %v", got)
	}

	if got := cfg.DepsCommands(cfg.Projects[1]); len(got) != 1 || got[0] != "go mod tidy" {
		t.Errorf("a project list must replace the kind's: %v", got)
	}

	if got := cfg.DepsCommands(cfg.Projects[2]); len(got) != 0 {
		t.Errorf("deps: none runs nothing: %v", got)
	}
}

// A deps kind with no commands anywhere is a typo, not a silent no-op.
func TestUnknownDepsKindIsRejected(t *testing.T) {
	t.Parallel()

	_, err := Load(write(t, `
projects_dir: /p
cmds:
  deps:
    uv:
      - "uv sync"
projects:
  - {name: a, git: g, deps: cargo}
`))
	if err == nil || !strings.Contains(err.Error(), "not in cmds.deps (defined: uv)") {
		t.Fatalf("err = %v", err)
	}

	// Its own command list makes the kind irrelevant.
	if _, err := Load(write(t, `
projects_dir: /p
projects:
  - {name: a, git: g, deps: cargo, cmds: {deps: ["cargo update"]}}
`)); err != nil {
		t.Errorf("a project that names its own commands should load: %v", err)
	}
}

// release_branch may name a branch that does not exist yet, but it has to be
// shaped like one, or the rc and tag arithmetic has nothing to read.
func TestReleaseBranchIsValidated(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /p
projects:
  - {name: a, git: g, release_branch: release-2026.06}
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Projects[0].ReleaseBranch != "release-2026.06" {
		t.Errorf("got %q", cfg.Projects[0].ReleaseBranch)
	}

	_, err = Load(write(t, `
projects_dir: /p
projects:
  - {name: a, git: g, release_branch: hotfix-1.2}
`))
	if err == nil || !strings.Contains(err.Error(), "does not start with") {
		t.Fatalf("err = %v", err)
	}
}

// release_tags is the one toggle that defaults to on: most projects are
// tagged, and a library consumed by branch opts out.
func TestReleaseTagsDefaultsToOn(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /p
defaults:
  release_tags: true
projects:
  - {name: tagged, git: g}
  - {name: untagged, git: g2, release_tags: false}
`))
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.Projects[0].ReleaseTagsEnabled() {
		t.Error("a project that says nothing is tagged")
	}

	if cfg.Projects[1].ReleaseTagsEnabled() {
		t.Error("an explicit false must survive the default of true")
	}

	// And with no defaults block at all it is still on.
	cfg, err = Load(write(t, `
projects_dir: /p
projects:
  - {name: a, git: g}
`))
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.Projects[0].ReleaseTagsEnabled() {
		t.Error("the built-in value is on")
	}
}

// product_version is optional, but a template that asks for one must have it:
// the alternative is a pushed tag reading "release 1.0.0 of {product_version}".
func TestProductVersionMustBeSetWhenUsed(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /p
product_version: "2026.06"
rc_tag_message: "rc {version} of {product_version}"
projects:
  - {name: a, git: g}
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ProductVersion != "2026.06" {
		t.Errorf("got %q", cfg.ProductVersion)
	}

	// Unset and unused is fine.
	if _, err := Load(write(t, `
projects_dir: /p
projects:
  - {name: a, git: g}
`)); err != nil {
		t.Errorf("a config without a product version should load: %v", err)
	}

	// Unset but used is a config error, wherever the placeholder sits.
	for _, body := range []string{
		`rc_tag_message: "rc {version} of {product_version}"`,
		`freeze_commit_message: "freeze {branch} for {product_version}"`,
		"cmds: {changelog: [\"echo {product_version} > v.txt\"]}",
	} {
		_, err := Load(write(t, "projects_dir: /p\n"+body+"\nprojects:\n  - {name: a, git: g, changelog: false}\n"))
		if err == nil || !strings.Contains(err.Error(), "product_version is not set") {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

// Asking about everything is the default: a run that changes nothing is cheap
// to confirm, an unwanted push is not.
func TestConfirmDefaultsToAlways(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /p
projects:
  - {name: a, git: g}
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Confirm != ConfirmAlways {
		t.Errorf("confirm = %q, want %q", cfg.Confirm, ConfirmAlways)
	}

	cfg, err = Load(write(t, `
projects_dir: /p
confirm: destructive
projects:
  - {name: a, git: g}
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Confirm != ConfirmDestructive {
		t.Errorf("an explicit setting must survive: %q", cfg.Confirm)
	}
}

// A message template holds a colon, so an unquoted one is a YAML mapping and
// the parser's complaint says nothing useful on its own.
func TestUnquotedMessageGetsAQuotingHint(t *testing.T) {
	t.Parallel()

	_, err := Load(write(t, `
projects_dir: /p
submodules_commit_message: chore(deps): update submodules on {branch}
projects:
  - {name: a, git: g}
`))
	if err == nil {
		t.Fatal("expected a parse error")
	}

	if !strings.Contains(err.Error(), `has to be quoted`) {
		t.Errorf("no hint in %q", err)
	}

	// Quoted, the same value loads.
	cfg, err := Load(write(t, `
projects_dir: /p
submodules_commit_message: "chore(deps): update submodules on {branch}"
projects:
  - {name: a, git: g}
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SubmodulesCommitMessage != "chore(deps): update submodules on {branch}" {
		t.Errorf("got %q", cfg.SubmodulesCommitMessage)
	}
}

// The same remote is written in more than one way, and a submodule is matched
// to its project by remote — paths differ between projects, section names are
// local to each .gitmodules.
func TestSameRemote(t *testing.T) {
	t.Parallel()

	same := [][2]string{
		{"git@git.example.com:group/lib.git", "git@git.example.com:group/lib.git"},
		{"git@git.example.com:group/lib.git", "https://git.example.com/group/lib.git"},
		{"git@git.example.com:group/lib.git", "ssh://git@git.example.com/group/lib"},
		{"https://git.example.com/group/lib", "git@git.example.com:group/lib.git/"},
		{"git@GIT.example.com:group/lib.git", "git@git.example.com:group/lib"},
	}
	for _, pair := range same {
		if !SameRemote(pair[0], pair[1]) {
			t.Errorf("%q and %q are the same repository", pair[0], pair[1])
		}
	}

	other := [][2]string{
		{"git@git.example.com:group/lib.git", "git@git.example.com:group/other.git"},
		{"git@git.example.com:group/lib.git", "git@other.example.com:group/lib.git"},
		{"git@git.example.com:group/lib.git", "git@git.example.com:another/lib.git"},
	}
	for _, pair := range other {
		if SameRemote(pair[0], pair[1]) {
			t.Errorf("%q and %q are different repositories", pair[0], pair[1])
		}
	}
}

// The example config is the documentation most people copy from, so it has
// to keep loading — a field renamed in code but not there would otherwise be
// found by whoever copies it next.
func TestExampleConfigLoads(t *testing.T) {
	t.Parallel()

	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Projects) == 0 || len(cfg.Flows) == 0 {
		t.Error("the example should show projects and a flow")
	}
}

// A failed pipeline gets one automatic second chance unless the config says
// otherwise; ci_retries: 0 makes a failure final at once.
func TestCIRetryLimit(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	if cfg.CIRetryLimit() != 1 {
		t.Errorf("default = %d, want 1", cfg.CIRetryLimit())
	}

	zero := 0
	cfg.CIRetries = &zero

	if cfg.CIRetryLimit() != 0 {
		t.Error("ci_retries: 0 must turn retries off")
	}

	two := 2
	cfg.CIRetries = &two

	if cfg.CIRetryLimit() != 2 {
		t.Errorf("got %d", cfg.CIRetryLimit())
	}

	negative := -1
	cfg.CIRetries = &negative

	if cfg.CIRetryLimit() != 0 {
		t.Error("a negative count means none, not infinity")
	}
}

// The pager follows the config, then $PAGER, then a sensible default.
func TestPagerCommand(t *testing.T) {
	cfg := &Config{}

	t.Setenv("PAGER", "")

	if got := cfg.PagerCommand(); got != "less -R" {
		t.Errorf("default pager = %q", got)
	}

	t.Setenv("PAGER", "more")

	if got := cfg.PagerCommand(); got != "more" {
		t.Errorf("$PAGER should be used: %q", got)
	}

	explicit := "bat -p"
	cfg.Pager = &explicit

	if got := cfg.PagerCommand(); got != "bat -p" {
		t.Errorf("the config should win: %q", got)
	}
}

// mr sits next to ci: a project setting with a default, where none is final
// — it keeps the project out of merge requests even when a run asks for
// them — and unset leaves the decision to --mr.
func TestMRIsAProjectSettingWithDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /srv/projects
defaults:
  mr: always
projects:
  - name: api
    git: git@example.com:api.git
  - name: ui
    git: git@example.com:ui.git
    mr: none
`))
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.Projects[0].MR; got != MRAlways {
		t.Errorf("api should take the default, got %q", got)
	}

	if got := cfg.Projects[1].MR; got != MRNone {
		t.Errorf("ui set its own, got %q", got)
	}

	if cfg.MergeRequest.Branch != "rt/{command}/{branch}/{date}" || cfg.MergeRequest.Title != "{message}" {
		t.Errorf("built-in templates missing: %+v", cfg.MergeRequest)
	}

	if cfg.MergeRequest.Wait != MRWaitDependents || cfg.MergeRequest.WaitFor() != 60*time.Minute ||
		cfg.MergeRequest.Settle() != 0 {
		t.Errorf("built-in wait settings missing: %+v", cfg.MergeRequest)
	}

	_, err = Load(write(t, "merge_request:\n  wait: later\n"+minimal))
	if err == nil || !strings.Contains(err.Error(), "wait: \"later\" is not one of dependents, always, none") {
		t.Errorf("a bad wait value must be refused: %v", err)
	}

	// Unset is a state of its own, not none.
	plain, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}

	if got := plain.Projects[0].MR; got != "" {
		t.Errorf("without a default mr stays unset, got %q", got)
	}

	_, err = Load(write(t, minimal+"    mr: sometimes\n"))
	if err == nil || !strings.Contains(err.Error(), "mr: \"sometimes\" is not one of always, none") {
		t.Errorf("a bad mr value must be refused: %v", err)
	}
}

// Hooks attach to the three events rt has; anything else is a typo found
// at load time.
func TestHooksAreValidated(t *testing.T) {
	t.Parallel()

	hooks := "hooks:\n  ask:\n    - \"notify-send rt \\\"$RT_MESSAGE\\\"\"\n  done:\n    - \"true\"\n"

	cfg, err := Load(write(t, hooks+minimal))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Hooks[HookAsk]) != 1 || len(cfg.Hooks[HookDone]) != 1 {
		t.Errorf("hooks = %v", cfg.Hooks)
	}

	_, err = Load(write(t, "hooks:\n  finished:\n    - \"true\"\n"+minimal))
	if err == nil || !strings.Contains(err.Error(), "hooks: \"finished\" is not one of ask, wait, done") {
		t.Errorf("an unknown event must be refused: %v", err)
	}
}
