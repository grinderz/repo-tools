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

// release: false takes a project out of the releases altogether: its tags
// and freezes go with it, whatever they say; defaults carry it too.
func TestReleaseOffTakesTagsAndFreezeWithIt(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /p
defaults:
  deps_freeze: true
projects:
  - {name: released, git: g}
  - {name: tooling, git: g2, release: false, release_tags: true}
`))
	if err != nil {
		t.Fatal(err)
	}

	if p := cfg.Projects[0]; !p.ReleaseEnabled() || !p.ReleaseTagsEnabled() || !p.DepsFreezeEnabled() {
		t.Errorf("a project that says nothing is released, tagged and frozen: %+v", p)
	}

	if p := cfg.Projects[1]; p.ReleaseEnabled() || p.ReleaseTagsEnabled() || p.DepsFreezeEnabled() {
		t.Errorf("release: false must switch the tags and the freeze off too: %+v", p)
	}

	cfg, err = Load(write(t, `
projects_dir: /p
defaults:
  release: false
projects:
  - {name: a, git: g}
  - {name: b, git: g2, release: true}
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Projects[0].ReleaseEnabled() || !cfg.Projects[1].ReleaseEnabled() {
		t.Error("defaults.release applies where the project says nothing")
	}

	cases := []struct{ body, want string }{
		{
			"projects:\n  - {name: t, git: g, release: false, release_branch: release-1.0}\n",
			"project t: release_branch \"release-1.0\" names a release the project does not have",
		},
		{
			"cmds: {changelog: [x]}\nprojects:\n" +
				"  - {name: t, git: g, release: false, changelog: true, changelog_branch: release}\n",
			"project t: changelog_branch: release names a release the project does not have",
		},
	}

	for _, tc := range cases {
		_, err := Load(write(t, "projects_dir: /p\n"+tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.body, err, tc.want)
		}
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

// The renovate block works without being written: the image, the program
// and the token variable have defaults, cache_dir takes ~, and a project
// opts out with renovate: false, itself or through the defaults.
func TestRenovateDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /srv/projects
renovate:
  cache_dir: ~/.cache/rt-renovate
  env: [GOPROXY, GOFLAGS=-mod=mod]
defaults:
  renovate: false
projects:
  - name: api
    git: git@example.com:api.git
    renovate: true
  - name: ui
    git: git@example.com:ui.git
`))
	if err != nil {
		t.Fatal(err)
	}

	r := cfg.Renovate
	if r.Image != DefaultRenovateImage || r.Docker != "docker" || r.TokenEnv != "RENOVATE_TOKEN" ||
		r.Update != "patch" || r.Group != "all" || r.Internal != "dev" {
		t.Errorf("defaults = %+v", r)
	}

	if strings.HasPrefix(r.CacheDir, "~") || !strings.HasSuffix(r.CacheDir, ".cache/rt-renovate") {
		t.Errorf("cache_dir = %q, want ~ expanded", r.CacheDir)
	}

	rel, err := Load(write(t, minimal+"renovate:\n  cache_dir: cache\n"))
	if err != nil {
		t.Fatal(err)
	}

	if !filepath.IsAbs(rel.Renovate.CacheDir) || filepath.Base(rel.Renovate.CacheDir) != "cache" {
		t.Errorf("cache_dir = %q, want absolute: a relative -v names a docker volume", rel.Renovate.CacheDir)
	}

	if !cfg.Projects[0].RenovateEnabled() || cfg.Projects[1].RenovateEnabled() {
		t.Errorf("api should be on by its own setting, ui off through the defaults")
	}

	plain, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}

	if !plain.Projects[0].RenovateEnabled() {
		t.Error("without a word in the config a project is in")
	}
}

func TestRenovateRejects(t *testing.T) {
	t.Parallel()

	cases := []struct{ body, want string }{
		{"renovate:\n  env: [\"1A\"]\n", "is not an environment variable name"},
		{"renovate:\n  env: [\"=x\"]\n", "is not an environment variable name"},
		{"renovate:\n  config: {platform: github}\n", "platform is set by deps renovate itself"},
		{"renovate:\n  env: [RENOVATE_TOKEN=x]\n", "RENOVATE_TOKEN is set by deps renovate itself"},
		{"renovate:\n  token_env: \"my token\"\n", "token_env: \"my token\" is not an environment variable name"},
		{
			"renovate:\n  hosts:\n    github.com: {token_env: \"1A\"}\n",
			"token_env: \"1A\" is not an environment variable name",
		},
		{"renovate:\n  config: {repositories: []}\n", "repositories is set by deps renovate itself"},
		{"renovate:\n  hosts:\n    github.com: {env: [\"1A\"]}\n", "hosts: github.com: env"},
		{"renovate:\n  hosts:\n    github.com: {config: {dryRun: full}}\n", "dryRun is set by deps renovate itself"},
		{"renovate:\n  update: all\n", "is not an update level"},
		{"renovate:\n  group: one\n", "is not a grouping"},
		{"renovate:\n  internal: sometimes\n", "is not an internal mode"},
		{"renovate:\n  config: {separateMinorPatch: false}\n", "separateMinorPatch is set by deps renovate itself"},
		{"renovate:\n  config: {packageRules: {enabled: false}}\n", "packageRules must be a list of rules"},
		{"renovate:\n  hosts:\n    github.com: {config: {packageRules: no}}\n", "packageRules must be a list of rules"},
	}

	for _, tc := range cases {
		_, err := Load(write(t, tc.body+minimal))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.body, err, tc.want)
		}
	}
}

// A hosts entry adjusts the run on that host alone: its env list replaces
// the block's, an empty one included, and its config goes over the block's
// key by key; a host with no entry sees the block as it is.
func TestRenovateForHost(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
renovate:
  env: [GOPROXY=https://proxy.example.com, RENOVATE_HOST_RULES]
  config: {prConcurrentLimit: 10, prHourlyLimit: 0}
  hosts:
    github.com:
      env: []
      config: {prHourlyLimit: 2, gitAuthor: "Bot <bot@example.com>"}
    git.example.com:
      config: {prConcurrentLimit: 3}
`+minimal))
	if err != nil {
		t.Fatal(err)
	}

	r := &cfg.Renovate

	gh := r.ForHost("github.com")
	if len(gh.Env) != 0 || gh.Config["prConcurrentLimit"] != 10 || gh.Config["prHourlyLimit"] != 2 ||
		gh.Config["gitAuthor"] != "Bot <bot@example.com>" {
		t.Errorf("github.com = env %q config %v", gh.Env, gh.Config)
	}

	gl := r.ForHost("git.example.com")
	if len(gl.Env) != 2 || gl.Config["prConcurrentLimit"] != 3 {
		t.Errorf("an entry without env keeps the block's: env %q config %v", gl.Env, gl.Config)
	}

	if other := r.ForHost("other.example.com"); other != r {
		t.Error("a host with no entry sees the block itself")
	}

	if r.Config["prHourlyLimit"] != 0 || len(r.Env) != 2 {
		t.Errorf("the block itself must stay as written: env %q config %v", r.Env, r.Config)
	}
}

