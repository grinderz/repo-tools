package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// repoWithCommit returns an initialised repository with one commit on main.
func repoWithCommit(t *testing.T) Repo {
	t.Helper()

	dir := t.TempDir()
	run(t, dir, "init", "--quiet", "--initial-branch=main")
	run(t, dir, "config", "user.email", "rt@example.com")
	run(t, dir, "config", "user.name", "repo-tools test")

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, dir, "add", "-A")
	run(t, dir, "commit", "--quiet", "-m", "initial")

	return Repo{Dir: dir}
}

func TestHasCommits(t *testing.T) {
	empty := t.TempDir()
	run(t, empty, "init", "--quiet")

	if (Repo{Dir: empty}).HasCommits() {
		t.Error("a repository without commits must report none")
	}

	if !repoWithCommit(t).HasCommits() {
		t.Error("a repository with a commit must report so")
	}
}

func TestExistsAndIsRepo(t *testing.T) {
	r := repoWithCommit(t)
	if !r.Exists() || !r.IsRepo() {
		t.Error("a real repository must be recognised")
	}

	plain := Repo{Dir: t.TempDir()}
	if !plain.Exists() || plain.IsRepo() {
		t.Error("a plain directory exists but is not a repository")
	}

	if (Repo{Dir: filepath.Join(t.TempDir(), "nope")}).Exists() {
		t.Error("a missing directory must not report as existing")
	}
}

