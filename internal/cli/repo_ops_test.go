package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// publishOrigin makes the fixture's branches visible as origin refs, the way a
// fetched clone sees them.
func publishOrigin(t *testing.T, dir string, branches ...string) {
	t.Helper()

	for _, b := range branches {
		git(t, dir, "update-ref", "refs/remotes/origin/"+b, b)
	}
}

func TestLatestReleasePicksTheHighest(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	for _, b := range []string{"release-1.9", "release-1.10", "release-2.0", "release-old"} {
		git(t, f.parent, "branch", b, "release-1.0")
	}

	publishOrigin(t, f.parent, "release-1.0", "release-1.9", "release-1.10", "release-2.0", "release-old")

	name, ver, err := latestRelease(r, p, "")
	if err != nil {
		t.Fatal(err)
	}

	// 1.10 must beat 1.9, which string ordering would get wrong.
	if name != "release-2.0" || ver.Major != 2 || ver.Minor != 0 {
		t.Errorf("got %s (%v), want release-2.0", name, ver)
	}
}

func TestLatestReleaseOverrideIsValidated(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	name, _, err := latestRelease(r, p, "release-3.4")
	if err != nil || name != "release-3.4" {
		t.Fatalf("an explicit branch should pass through: %q %v", name, err)
	}

	if _, _, err := latestRelease(r, p, "hotfix"); err == nil {
		t.Error("a branch that is not <prefix>X.Y must be refused")
	}
}

func TestLatestReleaseWithoutAnyReleaseBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	publishOrigin(t, f.parent, "develop")

	_, _, err := latestRelease(r, p, "")
	if err == nil || !strings.Contains(err.Error(), "run release branch first") {
		t.Errorf("expected guidance towards release branch, got %v", err)
	}
}

func TestRequireClean(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	if err := requireClean(r); err != nil {
		t.Fatalf("the fixture should start clean: %v", err)
	}

	writeFile(t, f.parent, "app.txt", "dirty\n")

	if err := requireClean(r); err == nil {
		t.Error("a modified file must be refused")
	}
}

// The review prints what is staged and, with nobody to ask, lets the commit
// through rather than blocking a non-interactive run.
func TestReviewStagedPrintsAndPassesWhenNotInteractive(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	writeFile(t, f.parent, "app.txt", "staged change\n")
	git(t, f.parent, "add", "-A")

	var (
		ok  bool
		err error
	)

	msg := "build/AB-0000: update submodules on develop"

	out := captureOutput(t, func() { ok, err = reviewStaged(testCtx(), r, "parent", msg) })

	if err != nil || !ok {
		t.Fatalf("non-interactive review should pass: %v %v", ok, err)
	}

	if !strings.Contains(out, "parent is about to commit") || !strings.Contains(out, "staged change") {
		t.Errorf("the diff was not shown: %q", out)
	}

	// The message is as much a decision as the diff, and it comes after it:
	// next to the question, not scrolled away above a long patch.
	if !strings.Contains(out, "message: build/AB-0000: update submodules on develop") {
		t.Errorf("the commit message was not shown: %q", out)
	}

	if strings.Index(out, "staged change") > strings.Index(out, "message:") {
		t.Errorf("the message should follow the diff, not precede it:\n%s", out)
	}
}

func TestReviewStagedSilentWhenDisabled(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	writeFile(t, f.parent, "app.txt", "staged change\n")
	git(t, f.parent, "add", "-A")

	off := false
	rctx := &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}, DiffFlag: &off}

	var ok bool

	out := captureOutput(t, func() { ok, _ = reviewStaged(rctx, r, "parent", "msg") })

	if !ok || out != "" {
		t.Errorf("--no-diff should print nothing and pass, got %q", out)
	}
}

// Nothing to push means nothing to review.
func TestReviewUnpushedWithNothingAhead(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	publishOrigin(t, f.parent, "release-1.0")

	var ok bool

	out := captureOutput(t, func() { ok, _ = reviewUnpushed(testCtx(), r, "parent", "release-1.0") })

	if !ok || out != "" {
		t.Errorf("expected silence, got %q", out)
	}
}

