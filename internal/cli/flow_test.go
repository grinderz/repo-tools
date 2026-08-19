package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// Every step is resolved before any runs: a broken flow must fail while
// nothing has happened yet, not in the middle of a release.
func TestFlowRefusesWhatItCannotRun(t *testing.T) {
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
	path := flowConfig(t, "flows:\n  rel:\n    - \"repo bogus\"\n")

	err := runFlow(path, "repo", "status", "--no-fetch")
	if err == nil || !strings.Contains(err.Error(), "is not a command") {
		t.Errorf("got %v", err)
	}
}

// The disable list holds inside a flow too, or it would be a fence with a
// gate: a dev config that forbids deps freeze forbids it spelled either way.
func TestFlowHonorsDisabledCommands(t *testing.T) {
	path := flowConfig(t, "disable:\n  - \"deps freeze\"\nflows:\n  rel:\n    - \"deps freeze\"\n")

	err := runFlow(path, "flow", "rel", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "deps freeze is disabled") {
		t.Errorf("got %v", err)
	}
}

// A flow name is answered with the list to pick from, and a config without
// flows says that instead of hinting at a list that is empty.
func TestFlowNameMustExist(t *testing.T) {
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
