package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// TestMain allows file:// submodules for every git process started by the
// tests, including the ones the production code spawns.
func TestMain(m *testing.M) {
	env := map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "protocol.file.allow",
		"GIT_CONFIG_VALUE_0": "always",
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}

	os.Exit(m.Run())
}

// offline is the --no-fetch override the tests run with: the fixtures have no
// reachable remote.
var offline = false //nolint:gochecknoglobals // shared by every test context

// testCtx is a non-interactive context: the tests drive git directly and must
// never block on a prompt.
func testCtx() *run.Ctx {
	return &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}, FetchFlag: &offline}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "--quiet", "--initial-branch=develop")
	git(t, dir, "config", "user.email", "rt@example.com")
	git(t, dir, "config", "user.name", "repo-tools test")
}

// commitOn adds a commit with a fixed commit date, for date-window tests.
func commitOn(t *testing.T, dir, date, msg string) string {
	t.Helper()

	stamp := date + "T12:00:00+00:00"
	cmd := exec.CommandContext(t.Context(), "git", "commit", "--quiet", "-a", "-m", msg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_DATE="+stamp,
		"TZ=UTC",
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit %q: %v\n%s", msg, err, out)
	}

	return git(t, dir, "rev-parse", "HEAD")
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

// fixture builds a submodule with develop and release-1.0 branches and a
// parent whose release-1.0 is frozen onto the submodule's release branch,
// mirroring the state deps freeze leaves behind.
type fixture struct {
	sub, parent string
	subRelease  string // submodule commit on release-1.0
	subDevelop  string // submodule commit on develop
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	parent := filepath.Join(root, "parent")

	initRepo(t, sub)
	writeFile(t, sub, "lib.txt", "v1\n")
	commit(t, sub, "sub: initial")
	git(t, sub, "branch", "release-1.0")

	git(t, sub, "checkout", "--quiet", "release-1.0")
	writeFile(t, sub, "lib.txt", "v1-hotfix\n")
	subRelease := commit(t, sub, "sub: release hotfix")

	git(t, sub, "checkout", "--quiet", "develop")
	writeFile(t, sub, "lib.txt", "v2\n")
	subDevelop := commit(t, sub, "sub: develop feature")
	git(t, sub, "branch", "next")

	initRepo(t, parent)
	writeFile(t, parent, "app.txt", "app v1\n")
	commit(t, parent, "parent: initial")
	git(t, parent, "submodule", "add", "--quiet", "-b", "develop", sub, "sub")
	commit(t, parent, "parent: add submodule")

	// release-1.0 frozen onto the submodule's release branch
	git(t, parent, "checkout", "--quiet", "-b", "release-1.0")
	git(t, parent, "config", "-f", ".gitmodules", "submodule.sub.branch", "release-1.0")
	git(t, parent, "submodule", "sync", "--quiet", "--", "sub")
	git(t, parent, "submodule", "update", "--remote", "--", "sub")
	commit(t, parent, "parent: freeze deps for release-1.0")

	return fixture{sub: sub, parent: parent, subRelease: subRelease, subDevelop: subDevelop}
}

// project builds the config entry for the fixture through config.Load, so the
// resolved directory behind Dir() is set the way the real commands expect.
func (f fixture) project(t *testing.T) *config.Project {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "projects:\n" +
		"  - name: parent\n" +
		"    git: " + f.sub + "\n" +
		"    project_dir: " + f.parent + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	return cfg.Projects[0]
}

func subPin(t *testing.T, parent string) string {
	t.Helper()
	out := git(t, parent, "submodule", "status", "--", "sub")
	return strings.TrimLeft(strings.Fields(out)[0], "+-U")
}

// A develop commit that bumps the submodule pin must land on the release
// branch with the pin taken from the release freeze, not from develop.
func TestCherryPickResolvesSubmodulePinFromFreeze(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "submodule", "update", "--remote", "--", "sub")
	writeFile(t, f.parent, "app.txt", "app v2\n")
	devCommit := commit(t, f.parent, "parent: feature plus submodule bump")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--", "sub")

	if err := pickOne(testCtx(), r, p, "release-1.0", devCommit); err != nil {
		t.Fatalf("cherry-pick should have resolved the submodule conflict: %v", err)
	}

	if got := subPin(t, f.parent); got != f.subRelease {
		t.Errorf("submodule pin = %s, want the release head %s (develop head is %s)", got, f.subRelease, f.subDevelop)
	}
	body, err := os.ReadFile(filepath.Join(f.parent, "app.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "app v2\n" {
		t.Errorf("app.txt = %q, want the cherry-picked content", body)
	}
	msg := git(t, f.parent, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "cherry picked from commit "+devCommit) {
		t.Errorf("commit message lost the -x trailer:\n%s", msg)
	}
	if out := git(t, f.parent, "status", "--porcelain"); out != "" {
		t.Errorf("working tree not clean after the pick:\n%s", out)
	}
}

// A develop commit that also rewrites .gitmodules must not unfreeze the
// release branch: the release version of .gitmodules wins.
func TestCherryPickKeepsReleaseGitmodules(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "config", "-f", ".gitmodules", "submodule.sub.branch", "next")
	git(t, f.parent, "submodule", "update", "--remote", "--", "sub")
	devCommit := commit(t, f.parent, "parent: retarget submodule branch")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--", "sub")

	if err := pickOne(testCtx(), r, p, "release-1.0", devCommit); err != nil {
		t.Fatalf("pick failed: %v", err)
	}

	branch, err := r.SubmoduleBranch("sub")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "release-1.0" {
		t.Errorf(".gitmodules branch = %q, want release-1.0", branch)
	}
	if got := subPin(t, f.parent); got != f.subRelease {
		t.Errorf("submodule pin = %s, want %s", got, f.subRelease)
	}
}