func TestReviewUnpushedShowsTheCommits(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	publishOrigin(t, f.parent, "release-1.0")
	writeFile(t, f.parent, "app.txt", "picked work\n")
	commit(t, f.parent, "parent: a pick")

	var ok bool

	out := captureOutput(t, func() { ok, _ = reviewUnpushed(testCtx(), r, "parent", "release-1.0") })

	if !ok {
		t.Fatal("non-interactive review should pass")
	}

	if !strings.Contains(out, "parent is about to push to release-1.0") ||
		!strings.Contains(out, "parent: a pick") {
		t.Errorf("the commits were not shown: %q", out)
	}
}

func TestCheckRemoteAndWorktree(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	// The fixture has no origin at all and no origin/develop.
	issues := checkRemote(r, p)
	if len(issues) != 2 {
		t.Fatalf("got %v, want a remote and a dev-branch complaint", issues)
	}

	if !strings.Contains(strings.Join(issues, " "), "no origin remote") {
		t.Errorf("got %v", issues)
	}

	git(t, f.parent, "checkout", "--quiet", "develop")
	// Switching branches leaves the submodule at the other branch's pin.
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")
	publishOrigin(t, f.parent, "develop")

	if issues := checkWorktree(r); len(issues) != 0 {
		t.Errorf("a clean branch in step with origin has no issues: %v", issues)
	}

	writeFile(t, f.parent, "app.txt", "local work\n")
	commit(t, f.parent, "parent: unpushed")
	writeFile(t, f.parent, "app.txt", "and uncommitted\n")

	issues = checkWorktree(r)
	joined := strings.Join(issues, " ")

	if !strings.Contains(joined, "dirty") || !strings.Contains(joined, "ahead of origin") {
		t.Errorf("got %v, want both a dirty tree and an ahead branch", issues)
	}
}

func TestCheckSubmodulesAgainstConfig(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	// Configured path that .gitmodules does not have.
	p.Submodules = []config.Submodule{{Path: "nope", FreezeTo: "develop"}}

	issues := checkSubmodules(testCtx(), r, p)
	if len(issues) == 0 || !strings.Contains(issues[0], "not in .gitmodules") {
		t.Errorf("got %v", issues)
	}

	// The real submodule is not configured, which only matters with freezing on.
	enabled := true
	p.Submodules = nil
	p.DepsFreeze = &enabled

	issues = checkSubmodules(testCtx(), r, p)
	if len(issues) != 1 || !strings.Contains(issues[0], "not configured for freeze") {
		t.Errorf("got %v", issues)
	}

	p.DepsFreeze = nil

	if issues := checkSubmodules(testCtx(), r, p); len(issues) != 0 {
		t.Errorf("without freezing an unconfigured submodule is fine: %v", issues)
	}
}

// A deps command whose program is missing cannot run, and that is worth
// saying before a batch gets halfway through.
func TestCheckDeps(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.Deps = "go"

	rctx := testCtx()
	rctx.Cfg.DepsCmds = map[string][]string{"go": {"definitely-not-a-real-program tidy"}}

	issues := checkDeps(rctx, p)
	if len(issues) != 1 || !strings.Contains(issues[0], "definitely-not-a-real-program is not in PATH") {
		t.Errorf("got %v", issues)
	}

	rctx.Cfg.DepsCmds = map[string][]string{"go": {"git status"}}
	if issues := checkDeps(rctx, p); len(issues) != 0 {
		t.Errorf("a reachable program is fine: %v", issues)
	}

	// A shell construct is not a program name and must not be looked up.
	rctx.Cfg.DepsCmds = map[string][]string{"go": {"FOO=1 git status"}}
	if issues := checkDeps(rctx, p); len(issues) != 0 {
		t.Errorf("an env assignment should be left alone: %v", issues)
	}

	p.Deps = config.DepsNone
	if issues := checkDeps(rctx, p); len(issues) != 0 {
		t.Errorf("deps: none checks nothing: %v", issues)
	}
}

