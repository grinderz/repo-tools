package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/run"
)

// flowConfig writes a config with one flow and returns its path.
func flowConfig(t *testing.T, flows string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := flows +
		"projects:\n" +
		"  - name: api\n" +
		"    git: git@example.com:group/api.git\n" +
		"    project_dir: " + dir + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func runFlow(path string, args ...string) error {
	root := NewRoot()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"-c", path}, args...))

	if err := root.Execute(); err != nil {
		return fmt.Errorf("rt: %w", err)
	}

	return nil
}

// A flow is the config's release routine: the sequence is printed first, then
// every command runs exactly as it would on its own.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestFlowRunsItsCommandsInOrder(t *testing.T) {
	path := flowConfig(t, "flows:\n  rel:\n    - \"repo sync\"\n    - \"repo status\"\n")

	var err error

	out := captureOutput(t, func() { err = runFlow(path, "flow", "rel", "api", "--dry-run") })
	if err != nil {
		t.Fatal(err)
	}

	sync := strings.Index(out, "repo sync (1/2)")
	status := strings.Index(out, "repo status (2/2)")

	if !strings.Contains(out, "flow rel plan (api)") || sync < 0 || status < 0 || status < sync {
		t.Errorf("output:\n%s", out)
	}
}

// A step may carry flags, the way the shell would: they reach the command,
// and an unknown one is a config error found at startup.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestFlowStepsTakeFlags(t *testing.T) {
	f := newFixture(t) // the parent sits on release-1.0, so --checkout has a switch to plan

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "flows:\n  back:\n    - \"repo sync --checkout\"\n" +
		"projects:\n" +
		"  - name: parent\n" +
		"    git: " + f.sub + "\n" +
		"    project_dir: " + f.parent + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var err error

	out := captureOutput(t, func() { err = runFlow(path, "flow", "back", "--dry-run", "--no-fetch") })
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "1. repo sync --checkout") ||
		!strings.Contains(out, "checkout develop (from release-1.0)") {
		t.Errorf("the flag did not reach the command:\n%s", out)
	}

	bogus := flowConfig(t, "flows:\n  bad:\n    - \"repo sync --bogus\"\n")

	err = runFlow(bogus, "repo", "status", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --bogus") {
		t.Errorf("an unknown flag in a flow must fail at startup, got %v", err)
	}
}

// A root flag in a step — --mr here — applies to that step alone: the step
// after it runs the way the command line had things.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestFlowStepFlagsDoNotLeakIntoTheNextStep(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "flows:\n  bump:\n    - \"deps submodules --mr\"\n    - \"deps submodules\"\n" +
		"projects:\n" +
		"  - name: parent\n" +
		"    git: " + f.sub + "\n" +
		"    project_dir: " + f.parent + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var err error

	out := captureOutput(t, func() { err = runFlow(path, "flow", "bump", "--dry-run", "--no-fetch") })
	if err != nil {
		t.Fatal(err)
	}

	first := strings.Index(out, "deps submodules --mr (1/2)")
	second := strings.Index(out, "deps submodules (2/2)")

	if first < 0 || second < first {
		t.Fatalf("steps out of order:\n%s", out)
	}

	if !strings.Contains(out[first:second], "open MR into develop") {
		t.Errorf("the first step should plan a merge request:\n%s", out[first:second])
	}

	if !strings.Contains(out[second:], "commit+push to origin/develop") || strings.Contains(out[second:], "open MR") {
		t.Errorf("the second step should push directly:\n%s", out[second:])
	}

	// The run's own flags are not a step's to set.
	bad := flowConfig(t, "flows:\n  bad:\n    - \"repo status --skip api\"\n")

	err = runFlow(bad, "repo", "status", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "--skip is the run's, not a step's") {
		t.Errorf("--skip in a step must be refused at startup, got %v", err)
	}
}

// Every step is resolved before any runs: a broken flow must fail while
// nothing has happened yet, not in the middle of a release.
func TestFlowRefusesWhatItCannotRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		flow string
		want string
	}{
		{"typo", "    - \"repo bogus\"\n", "is not a command"},
		{"group", "    - \"repo\"\n", "is a group"},
		{"nested flow", "    - \"flow rel\"\n", "cannot run another flow"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := flowConfig(t, "flows:\n  rel:\n    - \"repo sync\"\n"+tc.flow)

			err := runFlow(path, "flow", "rel", "--dry-run")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v", err)
			}
		})
	}
}