// Pins load with their kinds and reach the projects they are for; the
// three fields, the one capture group, the kinds and the placeholders of the
// value are checked at load.
func TestPinsLoadAndReject(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, `
projects_dir: /p
toolchain:
  go: "1.26.0"
cmds:
  deps: {go: [x], uv: [x]}
pins:
  - file: go.mod
    match: '^go (.+)$'
    value: "{go}"
  - file: .gitlab-ci.yml
    match: '^  GO_VERSION: "(.+)"$'
    value: "{go_minor}"
    kinds: [go]
    optional: true
  - file: Makefile
    match: '^PRE_COMMIT \?= (.+)$'
    value: "{go}"
    kinds: [go, none]
  - file: .python-version
    match: '^(.+)$'
    value: "{go}"
    except: [py]
  - file: Makefile
    match: '^CI_TOOLS \?= (.+)$'
    value: "{ci_tools}"
    kinds: [none]
    except: [plain]
projects:
  - name: lib
    git: g
    deps: go
    pins:
      - file: Makefile
        match: '^GO_TOOLCHAIN \?= go(.+)$'
        value: "{go}"
  - name: py
    git: h
    deps: uv
  - name: plain
    git: i
  - name: tooling
    git: j
    toolchain:
      ci_tools: "0.1.1"
`))
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]*Project{}
	for _, p := range cfg.Projects {
		byName[p.Name] = p
	}

	if pins := cfg.PinsFor(byName["lib"]); len(pins) != 5 || pins[4].File != "Makefile" || !pins[1].Optional {
		t.Errorf("lib pins = %+v, want the four global ones and its own", pins)
	}

	if pins := cfg.PinsFor(byName["py"]); len(pins) != 1 || pins[0].File != "go.mod" {
		t.Errorf("py pins = %+v, want only the pin without kinds, the excepted one apart", pins)
	}

	// plain has no ci_tools version and is excepted from the pin that wants
	// one: kinds alone would have held it, and the load would have failed.
	if pins := cfg.PinsFor(byName["plain"]); len(pins) != 3 || pins[1].File != "Makefile" {
		t.Errorf("plain pins = %+v, want the unkinded pins and the one for none", pins)
	}

	if pins := cfg.PinsFor(byName["tooling"]); len(pins) != 4 || pins[3].Value != "{ci_tools}" {
		t.Errorf("tooling pins = %+v, want the pin plain is excepted from", pins)
	}

	// Whether except leaves anyone is judged with the kinds the defaults give
	// the projects, not the ones they write themselves.
	if _, err := Load(write(t, `
projects_dir: /p
toolchain:
  go: "1.26.0"
cmds:
  deps: {go: [x]}
defaults:
  deps: go
pins:
  - {file: go.mod, match: '^go (.+)$', value: "{go}", kinds: [go], except: [other]}
projects:
  - {name: api, git: g}
  - {name: other, git: h, deps: none}
`)); err != nil {
		t.Errorf("a pin reaching a project through defaults.deps: %v", err)
	}

	tree := []string{"go.mod", "svc-api/Dockerfile", "svc-web/Dockerfile", "svc-api/src/Dockerfile"}

	if got := (Pin{File: "svc-*/Dockerfile"}).Files(tree); strings.Join(
		got,
		",",
	) != "svc-api/Dockerfile,svc-web/Dockerfile" {
		t.Errorf("glob files = %q", got)
	}

	if got := (Pin{File: "go.mod"}).Files(tree); strings.Join(got, ",") != "go.mod" {
		t.Errorf("plain files = %q", got)
	}

	cases := []struct{ body, want string }{
		{"pins:\n  - {file: go.mod, match: 'go (.+)'}\n", "pins[0]: needs file, match and value"},
		{"pins:\n  - {file: go.mod, match: 'go (.+', value: x}\n", "pins[0]: match:"},
		{"pins:\n  - {file: 'svc-[/Dockerfile', match: 'go (.+)', value: x}\n", "pins[0]: file:"},
		{"pins:\n  - {file: go.mod, match: 'go (.+)\\.(.+)', value: x}\n", "needs exactly one capture group"},
		{
			"pins:\n  - {file: go.mod, match: 'go (.+)', value: x, kinds: [rust]}\n",
			"kinds: \"rust\" is not a cmds.deps kind",
		},
		{
			"pins:\n  - {file: go.mod, match: 'go (.+)', value: x, except: [nope]}\n",
			"pins[0]: except: \"nope\" is not a project in this config",
		},
		{
			"projects:\n  - {name: api, git: g, pins: [{file: f, match: 'v(.+)', value: x, except: [api]}]}\n",
			"project api: pins[0]: except is for the global pins",
		},
		{
			"pins:\n  - {file: go.mod, match: 'go (.+)', value: x, except: [api]}\n",
			"pins[0]: except leaves no project the pin applies to",
		},
		{
			"pins:\n  - {file: go.mod, match: 'go (.+)', value: '{rust}'}\n",
			"project api: {rust} has no version in toolchain",
		},
		{
			"projects:\n  - {name: api, git: g, pins: [{file: f, match: 'v(.+)', value: '{rust}'}]}\n",
			"project api: {rust} has no version in toolchain",
		},
		{
			"cmds:\n  deps:\n    go: [x]\nprojects:\n  - {name: api, git: g, cmds: {toolchain: {go: [\"awk '{print}' f\"]}}}\n",
			"project api: {print} has no version in toolchain",
		},
		{
			"pins:\n  - {file: go.mod, match: 'go (.+)', value: '{Go}'}\n",
			"project api: {Go} has no version in toolchain",
		},
		{
			"projects:\n  - {name: api, git: g, cmds: {toolchain: {rust: [x]}}}\n",
			"project api: cmds.toolchain: \"rust\" is not a cmds.deps kind",
		},
		{
			"projects:\n  - {name: api, git: g, cmds: {toolchain_check: {rust: [x]}}}\n",
			"project api: cmds.toolchain_check: \"rust\" is not a cmds.deps kind",
		},
	}

	for _, tc := range cases {
		body := tc.body
		if !strings.Contains(body, "projects:") {
			body += minimal
		} else {
			body = "projects_dir: /p\n" + body
		}

		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.body, err, tc.want)
		}
	}
}