// freeze_to names a branch directly, or asks for the submodule's own highest
// release branch.
func TestResolveFreezeBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")

	explicit, err := resolveFreezeBranch(testCtx(), r, p, config.Submodule{Path: "sub", FreezeTo: "develop"}, "")
	if err != nil || explicit != "develop" {
		t.Fatalf("explicit branch: got %q, err %v", explicit, err)
	}

	// The submodule remote carries release-1.0, so "release" resolves to it.
	got, err := resolveFreezeBranch(testCtx(), r, p, config.Submodule{Path: "sub", FreezeTo: freezeToRelease}, "")
	if err != nil {
		t.Fatal(err)
	}

	if got != "release-1.0" {
		t.Errorf("got %q, want release-1.0", got)
	}
}

// The commands come from the config, a project may replace them outright, and
// deps: none runs nothing whatever the map says.
func TestDepsCmds(t *testing.T) {
	rctx := testCtx()
	rctx.Cfg.DepsCmds = map[string][]string{
		"uv": {"uv sync"},
		"go": {"make deps.update.internal", "echo {project} on {branch}"},
	}

	if got := depsCmds(rctx, &config.Project{Deps: "uv"}, "develop"); len(got) != 1 || got[0] != "uv sync" {
		t.Errorf("got %v", got)
	}

	got := depsCmds(rctx, &config.Project{Name: "api", Deps: "go"}, "release-1.0")
	if len(got) != 2 || got[1] != "echo api on release-1.0" {
		t.Errorf("placeholders were not expanded: %v", got)
	}

	own := &config.Project{Name: "api", Deps: "go", DepsCmds: []string{"go mod tidy"}}
	if got := depsCmds(rctx, own, "develop"); len(got) != 1 || got[0] != "go mod tidy" {
		t.Errorf("a project list should replace the kind's: %v", got)
	}

	if got := depsCmds(rctx, &config.Project{Deps: config.DepsNone}, "develop"); len(got) != 0 {
		t.Errorf("deps: none runs nothing: %v", got)
	}
}

// deps freeze runs the same configured commands, and --no-deps skips them.
func TestFreezeDepsRunsTheConfiguredDepsCmds(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.Deps = "uv"
	p.Submodules = []config.Submodule{{Path: "sub", FreezeTo: "release-1.0"}}

	rctx := testCtx()
	rctx.Cfg.DepsCmds = map[string][]string{"uv": {"echo {branch} > deps.txt"}}
	rctx.Cfg.FreezeCommitMessage = "chore(deps): freeze {branch}"

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	captureOutput(t, func() {
		// No origin in the fixture, so the push fails after the commit.
		_ = freezeDeps(rctx, p, "release-1.0", false, false)
	})

	body, err := os.ReadFile(filepath.Join(f.parent, "deps.txt"))
	if err != nil {
		t.Fatalf("the deps command did not run: %v", err)
	}

	if strings.TrimSpace(string(body)) != "release-1.0" {
		t.Errorf("deps.txt = %q, want the expanded {branch}", body)
	}

	if msg := git(t, f.parent, "log", "-1", "--format=%s"); msg != "chore(deps): freeze release-1.0" {
		t.Errorf("commit subject = %q", msg)
	}
}

func TestPlanFreezeDepsHonoursNoDeps(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.Deps = "uv"
	depsFreeze := true
	p.DepsFreeze = &depsFreeze

	rctx := testCtx()
	rctx.Cfg.DepsCmds = map[string][]string{"uv": {"uv sync"}}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")

	step, err := planFreezeDeps(rctx, p, "release-1.0", false, false)
	if err != nil || !strings.Contains(strings.Join(step.Plan, "\n"), "deps (uv): uv sync") {
		t.Errorf("plan = %v (warn %q, err %v)", step.Plan, step.Warn, err)
	}

	step, err = planFreezeDeps(rctx, p, "release-1.0", true, false)
	if err != nil || strings.Contains(strings.Join(step.Plan, "\n"), "deps (") {
		t.Errorf("--no-deps should not plan deps commands: %v (err %v)", step.Plan, err)
	}
}

