package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// fakeGlab puts a glab on PATH that records its calls and answers the two
// requests the merge request mode makes: the lookup of an open request for a
// branch, which finds one once "mr create" has run, and the creation itself.
// It returns the path of the call log.
func fakeGlab(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")

	script := `#!/bin/sh
echo "$*" >> "$GLAB_LOG"
case "$1 $2" in
  "api projects/:id/merge_requests/"*)
    state=opened; [ -f "$GLAB_LOG.state" ] && state=$(cat "$GLAB_LOG.state")
    target=develop; [ -f "$GLAB_LOG.target" ] && target=$(cat "$GLAB_LOG.target")
    target=${GLAB_RETARGET:-$target}
    merged=
    if [ "${GLAB_AUTO_MERGE:-}" = 1 ]; then
      source=$(cat "$GLAB_LOG.source")
      merged=$(git rev-parse "$source") || exit 1
      if [ "${GLAB_SQUASH:-}" = 1 ]; then
        merged=$(git commit-tree "$source^{tree}" -p "$target" -m squash) || exit 1
      fi
      git push origin "$merged:refs/heads/$target" >/dev/null 2>&1 || exit 1
      if [ "${GLAB_REWIND:-}" = 1 ]; then
        git push --force origin "$target:refs/heads/$target" >/dev/null 2>&1 || exit 1
      fi
      state=merged
    fi
    printf '{"state":"%s","target_branch":"%s","merge_commit_sha":"%s"}\n' "$state" "$target" "$merged" ;;
  "api projects/:id/merge_requests"*)
    if [ -f "$GLAB_LOG.mr" ]; then
      printf '[{"web_url":"https://gl/mr/1","target_branch":"%s","source_project_id":1,"target_project_id":1}]\n' \
        "$(cat "$GLAB_LOG.target")"
    else echo '[]'; fi ;;
  "mr create")
    printf '%s\n' "$4" > "$GLAB_LOG.source"
    printf '%s\n' "$6" > "$GLAB_LOG.target"
    touch "$GLAB_LOG.mr"; echo "Creating merge request for $4 into $6"; echo "https://gl/mr/1"
    echo "A new version of glab is available" >&2 ;;
  "api projects/:id/pipelines?"*)
    # no pipeline on the branch, one on the merge request
    case "$2" in
      *source=merge_request_event*) echo '[{"id":9,"status":"success","web_url":"https://gl/p/9"}]' ;;
      *) echo '[]' ;;
    esac ;;
  "config get")
    [ "$4" = "--host" ] || { echo "usage: glab config get <key> [--flags]"; echo "  -h --help"; exit 0; }
    echo "A new version of glab is available" >&2 # the update check, once a day
    echo "glpat-from-glab" ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "glab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GLAB_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

// mrFixture is the submodule fixture with a bare origin for the parent, on
// develop, plus a config with mr: always and ci: gitlab.
func mrFixture(t *testing.T) (fixture, *run.Ctx, *config.Project) {
	t.Helper()

	f := newFixture(t)

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	bare := filepath.Join(t.TempDir(), "parent.git")
	git(t, f.parent, "clone", "--bare", "--quiet", f.parent, bare)
	git(t, f.parent, "remote", "add", "origin", bare)
	git(t, f.parent, "fetch", "--quiet", "origin")
	git(t, f.parent, "branch", "--set-upstream-to=origin/develop", "develop")

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "projects:\n" +
		"  - name: parent\n" +
		"    git: " + f.sub + "\n" +
		"    ci: gitlab\n" +
		"    mr: always\n" +
		"    project_dir: " + f.parent + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Confirm = config.ConfirmNever

	// NoCI: the pipeline watch would ask the fake glab questions it cannot
	// answer; the request itself is what is under test.
	rctx := &run.Ctx{Cfg: cfg, FetchFlag: &offline, NoCI: true}

	return f, rctx, cfg.Projects[0]
}

// In merge request mode the commit never touches the target branch: it goes
// to the command's own branch, the request is opened into the target, and
// the working copy is back on the target branch, clean, when it is over.
//
//nolint:paralleltest // puts a fake glab on PATH and captures os.Stdout
func TestCommitAndPushOpensAMergeRequest(t *testing.T) {
	log := fakeGlab(t)
	f, rctx, p := mrFixture(t)
	r := gitx.Repo{Dir: f.parent}

	before := git(t, f.parent, "rev-parse", "origin/develop")
	branch := "rt/deps-submodules/develop/" + time.Now().Format(time.DateOnly)

	writeFile(t, f.parent, "app.txt", "bumped\n")

	var err error

	out := captureOutput(t, func() {
		err = commitAndPush(rctx, r, p, cmdDepsSubmodules, "develop", "chore: bump", "nothing", false)
	})
	if err != nil {
		t.Fatalf("commitAndPush: %v", err)
	}

	if got := git(t, f.parent, "rev-parse", "origin/develop"); got != before {
		t.Errorf("origin/develop moved from %s to %s; the commit must go to the request branch", before, got)
	}

	pushed := git(t, f.parent, "rev-parse", "origin/"+branch)
	if subject := git(t, f.parent, "log", "-1", "--format=%s", pushed); subject != "chore: bump" {
		t.Errorf("origin/%s is at %q, want the bump commit", branch, subject)
	}

	calls, _ := os.ReadFile(log)
	create := "mr create --source-branch " + branch + " --target-branch develop --title chore: bump"

	if !strings.Contains(string(calls), create) {
		t.Errorf("glab was not asked to open the request:\n%s", calls)
	}

	// With no description in the config the body is the system's business.
	if strings.Contains(string(calls), "--description") {
		t.Errorf("no body was configured, none should be passed:\n%s", calls)
	}

	if !strings.Contains(out, "merge request: https://gl/mr/1") {
		t.Errorf("the request URL is not reported:\n%s", out)
	}

	if !strings.Contains(out, "Commit, push and open MR into develop?") && rctx.Interactive() {
		t.Errorf("the question should cover the request:\n%s", out)
	}

	if cur := git(t, f.parent, "rev-parse", "--abbrev-ref", "HEAD"); cur != "develop" {
		t.Errorf("the working copy was left on %s, want develop", cur)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status != "" {
		t.Errorf("the working copy is not clean afterwards:\n%s", status)
	}

	// A rerun on the same day rewrites the branch with one fresh commit and
	// updates the open request instead of opening a second one.
	writeFile(t, f.parent, "app.txt", "bumped again\n")

	captureOutput(t, func() {
		err = commitAndPush(rctx, r, p, cmdDepsSubmodules, "develop", "chore: bump again", "nothing", false)
	})

	if err != nil {
		t.Fatalf("second commitAndPush: %v", err)
	}

	if n := git(t, f.parent, "rev-list", "--count", "origin/develop..origin/"+branch); n != "1" {
		t.Errorf("the rerun should leave one commit on the branch, got %s", n)
	}

	calls, _ = os.ReadFile(log)
	if strings.Count(string(calls), "mr create") != 1 {
		t.Errorf("the rerun must reuse the open request:\n%s", calls)
	}
}

func TestMatchingRequestChecksLaterPagesAndSkipsForks(t *testing.T) {
	t.Parallel()

	request := &mergeRequest{Branch: "shared", Target: "release"}
	fork := `[{"web_url":"fork","target_branch":"dev","source_project_id":2,"target_project_id":1}]`
	local := `[{"web_url":"local","target_branch":"release","source_project_id":1,"target_project_id":1}]`

	if got, err := matchingRequest(fork+"\n"+local, request); err != nil || got != "local" {
		t.Errorf("GitLab lookup = %q, %v", got, err)
	}

	local = `[{"html_url":"pr","base":{"ref":"release","repo":{"id":1}},"head":{"repo":{"id":1}}}]`
	fork = `[{"html_url":"fork","base":{"ref":"dev","repo":{"id":1}},"head":{"repo":{"id":2}}}]`

	if got, err := matchingRequest(fork+"\n"+local, request); err != nil || got != "pr" {
		t.Errorf("GitHub lookup = %q, %v", got, err)
	}

	conflict := strings.Replace(local, "release", "other", 1)
	if _, err := matchingRequest(local+"\n"+conflict, request); !errors.Is(err, errMRTargetConflict) {
		t.Errorf("second open PR into a different base: %v", err)
	}

	for _, broken := range []string{"", "]", "[]]", "null"} {
		if _, err := matchingRequest(broken, request); err == nil {
			t.Errorf("invalid request list %q treated as an empty result", broken)
		}
	}
}

func TestMRBranchCannotOverwriteTarget(t *testing.T) {
	fakeGlab(t)
	t.Setenv("GLAB_AUTO_MERGE", "1")
	f, rctx, p := mrFixture(t)
	p.MRBranch = "{branch}"
	before := git(t, f.parent, "rev-parse", "origin/develop")
	writeFile(t, f.parent, "app.txt", "bump\n")

	err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, "develop", "freeze", "nothing", true)

	if got := git(t, f.parent, "rev-parse", "origin/develop"); got != before {
		t.Fatalf("MR mode pushed directly to develop: %s", got)
	}

	if err == nil {
		t.Fatal("expected identical MR branches to fail")
	}
}

//nolint:paralleltest // fakeGlab sets PATH
func TestMRSourceCannotBeAnotherProjectBranch(t *testing.T) {
	for _, branches := range [][2]string{{"develop", "release-1.0"}, {"release-1.0", "develop"}} {
		fakeGlab(t)

		source, target := branches[0], branches[1]
		f, rctx, p := mrFixture(t)
		p.MRBranch = source

		git(t, f.parent, "checkout", "--recurse-submodules", target)
		writeFile(t, f.parent, "app.txt", "bump\n")

		before := git(t, f.parent, "rev-parse", "origin/"+source)
		err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, target, "bump", "nothing", false)

		if got := git(t, f.parent, "rev-parse", "origin/"+source); got != before || err == nil {
			t.Errorf("MR from %s into %s: source was %s, now %s, error %v", source, target, before, got, err)
		}
	}
}

//nolint:paralleltest // fakeGlab sets PATH
func TestMRRerunCannotRewriteRequestIntoAnotherTarget(t *testing.T) {
	fakeGlab(t)
	f, rctx, p := mrFixture(t)
	p.MRBranch = "rt/shared"

	writeFile(t, f.parent, "app.txt", "first bump\n")

	if err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, "develop", "first", "nothing", false); err != nil {
		t.Fatal(err)
	}

	before := git(t, f.parent, "rev-parse", "origin/rt/shared")
	git(t, f.parent, "checkout", "--recurse-submodules", "release-1.0")
	writeFile(t, f.parent, "app.txt", "second bump\n")

	err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, "release-1.0", "second", "nothing", false)

	if got := git(t, f.parent, "rev-parse", "origin/rt/shared"); got != before {
		t.Fatalf("rerun rewrote the source of the open MR into develop: %s", got)
	}

	if err == nil {
		t.Fatal("expected the conflicting target to fail")
	}
}

//nolint:paralleltest // fakeGlab sets PATH
func TestMRRerunPreservesRetainedSource(t *testing.T) {
	t.Run("single-branch", func(t *testing.T) { checkMRRerun(t, true) })
	t.Run("all-branches", func(t *testing.T) { checkMRRerun(t, false) })
}

func checkMRRerun(t *testing.T, singleBranch bool) {
	t.Helper()

	log := fakeGlab(t)
	f, rctx, p := mrFixture(t)
	rctx.FetchFlag = nil
	p.MRBranch = "rt/shared"

	if singleBranch {
		git(t, f.parent, "config", "remote.origin.fetch", "+refs/heads/develop:refs/remotes/origin/develop")
	}

	for _, change := range []string{"first", "second"} {
		writeFile(t, f.parent, "app.txt", change+"\n")

		err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, "develop", change, "nothing", false)
		if err != nil {
			t.Fatalf("%s bump: %v", change, err)
		}
	}

	origin := git(t, f.parent, "remote", "get-url", "origin")
	if got := git(t, origin, "show", "rt/shared:app.txt"); got != "second" {
		t.Errorf("rerun source contains %q", got)
	}

	// Hosts can retain or delete the source when the previous request closes.
	for _, keep := range []bool{true, false} {
		if err := os.Remove(log + ".mr"); err != nil {
			t.Fatal(err)
		}

		change := "reuse retained source"

		if !keep {
			git(t, origin, "update-ref", "-d", "refs/heads/rt/shared")

			git(t, f.parent, "fetch", "--prune", "origin")

			change = "recreate deleted source"
		}

		writeFile(t, f.parent, "app.txt", change+"\n")

		if err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, "develop", change, "nothing", false); err != nil {
			t.Fatalf("%s: %v", change, err)
		}

		if got := git(t, origin, "show", "rt/shared:app.txt"); got != change {
			t.Errorf("remote content = %q, want %q", got, change)
		}
	}

	if err := os.Remove(log + ".mr"); err != nil {
		t.Fatal(err)
	}

	other := git(t, origin, "rev-parse", "release-1.0")
	git(t, origin, "update-ref", "refs/heads/rt/shared", other)
	git(t, f.parent, "fetch", "--prune", "origin")
	writeFile(t, f.parent, "app.txt", "another bump\n")

	err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, "develop", "another bump", "nothing", false)
	if !errors.Is(err, errMRSourceChanged) || git(t, origin, "rev-parse", "rt/shared") != other {
		t.Errorf("remote source changed outside the run: %v", err)
	}
}

// --wait-merge holds the command even without later dependent projects, then
// refreshes the target checkout for the next flow step's working-copy checks.
func TestCommitAndPushWaitMergeRefreshesTarget(t *testing.T) {
	log := fakeGlab(t)
	t.Setenv("GLAB_AUTO_MERGE", "1")
	t.Setenv("GLAB_SQUASH", "1")

	f, rctx, p := mrFixture(t)
	rctx.FetchFlag = nil
	rctx.Cfg.MergeRequest.Wait = config.MRWaitNone
	r := gitx.Repo{Dir: f.parent}

	writeFile(t, f.sub, "lib.txt", "v3\n")
	newPin := commit(t, f.sub, "sub: next develop change")
	git(t, f.parent, "submodule", "update", "--remote", "--", "sub")
	git(t, f.parent, "config", "submodule.sub.update", "none")
	writeFile(t, f.parent, "app.txt", "merged bump\n")

	var err error

	out := captureOutput(t, func() {
		err = commitAndPush(rctx, r, p, cmdDepsSubmodules, "develop", "chore: bump", "nothing", true)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "waiting for https://gl/mr/1 to be merged (wait: --wait-merge)") {
		t.Errorf("wait was not reported: %s", out)
	}

	if got := git(t, f.parent, "rev-parse", "HEAD"); got != git(t, f.parent, "rev-parse", "origin/develop") {
		t.Errorf("target checkout %s did not catch up with origin/develop", got)
	}

	content, err := os.ReadFile(filepath.Join(f.parent, "app.txt"))
	if err != nil || string(content) != "merged bump\n" {
		t.Errorf("merged file in working copy = %q, %v", content, err)
	}

	if pin := git(t, filepath.Join(f.parent, "sub"), "rev-parse", "HEAD"); pin != newPin {
		t.Errorf("submodule checkout = %s, want merged pin %s", pin, newPin)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status != "" {
		t.Errorf("target checkout is dirty: %s", status)
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "api projects/:id/merge_requests/1") {
		t.Errorf("MR state was not queried: %s", calls)
	}
}

func TestWaitMergeDetectsRewoundTarget(t *testing.T) {
	fakeGlab(t)
	t.Setenv("GLAB_AUTO_MERGE", "1")
	t.Setenv("GLAB_REWIND", "1")

	f, rctx, p := mrFixture(t)
	rctx.FetchFlag = nil

	writeFile(t, f.parent, "app.txt", "must land before the next step\n")

	err := commitAndPush(rctx, repoOf(p), p, cmdDepsFreeze, "develop", "freeze", "nothing", true)
	if err == nil {
		t.Fatal("wait succeeded after origin/develop lost the merged commit")
	}
}

func TestRefreshMergedBranchReadsTheRecordedPin(t *testing.T) {
	t.Parallel()

	f, rctx, p := mrFixture(t)
	rctx.FetchFlag = nil

	git(t, f.parent, "checkout", "-b", "merged-bump")
	git(t, filepath.Join(f.parent, "sub"), "checkout", f.subRelease)
	landed := commit(t, f.parent, "bump the pin")
	writeFile(t, f.parent, "later.txt", "another change after the merge\n")
	commit(t, f.parent, "advance target")
	git(t, f.parent, "push", "origin", "HEAD:develop")
	git(t, f.parent, "checkout", "--recurse-submodules", "develop")
	git(t, f.parent, "config", "submodule.sub.update", "none")

	if err := refreshMergedBranch(rctx, repoOf(p), p, "develop", landed); err != nil {
		t.Fatal(err)
	}

	if got := subPin(t, f.parent); got != f.subRelease {
		t.Errorf("checkout after merge = %s, want %s", got, f.subRelease)
	}
}

func TestRefreshMergedBranchRemovesObsoleteSubmodule(t *testing.T) {
	t.Parallel()

	f, rctx, p := mrFixture(t)
	rctx.FetchFlag = nil

	git(t, f.parent, "checkout", "-b", "remove-submodule")
	git(t, f.parent, "rm", "sub")
	landed := commit(t, f.parent, "drop submodule")
	git(t, f.parent, "push", "origin", "HEAD:develop")
	git(t, f.parent, "checkout", "--recurse-submodules", "develop")

	if err := refreshMergedBranch(rctx, repoOf(p), p, "develop", landed); err != nil {
		t.Fatal(err)
	}

	if got := git(t, f.parent, "status", "--porcelain"); got != "" {
		t.Errorf("refresh left an obsolete worktree: %s", got)
	}
}

func TestRefreshMergedBranchPreservesWorkStartedDuringWait(t *testing.T) {
	t.Parallel()

	f, rctx, p := mrFixture(t)
	rctx.FetchFlag = nil

	git(t, f.parent, "checkout", "-b", "operator-work")
	landed := git(t, f.parent, "rev-parse", "origin/develop")

	if err := refreshMergedBranch(rctx, repoOf(p), p, "develop", landed); !errors.Is(err, errMRCheckoutChanged) {
		t.Fatalf("refresh on another branch: %v", err)
	}

	if got := git(t, f.parent, "branch", "--show-current"); got != "operator-work" {
		t.Fatalf("refresh switched to %s", got)
	}

	git(t, f.parent, "checkout", "develop")
	git(t, filepath.Join(f.parent, "sub"), "checkout", f.subRelease)
	git(t, f.parent, "config", "submodule.sub.ignore", "all")

	if err := refreshMergedBranch(rctx, repoOf(p), p, "develop", landed); !errors.Is(err, errDirty) {
		t.Fatalf("refresh with a local pin: %v", err)
	}

	if got := subPin(t, f.parent); got != f.subRelease {
		t.Errorf("refresh replaced the operator's pin with %s", got)
	}
}

func TestGitHubWaitUsesMatchingSourceAndTarget(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
echo "CLI update available" >&2
case "$1" in
  api)
    [ "$2" = 'repos/{owner}/{repo}/pulls?state=open&head={owner}:rt%2Fa%2Bb%26c' ] && [ "$3" = --paginate ] || exit 1
    echo '[{"html_url":"pr","base":{"ref":"develop","repo":{"id":1}},"head":{"repo":{"id":1}}}]' ;;
  pr)
    [ "$2 $3 $4 $5" = "view pr --json state,baseRefName,mergeCommit" ] || exit 1
    echo '{"state":"MERGED","baseRefName":"integration"}' ;;
  *) exit 1 ;;
esac
`

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := &config.Project{CI: config.CIGitHub}
	r := gitx.Repo{Dir: dir}
	m := &mergeRequest{Branch: "rt/a+b&c", Target: "develop"}

	link, err := existingMergeRequest(r, p, m)
	if err != nil || link != "pr" {
		t.Fatalf("open PR lookup = %q, %v", link, err)
	}

	if _, _, err := mrQuery(r, p, link, m.Target); !errors.Is(err, errMRTargetConflict) {
		t.Errorf("retargeted GitHub PR: %v", err)
	}
}