// A shell parameter of a placeholder's shape, ${home}, is the shell's and
// needs no toolchain key.
func TestToolchainShellParameters(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, "cmds:\n  deps:\n    go: [true]\n  toolchain:\n    go: [\"echo ${home} {go}\", "+
		"\"go list -f '{{if .Module}}{{end}}'\"]\n"+
		"toolchain:\n  go: \"1.26\"\nprojects_dir: /srv\nprojects:\n  - {name: api, git: g, deps: go}\n"))
	if err != nil {
		t.Fatal(err)
	}

	if keys := ToolchainKeysIn(cfg.ToolchainCommands(cfg.Projects[0])); strings.Join(keys, ",") != "go" {
		t.Errorf("keys = %v", keys)
	}
}

// An overlay goes over its base: scalars and lists replaced, maps merged
// key by key, projects by name field by field with new ones appended; the
// base path is relative to the overlay; a cycle and a missing base are
// errors, and an unknown key in the base is refused like one in the overlay.
func TestExtends(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeNamed := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	writeNamed("base.yaml", `
projects_dir: /srv/projects
confirm: never
disable: ["deps freeze"]
cmds:
  deps: {go: [make deps]}
  changelog: [git-cliff]
defaults:
  dev_branch: develop
  changelog: false
projects:
  - name: lib
    git: git@example.com:lib.git
    deps: go
  - name: api
    git: git@example.com:api.git
    deps: go
`)
	over := writeNamed("release.yaml", `
extends: base.yaml
product_version: "2026.07"
disable: []
cmds:
  deps: {uv: [uv sync]}
defaults:
  changelog: true
projects:
  - name: api
    release_branch: release-6.9
  - name: worker
    git: git@example.com:worker.git
`)

	cfg, err := Load(over)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ProjectsDir != "/srv/projects" || cfg.Confirm != "never" || cfg.ProductVersion != "2026.07" {
		t.Errorf("scalars: %+v", cfg)
	}

	if len(cfg.Disable) != 0 {
		t.Errorf("a list is replaced as a whole: %v", cfg.Disable)
	}

	if len(cfg.Cmds.Deps["go"]) != 1 || len(cfg.Cmds.Deps["uv"]) != 1 || len(cfg.Cmds.Changelog) != 1 {
		t.Errorf("maps merge key by key: %+v", cfg.Cmds)
	}

	if cfg.Defaults.DevBranch != "develop" || cfg.Defaults.Changelog == nil || !*cfg.Defaults.Changelog {
		t.Errorf("defaults: %+v", cfg.Defaults)
	}

	names := make([]string, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		names = append(names, p.Name)
	}

	if strings.Join(names, ",") != "lib,api,worker" {
		t.Errorf("projects = %v, want the base order with the new one last", names)
	}

	if api := cfg.Projects[1]; api.Git != "git@example.com:api.git" || api.Deps != "go" ||
		api.ReleaseBranch != "release-6.9" {
		t.Errorf("api should keep its base fields and take the overlay's: %+v", api)
	}
}