// A conflict outside submodules is a human's job: the pick stays in progress.
func TestCherryPickStopsOnRegularConflict(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "app from develop\n")
	devCommit := commit(t, f.parent, "parent: develop edit")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.parent, "app.txt", "app from release\n")
	commit(t, f.parent, "parent: release edit")

	err := pickOne(testCtx(), r, p, "release-1.0", devCommit)
	if err == nil {
		t.Fatal("expected the pick to stop on a regular conflict")
	}
	if !strings.Contains(err.Error(), "manual conflict resolution") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.parent, ".git", "CHERRY_PICK_HEAD")); statErr != nil {
		t.Error("the cherry-pick should have been left in progress for manual resolution")
	}
	git(t, f.parent, "cherry-pick", "--abort")
}

// Picking a commit whose change is already on the release branch is a
// warning, not a failure.
func TestCherryPickSkipsEmptyPick(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "shared fix\n")
	devCommit := commit(t, f.parent, "parent: shared fix")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.parent, "app.txt", "shared fix\n")
	commit(t, f.parent, "parent: same fix applied directly")
	before := git(t, f.parent, "rev-parse", "HEAD")

	if err := pickOne(testCtx(), r, p, "release-1.0", devCommit); err != nil {
		t.Fatalf("an already-applied commit should be skipped, got: %v", err)
	}
	if after := git(t, f.parent, "rev-parse", "HEAD"); after != before {
		t.Error("an empty pick should not create a commit")
	}
}

// alreadyPicked and candidates rely on the -x trailer written above.
func TestAlreadyPickedReadsTrailers(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "app v2\n")
	devCommit := commit(t, f.parent, "parent: feature")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	if err := pickOne(testCtx(), r, p, "release-1.0", devCommit); err != nil {
		t.Fatal(err)
	}

	// alreadyPicked reads the origin refs of both branches, so give the repo them.
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "HEAD")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")
	picked, err := alreadyPicked(r, p, "release-1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !picked[devCommit] {
		t.Errorf("commit %s not recognised as already picked, got %v", devCommit, picked)
	}
}

// A branch created in the submodule's origin after the parent cloned it is
// only reachable once the submodule itself is fetched.
func TestUpdateSubmoduleRemoteFetchesNewBranch(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	// appears in the submodule origin only now, after the clone
	git(t, f.sub, "branch", "hotfix", "release-1.0")
	git(t, f.parent, "config", "-f", ".gitmodules", "submodule.sub.branch", "hotfix")

	rctx := &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}}
	if err := updateSubmoduleRemote(rctx, nil, r, "sub", "hotfix"); err != nil {
		t.Fatalf("the submodule should have been fetched before --remote: %v", err)
	}

	if got := subPin(t, f.parent); got != f.subRelease {
		t.Errorf("submodule pin = %s, want the hotfix head %s", got, f.subRelease)
	}
}

// A branch that exists nowhere is reported plainly, not as a raw git failure.
func TestUpdateSubmoduleRemoteReportsMissingBranch(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	err := updateSubmoduleRemote(testCtx(), nil, r, "sub", "nope")
	if err == nil {
		t.Fatal("expected an error for a branch that does not exist")
	}

	if !strings.Contains(err.Error(), "origin/nope does not exist") {
		t.Errorf("unclear error: %v", err)
	}
}