func TestWaitMergeRejectsRetargetedRequest(t *testing.T) {
	fakeGlab(t)
	t.Setenv("GLAB_AUTO_MERGE", "1")
	t.Setenv("GLAB_RETARGET", "integration")

	f, rctx, p := mrFixture(t)
	rctx.FetchFlag = nil

	writeFile(t, f.parent, "app.txt", "merge into develop\n")

	err := commitAndPush(rctx, repoOf(p), p, cmdDepsSubmodules, "develop", "bump", "nothing", true)
	if err == nil || !strings.Contains(err.Error(), "integration") {
		t.Fatalf("MR landed on integration while waiting for develop: %v", err)
	}
}

// The freeze step uses the same wait as deps submodules: the next step must
// see the merged release commit and the submodule pin it records.
func TestFreezeDepsWaitMergeRefreshesRelease(t *testing.T) {
	fakeGlab(t)
	t.Setenv("GLAB_AUTO_MERGE", "1")

	f, rctx, p := mrFixture(t)
	on := true
	p.DepsFreeze = &on
	p.Submodules = []config.Submodule{{Path: "sub", FreezeTo: "release-1.0"}}
	// The clone's default fetch does not refresh origin/release-1.0.
	git(t, f.parent, "config", "remote.origin.fetch", "+refs/heads/develop:refs/remotes/origin/develop")

	rctx.FetchFlag = nil
	rctx.Cfg.MergeRequest.Wait = config.MRWaitNone

	git(t, f.sub, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.sub, "lib.txt", "next release pin\n")
	newPin := commit(t, f.sub, "sub: next release change")

	var err error

	out := captureOutput(t, func() {
		err = freezeDeps(rctx, p, "release-1.0", true, false, true)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "waiting for https://gl/mr/1 to be merged (wait: --wait-merge)") {
		t.Errorf("freeze did not wait for its MR: %s", out)
	}

	if head := git(t, f.parent, "rev-parse", "HEAD"); head != git(t, f.parent, "rev-parse", "origin/release-1.0") {
		t.Errorf("release checkout %s did not catch up with origin", head)
	}

	if pin := git(t, filepath.Join(f.parent, "sub"), "rev-parse", "HEAD"); pin != newPin {
		t.Errorf("submodule checkout = %s, want merged pin %s", pin, newPin)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status != "" {
		t.Errorf("release checkout is dirty: %s", status)
	}
}

// The plan says where the commit goes and what the review will ask; a
// project that cannot open a request is reported by repo check, not at the
// push.
func TestMergeRequestPlanAndCheck(t *testing.T) {
	t.Parallel()

	p := &config.Project{Name: "api", CI: config.CIGitLab, MR: config.MRAlways}
	rctx := &run.Ctx{Cfg: &config.Config{MergeRequest: config.MergeRequest{
		Branch: "rt/{command}/{branch}/{date}", Title: "{message}",
	}}}

	lines := commitPlanLines(rctx, p, cmdChangelogUpdate, "release-1.0", "files changed",
		"chore: changelog\n\nbody", false)

	want := "commit to rt/changelog-update/release-1.0/" + time.Now().Format(time.DateOnly) +
		", push, open MR into release-1.0 if files changed"
	if !strings.Contains(lines[0], want) {
		t.Errorf("plan line %q lacks %q", lines[0], want)
	}

	// A branch template of the project's own wins over the global one.
	p.MRBranch = "own/{command}"
	if own := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "x"); own.Branch != "own/changelog-update" {
		t.Errorf("mr_branch: %q", own.Branch)
	}

	p.MRBranch = ""

	m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "chore: changelog\n\nbody")
	if m.Title != "chore: changelog" {
		t.Errorf("the title should be the commit subject, got %q", m.Title)
	}

	if m.Description != "" || len(m.args("--description")) != 0 {
		t.Errorf("nothing configured, nothing to pass: %+v", m)
	}

	// The body is a template; the people come from the config, the project's
	// own list first.
	rctx.Cfg.MergeRequest.Description = "{command} on {branch} by rt"
	rctx.Cfg.MergeRequest.Assignees = []string{"lead"}
	rctx.Cfg.MergeRequest.Reviewers = []string{"one", "two"}
	p.MRReviewers = []string{"owner"}

	m = planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg")
	got := strings.Join(m.args("--description"), " ")
	want = "--description changelog-update on release-1.0 by rt --assignee lead --reviewer owner"

	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	if !strings.Contains(m.review(), "assigned to lead") || !strings.Contains(m.review(), "reviewed by owner") {
		t.Errorf("the review should name the people:\n%s", m.review())
	}

	if issues := mrCheck(rctx, p); len(issues) != 0 {
		t.Errorf("ci: gitlab can open requests: %v", issues)
	}

	// The wait is in the plan when the config always waits, settle included.
	settle := 30
	rctx.Cfg.MergeRequest.Wait = config.MRWaitAlways
	rctx.Cfg.MergeRequest.SettleSeconds = &settle

	lines = commitPlanLines(rctx, p, cmdChangelogUpdate, "release-1.0", "files changed", "msg", false)
	if !strings.Contains(lines[0], "then wait for the MR to be merged (wait: always) and 30s more") {
		t.Errorf("plan line %q lacks the wait", lines[0])
	}

	rctx.Cfg.MergeRequest.Wait = config.MRWaitNone
	if got := waitPlan(rctx, p, false); got != "" {
		t.Errorf("wait: none must plan no wait, got %q", got)
	}

	p.CI = config.CINone
	if issues := mrCheck(rctx, p); len(issues) != 1 || !strings.Contains(issues[0], "ci is none") {
		t.Errorf("ci: none cannot open a request: %v", issues)
	}

	off := false
	rctx.MRFlag = &off

	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m != nil {
		t.Error("--no-mr must switch the mode off")
	}

	// mr: none is final: --mr does not pull the project in.
	on := true
	rctx.MRFlag = &on
	p.MR = config.MRNone

	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m != nil {
		t.Error("mr: none must hold under --mr")
	}

	// Unset: only the flag turns it on.
	p.MR = ""
	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m == nil {
		t.Error("--mr must turn the mode on for a project that leaves mr unset")
	}

	rctx.MRFlag = nil
	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m != nil {
		t.Error("without --mr an unset mr means no merge request")
	}
}