// A scalar keeps the text it was written with through the merge: 3.10 is a
// version, not a float; an alias in either file is resolved, so the same
// anchor name in both does not collide.
func TestExtendsKeepsScalarsAndAliases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, body := range map[string]string{
		"base.yaml": "projects_dir: /p\ntoolchain:\n  python: 3.10\ncmds:\n  deps: {go: [&x make deps]}\n" +
			"  deps_check: {go: [*x]}\nprojects:\n  - {name: api, git: g, deps: go}\n",
		"over.yaml": "extends: base.yaml\nproduct_version: 2026.10\ncmds:\n  changelog: [&x git-cliff]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"base.yaml", "over.yaml"} {
		cfg, err := Load(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if cfg.Toolchain["python"] != "3.10" {
			t.Errorf("%s: python = %q, want the text as written", name, cfg.Toolchain["python"])
		}

		if cfg.Cmds.DepsCheck["go"][0] != "make deps" {
			t.Errorf("%s: alias = %q", name, cfg.Cmds.DepsCheck["go"])
		}
	}

	cfg, err := Load(filepath.Join(dir, "over.yaml"))
	if err != nil || cfg.ProductVersion != "2026.10" || cfg.Cmds.Changelog[0] != "git-cliff" {
		t.Errorf("overlay: %v %q %q", err, cfg.ProductVersion, cfg.Cmds.Changelog)
	}
}