// A release branch the config names before anyone creates it cannot be checked
// out, so the plan says so instead of describing a freeze built from whatever
// the working tree happens to have.
func TestPlanFreezeDepsRefusesAPendingReleaseBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	depsFreeze := true
	p.DepsFreeze = &depsFreeze

	step, err := planFreezeDeps(testCtx(), p, "release-9.9", false, false)
	if err != nil {
		t.Fatal(err)
	}

	if !step.Skip || !strings.Contains(step.Warn, "origin/release-9.9 does not exist yet") {
		t.Errorf("skip=%v warn=%q", step.Skip, step.Warn)
	}

	if len(step.Plan) != 0 {
		t.Errorf("nothing is planned for a branch checkout cannot reach: %v", step.Plan)
	}
}

// The "nothing configured" warning judges the branch being frozen: a release
// branch cut before the submodule was added has none to freeze, however many
// the current checkout carries.
func TestPlanFreezeDepsWarnsAboutTheReleaseBranchSubmodules(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	depsFreeze := true
	p.DepsFreeze = &depsFreeze

	git(t, f.parent, "branch", "release-2.0", git(t, f.parent, "rev-list", "--max-parents=0", "HEAD"))

	step, err := planFreezeDeps(testCtx(), p, "release-2.0", false, false)
	if err != nil || step.Warn != "" {
		t.Errorf("no submodules there, nothing to warn about: %q (err %v)", step.Warn, err)
	}

	step, err = planFreezeDeps(testCtx(), p, "release-1.0", false, false)
	if err != nil || !strings.Contains(step.Warn, "none are configured for freeze") {
		t.Errorf("the submodule on release-1.0 is unconfigured: %q (err %v)", step.Warn, err)
	}
}

// The freeze plan describes the release branch, so it must read that branch's
// .gitmodules. Judging by the checked-out branch would resolve freeze_to
// against the wrong submodule set — or against no submodule at all.
func TestPlanSubmoduleFreezeReadsTheReleaseBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.Submodules = []config.Submodule{{Path: "sub", FreezeTo: freezeToRelease}}

	// develop drops the submodule entirely; release-1.0 still has it.
	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "rm", "--quiet", "-f", "sub")
	commit(t, f.parent, "parent: drop the submodule on develop")
	publishOrigin(t, f.parent, "develop", "release-1.0")

	r := gitx.Repo{Dir: f.parent}

	plan := strings.Join(planSubmoduleFreeze(testCtx(), r, p, branchRef(r, "release-1.0")), "\n")
	if !strings.Contains(plan, "submodule sub -> branch release-1.0") {
		t.Errorf("the release branch still has sub:\n%s", plan)
	}

	// Reading the checkout instead is what the fix is about: develop dropped
	// the submodule, so the plan there can only report it missing.
	stale := strings.Join(planSubmoduleFreeze(testCtx(), r, p, ""), "\n")
	if !strings.Contains(stale, "MISSING from .gitmodules on the working tree") {
		t.Errorf("the develop checkout has no sub, so this is the wrong answer:\n%s", stale)
	}
}

// A configured submodule the release branch does not have is skipped with a
// warning, the way the plan said — the other submodules still get frozen.
func TestFreezeDepsSkipsSubmodulesMissingOnTheBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.Submodules = []config.Submodule{
		{Path: "sub", FreezeTo: "release-1.0"},
		{Path: "gone", FreezeTo: "develop"},
	}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	out := captureOutput(t, func() {
		// No origin in the fixture, so the push at the end fails.
		_ = freezeDeps(testCtx(), p, "release-1.0", true, false)
	})

	if !strings.Contains(out, "WARNING: gone is not in .gitmodules") {
		t.Errorf("the missing submodule should be reported:\n%s", out)
	}

	if !strings.Contains(out, "sub -> release-1.0") {
		t.Errorf("the present submodule should still be frozen:\n%s", out)
	}
}