// Explicit hashes are checked against the release branch before anything runs:
// an already-picked commit is dropped with a warning, not handed to git.
func TestResolveShasSkipsAlreadyPicked(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "app v2\n")
	first := commit(t, f.parent, "parent: first")
	writeFile(t, f.parent, "other.txt", "other\n")
	second := commit(t, f.parent, "parent: second")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	if err := pickOne(testCtx(), r, p, "release-1.0", first); err != nil {
		t.Fatal(err)
	}

	// resolveShas reads the origin refs of both branches.
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "HEAD")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")

	got, err := resolveShas(r, p, "release-1.0", []string{first, second})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != second {
		t.Errorf("got %v, want only the unpicked commit %s", got, second)
	}
}

func TestResolveShasFailsWhenEverythingIsAlreadyThere(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "app v2\n")
	only := commit(t, f.parent, "parent: only")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	if err := pickOne(testCtx(), r, p, "release-1.0", only); err != nil {
		t.Fatal(err)
	}

	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "HEAD")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")

	_, err := resolveShas(r, p, "release-1.0", []string{only})
	if err == nil {
		t.Fatal("expected an error when nothing is left to pick")
	}

	if !strings.Contains(err.Error(), "already in release-1.0") {
		t.Errorf("unclear error: %v", err)
	}
}

// A commit that is not in the release branch must survive the check: git cherry
// looks at whole histories unless it is limited to the single commit.
func TestResolveShasKeepsUnpickedCommit(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "app v2\n")
	fresh := commit(t, f.parent, "parent: fresh")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "HEAD")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")

	got, err := resolveShas(r, p, "release-1.0", []string{fresh})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != fresh {
		t.Errorf("got %v, want %s kept", got, fresh)
	}
}

// --since is handed to git log, so only commits from that date onwards are
// offered as candidates.
func TestCandidatesForDateWindow(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "old.txt", "old\n")
	git(t, f.parent, "add", "-A")
	old := commitOn(t, f.parent, "2026-01-10", "feat/AB-1: old work")
	writeFile(t, f.parent, "new.txt", "new\n")
	git(t, f.parent, "add", "-A")
	recent := commitOn(t, f.parent, "2026-08-10", "feat/AB-2: recent work")

	// candidatesFor reads origin refs, so publish both branches locally.
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")

	all, err := candidatesFor(r, p, "release-1.0", mustFilter(t, nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	if len(all.Picking) != 2 {
		t.Fatalf("without a window: got %d candidates, want 2", len(all.Picking))
	}

	since, err := newPickFilter(nil, nil, "2026-06-01", "")
	if err != nil {
		t.Fatal(err)
	}

	windowed, err := candidatesFor(r, p, "release-1.0", since)
	if err != nil {
		t.Fatal(err)
	}

	if len(windowed.Picking) != 1 || windowed.Picking[0].SHA != recent {
		t.Fatalf("with --since: got %d candidates %v, want only %s", len(windowed.Picking), windowed.Picking, recent)
	}

	if windowed.Picking[0].Subject != "feat/AB-2: recent work" {
		t.Errorf("wrong commit kept: %q", windowed.Picking[0].Subject)
	}

	if windowed.Picking[0].SHA == old {
		t.Error("the commit outside the window leaked in")
	}
}

// A pick can fail without any conflict — a dirty index, for one. The tool must
// pass git's own words through instead of announcing a conflict.
func TestPickOneReportsNonConflictFailure(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "app v2\n")
	devCommit := commit(t, f.parent, "parent: feature")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")

	// An uncommitted change to the same file blocks the pick.
	writeFile(t, f.parent, "app.txt", "local edit\n")
	git(t, f.parent, "add", "app.txt")

	err := pickOne(testCtx(), r, p, "release-1.0", devCommit)
	if err == nil {
		t.Fatal("expected the pick to fail")
	}

	if strings.Contains(err.Error(), "manual conflict resolution") {
		t.Errorf("a blocked pick is not a conflict: %v", err)
	}

	if !strings.Contains(err.Error(), "failed in parent") {
		t.Errorf("error should carry git's reason: %v", err)
	}
}

// Unpushed local commits must stop a batch command: committing on top of them
// would push someone's unrelated work along with the change.
func TestCheckoutTrackingRefusesUnpushedWork(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "HEAD")

	if err := checkoutTracking(r, "release-1.0"); err != nil {
		t.Fatalf("a branch in step with origin must pass: %v", err)
	}

	writeFile(t, f.parent, "app.txt", "local work\n")
	commit(t, f.parent, "parent: unpushed work")

	err := checkoutTracking(r, "release-1.0")
	if err == nil {
		t.Fatal("expected unpushed commits to be refused")
	}

	if !strings.Contains(err.Error(), "not on origin") {
		t.Errorf("unclear error: %v", err)
	}
}