// A broken flow is a config error for every command, not a surprise on
// release day when the flow first runs.
func TestFlowsAreValidatedAtStartup(t *testing.T) {
	t.Parallel()

	path := flowConfig(t, "flows:\n  rel:\n    - \"repo bogus\"\n")

	err := runFlow(path, "repo", "status", "--no-fetch")
	if err == nil || !strings.Contains(err.Error(), "is not a command") {
		t.Errorf("got %v", err)
	}
}

// The commands a flow cannot express or should never batch are refused at
// startup with the reason, like every other flow config error.
func TestFlowRefusesBannedSteps(t *testing.T) {
	t.Parallel()

	for _, step := range []string{"git cherry-pick", "git rebase", "repo exec", "changelog gen"} {
		path := flowConfig(t, "flows:\n  rel:\n    - \""+step+"\"\n")

		err := runFlow(path, "repo", "status", "--no-fetch")
		if err == nil || !strings.Contains(err.Error(), "cannot be a flow step") {
			t.Errorf("%s: got %v", step, err)
		}
	}
}

// The disable list holds inside a flow too, or it would be a fence with a
// gate: a dev config that forbids deps freeze forbids it spelled either way.
func TestFlowHonorsDisabledCommands(t *testing.T) {
	t.Parallel()

	path := flowConfig(t, "disable:\n  - \"deps freeze\"\nflows:\n  rel:\n    - \"deps freeze\"\n")

	err := runFlow(path, "flow", "rel", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "deps freeze is disabled") {
		t.Errorf("got %v", err)
	}
}

// A flow name is answered with the list to pick from, and a config without
// flows says that instead of hinting at a list that is empty.
func TestFlowNameMustExist(t *testing.T) {
	t.Parallel()

	path := flowConfig(t, "flows:\n  rel:\n    - \"repo sync\"\n")

	err := runFlow(path, "flow", "rell", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "defined: rel") {
		t.Errorf("got %v", err)
	}

	bare := flowConfig(t, "")
	if err := runFlow(bare, "flow", "rel", "--dry-run"); err == nil ||
		!strings.Contains(err.Error(), "no flows at all") {
		t.Errorf("got %v", err)
	}
}

// The done hook fires once per invocation with the outcome, and a dry run
// fires nothing: a notification about a plan nobody executed is noise.
//
//nolint:paralleltest // installs the process-wide hooks and swaps os.Args
func TestMainFiresTheDoneHook(t *testing.T) {
	f := newFixture(t)
	log := filepath.Join(t.TempDir(), "hooks.log")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "hooks:\n  done:\n    - \"echo \\\"$RT_COMMAND|$RT_STATUS|$RT_MESSAGE\\\" >> " + log + "\"\n" +
		"confirm: never\n" +
		"projects:\n" +
		"  - name: parent\n" +
		"    git: " + f.sub + "\n" +
		"    project_dir: " + f.parent + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	args := os.Args

	defer func() { os.Args = args }()

	defer run.SetHooks(nil, "", false)

	os.Args = []string{"rt", "-c", path, "repo", "status", "parent", "--no-fetch"}

	if code := captureExit(t, Main); code != 0 {
		t.Fatalf("repo status exited %d", code)
	}

	os.Args = []string{"rt", "-c", path, "repo", "status", "nope"}

	if code := captureExit(t, Main); code != 1 {
		t.Fatalf("an unknown project should exit 1, got %d", code)
	}

	os.Args = []string{"rt", "-c", path, "repo", "sync", "--dry-run", "--no-fetch"}

	if code := captureExit(t, Main); code != 0 {
		t.Fatalf("dry run exited %d", code)
	}

	got, _ := os.ReadFile(log)
	want := "repo status parent|ok|\nrepo status nope|failed|project \"nope\" is not in the config\n"

	if string(got) != want {
		t.Errorf("hook log:\n%s\nwant:\n%s", got, want)
	}
}

// captureExit runs main with its output swallowed and returns the exit code.
func captureExit(t *testing.T, main func() int) int {
	t.Helper()

	code := 0

	captureOutput(t, func() {
		stderr := os.Stderr
		os.Stderr, _ = os.Open(os.DevNull)

		defer func() { os.Stderr = stderr }()

		code = main()
	})

	return code
}