// The wait is for the projects still to come that pull this one in — as a
// submodule or as a pinned module — and only for those.
func TestMRDependentsLookAheadInTheRun(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	// A go project pinning the library through its make variable.
	gomod := filepath.Join(t.TempDir(), "gomod")
	initRepo(t, gomod)
	writeFile(t, gomod, "Makefile", "GO_DEPS_UPDATE_INTERNAL := "+f.sub+"@develop\n")
	commit(t, gomod, "gomod: pin the library")

	// A project with nothing to do with the library.
	other := filepath.Join(t.TempDir(), "other")
	initRepo(t, other)
	writeFile(t, other, "README", "alone\n")
	commit(t, other, "other: initial")

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "cmds: {deps: {go: [\"go mod tidy\"]}}\n" +
		"deps_pins:\n  go:\n    file: Makefile\n    var: GO_DEPS_UPDATE_INTERNAL\n" +
		"projects:\n" +
		"  - name: lib\n    git: " + f.sub + "\n    project_dir: " + f.sub + "\n" +
		"  - name: parent\n    git: " + f.parent + "\n    project_dir: " + f.parent + "\n" +
		"  - name: gomod\n    git: " + gomod + "\n    project_dir: " + gomod + "\n    deps: go\n" +
		"  - name: other\n    git: " + other + "\n    project_dir: " + other + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	rctx := &run.Ctx{Cfg: cfg, FetchFlag: &offline, Selection: cfg.Projects}
	lib, parent := cfg.Projects[0], cfg.Projects[1]

	if got := mrDependents(rctx, lib); strings.Join(got, ",") != "parent,gomod" {
		t.Errorf("lib's dependents = %v, want parent and gomod", got)
	}

	parent.IgnoreSubmodules = []string{"sub"}

	if got := mrDependents(rctx, lib); strings.Join(got, ",") != "gomod" {
		t.Errorf("lib's dependents with the parent's submodule ignored = %v, want gomod", got)
	}

	parent.Deps = "go"
	writeFile(t, f.parent, "Makefile", "GO_DEPS_UPDATE_INTERNAL := "+f.sub+"@develop\n")
	commit(t, f.parent, "parent: pin the library as a module too")

	if !dependsOn(rctx, parent, lib) {
		t.Error("a module pin still depends on lib when its submodule is ignored")
	}

	if got := mrDependents(rctx, parent); len(got) != 0 {
		t.Errorf("nothing depends on parent, got %v", got)
	}

	// Only the projects after this one count: a run over parent alone waits
	// for nobody.
	rctx.Selection = []*config.Project{parent, lib}
	if got := mrDependents(rctx, lib); len(got) != 0 {
		t.Errorf("parent runs before lib and cannot wait for it, got %v", got)
	}

	if got := mrWaitFor(rctx, lib, false); got != nil {
		t.Errorf("dependents: no wait without dependents, got %v", got)
	}

	if got := mrWaitFor(rctx, lib, true); len(got) != 1 || got[0] != "wait: --wait-merge" {
		t.Errorf("--wait-merge must override the dependent policy, got %v", got)
	}

	cfg.MergeRequest.Wait = config.MRWaitAlways

	if got := mrWaitFor(rctx, lib, false); len(got) != 1 {
		t.Errorf("always: wait whatever the run, got %v", got)
	}
}