// Document is the merged file as Load reads it, extends gone.
func TestExtendsDocument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, body := range map[string]string{
		"base.yaml": minimal,
		"over.yaml": "extends: base.yaml\nprojects:\n  - {name: api, release_branch: release-6.9}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	doc, err := Document(filepath.Join(dir, "over.yaml"))
	if err != nil || !strings.Contains(string(doc), "release_branch: release-6.9") ||
		!strings.Contains(string(doc), "git: git@example.com:api.git") || strings.Contains(string(doc), "extends") {
		t.Errorf("document = %s, %v", doc, err)
	}
}

// A cycle, a base that is not there and an unknown key in the base are
// refused, each with the file named.
func TestExtendsRejects(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, body := range map[string]string{
		"loop-a.yaml":   "extends: loop-b.yaml\n",
		"loop-b.yaml":   "extends: loop-a.yaml\n",
		"gone.yaml":     "extends: nowhere.yaml\n",
		"bad-base.yaml": "extends: typo.yaml\n",
		"typo.yaml":     "projects_dirr: /x\n" + minimal,
		"list.yaml":     "- a\n",
		"twice.yaml":    "extends: typo.yaml\nprojects:\n  - {name: api}\n  - {name: api}\n",
		"clean.yaml":    minimal,
		"dup.yaml":      "extends: clean.yaml\nprojects_dir: /a\nprojects_dir: /b\n",
		"dup-deep.yaml": "extends: clean.yaml\ncmds:\n  deps:\n    go: [a]\n    go: [b]\n",
		"dup-ext.yaml":  "extends: clean.yaml\nextends: clean.yaml\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for name, want := range map[string]string{
		"loop-a.yaml":   "extends cycle",
		"gone.yaml":     "read config",
		"bad-base.yaml": "projects_dirr",
		"list.yaml":     "not a mapping",
		"twice.yaml":    `duplicate name "api"`,
		"dup.yaml":      `mapping key "projects_dir" already defined`,
		"dup-deep.yaml": `mapping key "go" already defined`,
		"dup-ext.yaml":  `mapping key "extends" already defined`,
	} {
		if _, err := Load(filepath.Join(dir, name)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
}

func TestSplitRemote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		url, host, path string
		ok              bool
	}{
		{"git@git.example.com:group/api.git", "git.example.com", "group/api", true},
		{"git@git.example.com:group/sub/api", "git.example.com", "group/sub/api", true},
		{"https://git.example.com/group/api.git", "git.example.com", "group/api", true},
		{"ssh://git@git.example.com:2222/group/api.git", "git.example.com", "group/api", true},
		{"https://github.com/Owner/Repo/", "github.com", "Owner/Repo", true},
		{"https://deploy:glpat-x@git.example.com:8443/group/api.git", "git.example.com:8443", "group/api", true},
		{"git@git.example.com:group/api@2.git", "git.example.com", "group/api@2", true},
		{"/srv/git/api.git", "", "", false},
		{"../api", "", "", false},
	}

	for _, tc := range cases {
		host, path, ok := SplitRemote(tc.url)
		if host != tc.host || path != tc.path || ok != tc.ok {
			t.Errorf("SplitRemote(%q) = %q, %q, %v; want %q, %q, %v", tc.url, host, path, ok, tc.host, tc.path, tc.ok)
		}
	}
}

const withToolchain = `
projects_dir: /srv/projects
toolchain:
  go: "1.26"
  go_toolchain: "1.26.2"
  python: "3.14"
cmds:
  deps:
    go: ["go mod tidy"]
    uv: ["uv sync"]
  toolchain:
    go: ["go mod edit -go={go_minor}.0 -toolchain=go{go_toolchain}"]
    uv: ["uv python pin {python_minor}", "echo {python_next_minor}"]
  toolchain_check:
    uv: ["grep -qx {python_minor} .python-version"]
projects:
  - name: lib
    git: git@example.com:lib.git
    deps: go
  - name: api
    git: git@example.com:api.git
    deps: uv
  - name: tooling
    git: git@example.com:tooling.git
    cmds:
      toolchain:
        go: ["sed -i s/GO_TOOLCHAIN.*/go{go_toolchain}/ go.mk"]
  - name: legacy
    git: git@example.com:legacy.git
    deps: go
    toolchain:
      go_toolchain: "1.25.14"
    toolchain_commit_message: "chore: bump go to {go_toolchain}"
  - name: plain
    git: git@example.com:plain.git
`