func TestIsCleanAndCurrentBranch(t *testing.T) {
	r := repoWithCommit(t)

	branch, err := r.CurrentBranch()
	if err != nil || branch != "main" {
		t.Fatalf("branch = %q, err = %v", branch, err)
	}

	clean, err := r.IsClean()
	if err != nil || !clean {
		t.Fatalf("fresh repository should be clean: %v %v", clean, err)
	}

	if err := os.WriteFile(filepath.Join(r.Dir, "f.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if clean, _ := r.IsClean(); clean {
		t.Error("a modified file makes the tree dirty")
	}
}

// Ahead and behind are counted against the given ref, not swapped.
func TestAheadBehind(t *testing.T) {
	r := repoWithCommit(t)
	run(t, r.Dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	ahead, behind, err := r.AheadBehind("origin/main")
	if err != nil || ahead != 0 || behind != 0 {
		t.Fatalf("in step: %d/%d %v", ahead, behind, err)
	}

	if err := os.WriteFile(filepath.Join(r.Dir, "f.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, r.Dir, "commit", "--quiet", "-am", "second")

	ahead, behind, err = r.AheadBehind("origin/main")
	if err != nil || ahead != 1 || behind != 0 {
		t.Fatalf("one local commit: got %d ahead %d behind, err %v", ahead, behind, err)
	}
}

func TestBranchExistsAndRemoteBranches(t *testing.T) {
	r := repoWithCommit(t)
	run(t, r.Dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	run(t, r.Dir, "update-ref", "refs/remotes/origin/release-1.0", "HEAD")
	run(t, r.Dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	if !r.LocalBranchExists("main") || r.LocalBranchExists("nope") {
		t.Error("local branch detection is wrong")
	}

	if !r.RemoteBranchExists("release-1.0") || r.RemoteBranchExists("nope") {
		t.Error("remote branch detection is wrong")
	}

	branches, err := r.RemoteBranches()
	if err != nil {
		t.Fatal(err)
	}

	// origin/HEAD is a symbolic alias, not a branch to release from.
	for _, b := range branches {
		if b == "HEAD" || strings.Contains(b, "->") {
			t.Errorf("RemoteBranches leaked %q", b)
		}
	}

	if len(branches) != 2 {
		t.Errorf("got %v, want main and release-1.0", branches)
	}
}

func TestTags(t *testing.T) {
	r := repoWithCommit(t)
	run(t, r.Dir, "tag", "1.0.0")
	run(t, r.Dir, "tag", "1.0.1-rc.0")

	tags, err := r.Tags()
	if err != nil {
		t.Fatal(err)
	}

	if len(tags) != 2 {
		t.Errorf("got %v, want both tags", tags)
	}
}

// Submodule helpers must not fail on a repository that has no .gitmodules.
func TestSubmoduleHelpersWithoutGitmodules(t *testing.T) {
	r := repoWithCommit(t)

	paths, err := r.SubmodulePaths()
	if err != nil {
		t.Fatalf("a repository without submodules is not an error: %v", err)
	}

	if len(paths) != 0 {
		t.Errorf("got %v, want none", paths)
	}

	if _, err := r.SubmoduleNameByPath("sub"); err == nil {
		t.Error("looking up a missing submodule must fail")
	}
}

func TestSubmoduleAttributes(t *testing.T) {
	r := repoWithCommit(t)

	gitmodules := "" +
		"[submodule \"lib\"]\n\tpath = vendor/lib\n\turl = git@example.com:lib.git\n\tbranch = develop\n" +
		"[submodule \"tools\"]\n\tpath = .tools\n\turl = git@example.com:tools.git\n"

	if err := os.WriteFile(filepath.Join(r.Dir, ".gitmodules"), []byte(gitmodules), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := r.SubmodulePaths()
	if err != nil {
		t.Fatal(err)
	}

	if len(paths) != 2 || paths[0] != ".tools" || paths[1] != "vendor/lib" {
		t.Errorf("got %v, want both paths sorted", paths)
	}

	name, err := r.SubmoduleNameByPath("vendor/lib")
	if err != nil || name != "lib" {
		t.Errorf("name = %q, err = %v", name, err)
	}

	branch, err := r.SubmoduleBranch("vendor/lib")
	if err != nil || branch != "develop" {
		t.Errorf("branch = %q, err = %v", branch, err)
	}

	// A submodule may simply not track a branch.
	branch, err = r.SubmoduleBranch(".tools")
	if err != nil || branch != "" {
		t.Errorf("missing branch should be empty, got %q, err %v", branch, err)
	}

	url, err := r.SubmoduleURL(".tools")
	if err != nil || url != "git@example.com:tools.git" {
		t.Errorf("url = %q, err = %v", url, err)
	}
}

func TestGitReportsFailureWithOutput(t *testing.T) {
	r := repoWithCommit(t)

	_, err := r.Git("rev-parse", "--verify", "refs/heads/nope")
	if err == nil {
		t.Fatal("expected an error for a missing ref")
	}

	if !strings.Contains(err.Error(), "git rev-parse") {
		t.Errorf("error should name the command: %v", err)
	}
}

func TestShEnv(t *testing.T) {
	r := repoWithCommit(t)

	if err := r.ShEnv("test \"$RT_TEST\" = yes", []string{"RT_TEST=yes"}); err != nil {
		t.Errorf("the extra environment did not reach the command: %v", err)
	}

	if err := r.Sh("exit 3"); err == nil {
		t.Error("a failing command must be reported")
	}
}

func TestClone(t *testing.T) {
	src := repoWithCommit(t)
	dst := filepath.Join(t.TempDir(), "nested", "clone")

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	if err := Clone(src.Dir, dst, "main"); err != nil {
		t.Fatalf("clone into a missing parent directory should work: %v", err)
	}

	clone := Repo{Dir: dst}
	if !clone.IsRepo() {
		t.Fatal("clone is not a repository")
	}

	if branch, _ := clone.CurrentBranch(); branch != "main" {
		t.Errorf("branch = %q, want main", branch)
	}
}

// An uninitialised submodule is an empty directory inside its parent, and
// rev-parse answers about the parent unless the question is asked precisely.
func TestIsRepoRootVsIsRepo(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "--quiet")

	root := Repo{Dir: dir}
	if !root.IsRepoRoot() || !root.IsRepo() {
		t.Fatal("the repository itself is both")
	}

	inside := filepath.Join(dir, "empty-submodule")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}

	sub := Repo{Dir: inside}
	if !sub.IsRepo() {
		t.Error("IsRepo walks up, which is the trap this guards")
	}

	if sub.IsRepoRoot() {
		t.Error("an empty directory is not a working tree of its own")
	}

	// A real repository there is a root again.
	run(t, inside, "init", "--quiet")

	if !sub.IsRepoRoot() {
		t.Error("a nested repository is its own root")
	}
}