// A config that names its release branch describes one release: every command
// takes that branch, and it need not exist yet.
func TestReleaseBranchFromConfig(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.ReleaseBranch = "release-9.9"

	r := gitx.Repo{Dir: f.parent}
	publishOrigin(t, f.parent, "develop", "release-1.0")

	branch, ver, err := latestRelease(r, p, "")
	if err != nil {
		t.Fatal(err)
	}

	if branch != "release-9.9" || ver.Major != 9 || ver.Minor != 9 {
		t.Errorf("got %s (%v), want the configured release-9.9", branch, ver)
	}

	// The flag still wins over the config.
	if branch, _, err = latestRelease(r, p, "release-2.0"); err != nil || branch != "release-2.0" {
		t.Errorf("--release-branch should win: %q %v", branch, err)
	}

	// And release branch creates exactly the configured one.
	name, err := nextReleaseBranch(r, p, "", false)
	if err != nil || name != "release-9.9" {
		t.Errorf("nextReleaseBranch = %q, %v; want release-9.9", name, err)
	}

	// --version and --major still override it.
	if name, err = nextReleaseBranch(r, p, "3.1", false); err != nil || name != "release-3.1" {
		t.Errorf("--version should win: %q %v", name, err)
	}

	if name, err = nextReleaseBranch(r, p, "", true); err != nil || name != "release-2.0" {
		t.Errorf("--major should bump the highest branch on origin: %q %v", name, err)
	}
}

// A release branch the config names but nobody has created yet is reported for
// every project: release branch creates it regardless of deps_freeze, and a
// library others freeze to is exactly the project that would look fine while
// blocking everyone else.
func TestCheckReleaseBranchPending(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	if issues := checkReleaseBranch(r, p); len(issues) != 0 {
		t.Errorf("no release branch named, nothing pending: %v", issues)
	}

	p.ReleaseBranch = "release-9.9"
	enabled := true
	p.DepsFreeze = &enabled

	issues := checkReleaseBranch(r, p)
	if len(issues) != 1 || !strings.Contains(issues[0], "origin/release-9.9 does not exist yet") {
		t.Fatalf("got %v", issues)
	}

	// The regression: a project that freezes nothing still needs its branch.
	p.DepsFreeze = nil
	if issues := checkReleaseBranch(r, p); len(issues) != 1 {
		t.Errorf("deps_freeze must not gate this: %v", issues)
	}
}

// Before the release branch exists there is nothing to validate the submodule
// settings against, so checkSubmodules stays silent rather than judging the dev
// branch or repeating what checkReleaseBranch already said.
func TestCheckSubmodulesWithAPendingReleaseBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.ReleaseBranch = "release-9.9"
	enabled := true
	p.DepsFreeze = &enabled

	if issues := checkSubmodules(testCtx(), gitx.Repo{Dir: f.parent}, p); len(issues) != 0 {
		t.Errorf("got %v", issues)
	}
}

// changelog_branch: release means "this project's release branch", so a
// per-release config names the branch once instead of in two fields.
func TestChangelogBranchRelease(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}
	publishOrigin(t, f.parent, "develop", "release-1.0")

	if got, err := changelogBranch(r, p, ""); err != nil || got != "develop" {
		t.Errorf("without the sentinel the dev branch stands: %q %v", got, err)
	}

	p.ChangelogBranch = changelogBranchRelease

	got, err := changelogBranch(r, p, "")
	if err != nil || got != "release-1.0" {
		t.Errorf("got %q, %v; want the highest release branch", got, err)
	}

	// release_branch from the config wins over what origin happens to have.
	p.ReleaseBranch = "release-9.9"

	if got, err = changelogBranch(r, p, ""); err != nil || got != "release-9.9" {
		t.Errorf("got %q, %v; want the configured release branch", got, err)
	}

	// And --branch still wins over everything.
	if got, err = changelogBranch(r, p, "hotfix"); err != nil || got != "hotfix" {
		t.Errorf("got %q, %v; want the flag", got, err)
	}
}