// The wait holds the batch until the request is merged, then settles; a
// request closed without a merge fails the step, since the next project
// would pull a dev branch without the change.
//
//nolint:paralleltest // puts a fake glab on PATH and captures os.Stdout
func TestWaitMergedPollsUntilMerged(t *testing.T) {
	log := fakeGlab(t)
	f, rctx, p := mrFixture(t)
	r := gitx.Repo{Dir: f.parent}

	poll, settle := 1, 1
	rctx.Cfg.CIPollSeconds = &poll
	rctx.Cfg.MergeRequest.SettleSeconds = &settle

	// Merged a moment after the wait starts, so it has to poll more than once.
	go func() {
		time.Sleep(1500 * time.Millisecond)

		_ = os.WriteFile(log+".state", []byte("merged"), 0o644)
	}()

	var err error

	started := time.Now()

	out := captureOutput(t, func() {
		_, err = waitMerged(rctx, r, p, "https://gl/mr/7", "develop", []string{"api"})
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	if elapsed := time.Since(started); elapsed < 2*time.Second {
		t.Errorf("the wait returned after %s, before the merge plus the settle", elapsed)
	}

	wants := []string{"waiting for https://gl/mr/7 to be merged (needed by api)", "merged", "settling for 1s"}

	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "api projects/:id/merge_requests/7") {
		t.Errorf("the request was not asked by its iid:\n%s", calls)
	}

	// Closed without a merge: the change is not coming.
	if err := os.WriteFile(log+".state", []byte("closed"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureOutput(t, func() { _, err = waitMerged(rctx, r, p, "https://gl/mr/7", "develop", []string{"api"}) })

	if err == nil || !strings.Contains(err.Error(), "closed without being merged") {
		t.Errorf("a closed request must fail the wait, got %v", err)
	}
}

// The web form fills the body from the repository's template and the API
// does not, so a template the config names is read from the target branch,
// expanded and sent as the body.
//
//nolint:paralleltest // puts a fake glab on PATH and captures os.Stdout
func TestCommitAndPushFillsTheBodyFromTheTemplate(t *testing.T) {
	log := fakeGlab(t)
	f, rctx, p := mrFixture(t)
	r := gitx.Repo{Dir: f.parent}

	if err := os.MkdirAll(filepath.Join(f.parent, ".gitlab/merge_request_templates"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, f.parent, ".gitlab/merge_request_templates/Default.md",
		"#### Description\n{message}\n\n### CR\n- [ ] @lead\n")
	commit(t, f.parent, "parent: add the merge request template")
	git(t, f.parent, "push", "--quiet", "origin", "develop")

	rctx.Cfg.MergeRequest.Template = "Default"

	writeFile(t, f.parent, "app.txt", "bumped\n")

	var err error

	out := captureOutput(t, func() {
		err = commitAndPush(rctx, r, p, cmdDepsSubmodules, "develop", "chore: bump", "nothing", false)
	})
	if err != nil {
		t.Fatalf("commitAndPush: %v", err)
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--description #### Description\nchore: bump\n\n### CR\n- [ ] @lead") {
		t.Errorf("the expanded template was not sent as the body:\n%s", calls)
	}

	if !strings.Contains(out, "body from .gitlab/merge_request_templates/Default.md") {
		t.Errorf("the review should say where the body comes from:\n%s", out)
	}
}

// A template the config names but the branch lacks is a check finding and an
// error at the request, not a silently empty body.
func TestMissingMergeRequestTemplateIsReported(t *testing.T) {
	t.Parallel()

	p := newFixture(t).project(t) // a real clone, without a template
	p.CI, p.MR = config.CIGitLab, config.MRAlways

	rctx := ctxFor(p)
	rctx.Cfg.MergeRequest = config.MergeRequest{
		Branch: "rt/{command}/{branch}/{date}", Title: "{message}", Template: "Default",
	}

	if issues := mrCheck(rctx, p); len(issues) != 1 || !strings.Contains(issues[0], "Default.md is not on develop") {
		t.Errorf("a missing template should be reported: %v", issues)
	}

	m := planMergeRequest(rctx, p, cmdChangelogUpdate, "develop", "msg")
	if m.BodyErr == nil || m.Description != "" {
		t.Errorf("a missing template must not pass as an empty body: %+v", m)
	}

	if _, _, err := commitToMergeRequest(rctx, gitx.Repo{Dir: p.Dir()}, p, m, "msg"); err == nil ||
		!strings.Contains(err.Error(), "template not found") {
		t.Errorf("the request must refuse to open with the template missing, got %v", err)
	}
}