// The toolchain commands follow the deps kind, a project's own list replaces
// its kind's, a tooling repository with deps: none still gets its own, and a
// project's versions override the global ones entry by entry.
func TestToolchainCommandsAndVersions(t *testing.T) {
	t.Parallel()

	cfg, err := Load(write(t, withToolchain))
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]*Project{}
	for _, p := range cfg.Projects {
		byName[p.Name] = p
	}

	if got := cfg.ToolchainCommands(byName["lib"]); len(got) != 1 || !strings.Contains(got[0], "go mod edit") {
		t.Errorf("lib = %q", got)
	}

	if got := cfg.ToolchainCheckCommands(byName["api"]); len(got) != 1 {
		t.Errorf("api checks = %q", got)
	}

	if got := cfg.ToolchainCommands(byName["tooling"]); len(got) != 1 || !strings.Contains(got[0], "go.mk") {
		t.Errorf("tooling = %q", got)
	}

	if got := cfg.ToolchainKinds(byName["tooling"]); len(got) != 1 || got[0] != "go" {
		t.Errorf("tooling kinds = %q", got)
	}

	if got := cfg.ToolchainCommands(byName["plain"]); len(got) != 0 {
		t.Errorf("plain has no kind and no list of its own: %q", got)
	}

	legacy := cfg.ToolchainFor(byName["legacy"])
	if legacy["go_toolchain"] != "1.25.14" || legacy["go"] != "1.26" {
		t.Errorf("legacy = %v, want its own toolchain over the global go", legacy)
	}

	if codename, err := Load(write(t, "toolchain:\n  debian: trixie\n"+minimal)); err != nil ||
		codename.Toolchain["debian"] != "trixie" {
		t.Errorf("a release codename is a version too: %v", err)
	}

	if cfg.ToolchainCommitMessage != "chore: bump toolchain" {
		t.Errorf("default message = %q", cfg.ToolchainCommitMessage)
	}

	if got := cfg.ToolchainCommitMessageFor(byName["legacy"]); got != "chore: bump go to {go_toolchain}" {
		t.Errorf("legacy message = %q, want its own", got)
	}

	if got := cfg.ToolchainCommitMessageFor(byName["plain"]); got != "chore: bump toolchain" {
		t.Errorf("plain message = %q, want the global one", got)
	}
}

func TestToolchainRejects(t *testing.T) {
	t.Parallel()

	cases := []struct{ body, want string }{
		{"toolchain:\n  Go: \"1.26\"\n", "is not a toolchain key"},
		{"toolchain:\n  go_minor: \"1.26\"\n", "_minor is a derived form"},
		{"toolchain:\n  branch: \"1.0\"\n", `"branch" is not a toolchain key`},
		{
			"toolchain:\n  python: \"3.14rc1\"\npins:\n  - {file: f, match: 'v(.+)', value: '{python_next_minor}'}\n",
			"{python_next_minor}: python \"3.14rc1\" has no minor number",
		},
		{"toolchain:\n  go: \"\"\n", "is not a version"},
		{"toolchain:\n  go: \"1.26 rc\"\n", "is not a version"},
		{"toolchain:\n  go: latest\n", "is not a version"},
		{"cmds:\n  deps: {go: [x]}\n  toolchain:\n    rust: [x]\n", "cmds.toolchain: \"rust\" is not a cmds.deps kind"},
		{
			"toolchain:\n  go: \"1.26\"\ncmds:\n  deps: {go: [x]}\n  toolchain:\n    go: [\"echo {rust_next_minor}\"]\n" +
				"projects:\n  - {name: lib, git: g, deps: go}\n",
			"project lib: {rust_next_minor} has no version in toolchain",
		},
		{
			"toolchain:\n  go: \"1.26\"\ntoolchain_commit_message: \"bump {date}\"\n" +
				"cmds:\n  deps: {go: [x]}\n  toolchain:\n    go: [x]\nprojects:\n  - {name: lib, git: g, deps: go}\n",
			"project lib: {date} has no version in toolchain",
		},
		{
			"toolchain:\n  go: \"1.26\"\ncmds:\n  deps: {go: [x]}\n  toolchain:\n    go: [x]\nprojects:\n" +
				"  - {name: lib, git: g, deps: go, toolchain_commit_message: \"bump {rust}\"}\n",
			"project lib: {rust} has no version in toolchain",
		},
	}

	for _, tc := range cases {
		body := tc.body
		if !strings.Contains(body, "projects:") {
			body += minimal
		} else {
			body = "projects_dir: /p\n" + body
		}

		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.body, err, tc.want)
		}
	}
}