// A project kept off the network must not be planned as if it were fetched.
func TestPlanSyncHonoursTheProjectFetchSetting(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	// No --fetch/--no-fetch here: the project setting is what is under test.
	rctx := &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}}

	if plan := strings.Join(planSync(rctx, p, syncOptions{}).Plan, "\n"); !strings.Contains(plan, "fetch --prune") {
		t.Errorf("by default sync fetches: %q", plan)
	}

	disabled := false
	p.Fetch = &disabled

	plan := strings.Join(planSync(rctx, p, syncOptions{}).Plan, "\n")
	if strings.Contains(plan, "fetch --prune") || !strings.Contains(plan, "no fetch") {
		t.Errorf("fetch: false should be visible in the plan: %q", plan)
	}
}

// The dirty-tree refusal names what is in the way; the usual cause is a
// previous run that stopped half way, and its leftovers are invisible
// otherwise.
func TestRequireCleanNamesTheChanges(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	if err := requireClean(r); err != nil {
		t.Fatalf("the fixture starts clean: %v", err)
	}

	writeFile(t, f.parent, "app.txt", "half-done work\n")

	err := requireClean(r)
	if err == nil {
		t.Fatal("a modified file must be refused")
	}

	if !strings.Contains(err.Error(), "app.txt") {
		t.Errorf("the error should name the file: %v", err)
	}

	if !strings.Contains(err.Error(), "discard") {
		t.Errorf("the error should offer a way out: %v", err)
	}
}

// A submodule that is itself a configured project needs no freeze_to of its
// own: the branch is written once, in that project's definition.
func TestFreezeListDerivedFromProjects(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.Submodules = nil

	rctx := testCtx()
	// The fixture's submodule is the "sub" repository; make it a project.
	provider := &config.Project{Name: "sub", Git: f.sub, DevBranch: "develop"}
	rctx.Cfg.Projects = append(rctx.Cfg.Projects, provider)

	r := gitx.Repo{Dir: f.parent}
	git(t, f.parent, "checkout", "--quiet", "develop")

	derived, err := freezeList(rctx, r, p, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(derived) != 1 || derived[0].Path != "sub" || derived[0].FreezeTo != config.FreezeToProject {
		t.Fatalf("derived = %+v", derived)
	}

	// It resolves to the branch that project works on in this config.
	target, err := resolveFreezeBranch(rctx, r, p, derived[0], "")
	if err != nil || target != "develop" {
		t.Errorf("target = %q, %v; want develop", target, err)
	}

	// With a release branch in the config, that is the branch instead.
	provider.ReleaseBranch = "release-1.0"

	if target, err = resolveFreezeBranch(rctx, r, p, derived[0], ""); err != nil || target != "release-1.0" {
		t.Errorf("target = %q, %v; want release-1.0", target, err)
	}

	// An explicit list still wins, and a submodule from no project is not
	// derived at all.
	p.Submodules = []config.Submodule{{Path: "sub", FreezeTo: "master"}}

	if derived, _ = freezeList(rctx, r, p, ""); derived[0].FreezeTo != "master" {
		t.Errorf("an explicit list must win: %+v", derived)
	}

	p.Submodules = nil
	rctx.Cfg.Projects = rctx.Cfg.Projects[:len(rctx.Cfg.Projects)-1]

	if derived, _ = freezeList(rctx, r, p, ""); len(derived) != 0 {
		t.Errorf("a submodule that is not a project has no branch to follow: %+v", derived)
	}

	// And the derivation can be switched off wholesale.
	rctx.Cfg.Projects = append(rctx.Cfg.Projects, provider)
	off := false
	rctx.Cfg.DeriveSubmodules = &off

	if derived, _ = freezeList(rctx, r, p, ""); len(derived) != 0 {
		t.Errorf("derive_submodules: false should stop it: %+v", derived)
	}
}
