package changelog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/gitx"
)

// commitAt adds a commit with a fixed commit date.
func commitAt(t *testing.T, dir, date, subject, author string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(subject), 0o644); err != nil {
		t.Fatal(err)
	}

	stamp := date + "T12:00:00+00:00"
	cmd := exec.CommandContext(t.Context(), "git", "commit", "--quiet", "-a", "-m", subject)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+author,
		"GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME="+author,
		"GIT_COMMITTER_EMAIL=a@example.com",
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_DATE="+stamp,
		"TZ=UTC",
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit %q: %v\n%s", subject, err, out)
	}
}

func fixtureRepo(t *testing.T) gitx.Repo {
	t.Helper()

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.email", "rt@example.com"},
		{"config", "user.name", "repo-tools test"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "git", "add", "-A")
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	commitAt(t, dir, "2026-01-05", "feat/AB-1: first", "Ann")
	commitAt(t, dir, "2026-01-05", "fix/AB-2: second", "Bob")
	commitAt(t, dir, "2026-02-10", "feat/AB-3: later", "Ann")

	return gitx.Repo{Dir: dir}
}

// The layout must match the changelog-gen.sh script this replaces: header,
// then a blank line, "## [date]", a blank line and the day's commits, newest
// day first and oldest commit first inside a day.
func TestGitLogLayout(t *testing.T) {
	t.Setenv("TZ", "UTC")

	got, err := GitLog(fixtureRepo(t), Options{Header: "# CHANGELOG of demo"})
	if err != nil {
		t.Fatal(err)
	}

	want := "# CHANGELOG of demo\n" +
		"\n## [2026-02-10]\n\n" +
		" * feat/AB-3: later (Ann)\n" +
		"\n## [2026-01-05]\n\n" +
		" * feat/AB-1: first (Ann)\n" +
		" * fix/AB-2: second (Bob)\n"

	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestGitLogWindow(t *testing.T) {
	t.Setenv("TZ", "UTC")

	got, err := GitLog(fixtureRepo(t), Options{
		Header: "# CHANGELOG",
		Since:  "2026-02-01",
		Until:  "2026-03-01",
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got, "2026-01-05") {
		t.Errorf("commits outside the window leaked in:\n%s", got)
	}

	if !strings.Contains(got, " * feat/AB-3: later (Ann)") {
		t.Errorf("commit inside the window is missing:\n%s", got)
	}
}

func TestGitLogDefaultHeader(t *testing.T) {
	got, err := GitLog(fixtureRepo(t), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(got, DefaultHeader+"\n") {
		t.Errorf("missing default header, got %q", got[:min(len(got), 40)])
	}
}

func TestGitLogEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init", "--quiet")
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	got, err := GitLog(gitx.Repo{Dir: dir}, Options{Header: "# CHANGELOG"})
	if err != nil {
		t.Fatal(err)
	}

	if got != "# CHANGELOG\n" {
		t.Errorf("got %q, want just the header", got)
	}
}
