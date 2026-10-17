package cli

import (
	"errors"
	"strings"
	"testing"
)

// Each kind of Renovate log line reads the way rt says things: the merge
// request with its number, the branch, the extraction summary, warnings and
// errors labelled, the rest dim; a line that is not JSON passes through.
func TestRenderRenovateLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, platform, line, want string
	}{
		{
			"pr created", "gitlab",
			`{"level":30,"msg":"PR created","repository":"group/api","pr":7,` +
				`"prTitle":"build/AB-0000: update pydantic to v2.11"}`,
			"MR !7 created: build/AB-0000: update pydantic to v2.11",
		},
		{
			"pr updated on github", "github",
			`{"level":30,"msg":"PR updated","repository":"o/r","pr":{"number":12},"prTitle":"t"}`,
			"PR #12 updated: t",
		},
		{
			"branch", "gitlab",
			`{"level":30,"msg":"Branch created","repository":"group/api","branch":"renovate/pydantic-2.x"}`,
			"branch renovate/pydantic-2.x created",
		},
		{
			"extraction", "gitlab",
			`{"level":30,"msg":"Dependency extraction complete","repository":"group/api","baseBranch":"develop",` +
				`"stats":{"total":{"fileCount":3,"depCount":12},"managers":{"gomod":{"fileCount":1,"depCount":10},` +
				`"dockerfile":{"fileCount":2,"depCount":2}}}}`,
			"12 dependencies in 3 files (dockerfile 2, gomod 10) on develop",
		},
		{
			"extraction, one file", "gitlab",
			`{"level":30,"msg":"Dependency extraction complete","repository":"group/api","baseBranch":"develop",` +
				`"stats":{"total":{"fileCount":1,"depCount":1},"managers":{"gomod":{"fileCount":1,"depCount":1}}}}`,
			"1 dependency in 1 file (gomod 1) on develop",
		},
		{
			"dry run", "gitlab",
			`{"level":30,"msg":"DRY-RUN: Would create PR \"update x\"","repository":"group/api"}`,
			`DRY-RUN: Would create PR "update x"`,
		},
		{
			"error with a reason", "gitlab",
			`{"level":50,"msg":"Repository has changed during renovation","repository":"group/api",` +
				`"err":{"message":"conflict"}}`,
			"ERROR: Repository has changed during renovation: conflict",
		},
		{
			"validation warning", "gitlab",
			`{"level":40,"msg":"Config validation errors found","errors":[{"topic":"Configuration Error",` +
				`"message":"Invalid configuration option: bogusKey"}]}`,
			"WARNING: Config validation errors found\n      - Invalid configuration option: bogusKey",
		},
		{
			"warnings as strings, capped", "gitlab",
			`{"level":40,"msg":"Package lookup failures","repository":"group/api","files":["pyproject.toml"],` +
				`"warnings":["Failed to look up pypi package a","Failed to look up pypi package b",` +
				`"Failed to look up pypi package c","Failed to look up pypi package d","Failed to look up pypi package e"]}`,
			"WARNING: Package lookup failures (pyproject.toml)\n      - Failed to look up pypi package a\n" +
				"      - Failed to look up pypi package b\n      - Failed to look up pypi package c\n      ... and 2 more",
		},
		{
			"artifact errors name the lock file and the cause", "gitlab",
			`{"level":40,"msg":"artifactErrors","repository":"group/api","branch":"renovate/dependencies",` +
				`"artifactErrors":[{"lockFile":"uv.lock","stderr":"Using CPython 3.13\nerror: Failed to build ` +
				`pylib\n  cause: not a project\n"}]}`,
			"ERROR: lock file not updated\n      - uv.lock: error: Failed to build pylib",
		},
		{
			"an odd field costs the field, not the line", "gitlab",
			`{"level":40,"msg":"something","repository":"group/api","stats":"not an object"}`,
			"WARNING: something",
		},
		{"started is silent", "gitlab", `{"level":30,"msg":"Repository started","repository":"group/api"}`, ""},
		{
			"other info is dim",
			"gitlab",
			`{"level":30,"msg":"Repository finished?no, something else"}`,
			"Repository finished?no",
		},
	}

	for _, tc := range cases {
		event, ok := parseRenovateLine(tc.line)
		if !ok {
			t.Errorf("%s: not parsed", tc.name)

			continue
		}

		got := newRenovateReport(tc.platform, nil).render(event)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: render = %q, want %q", tc.name, got, tc.want)
		}
	}

	if _, ok := parseRenovateLine("Unable to find image locally"); ok {
		t.Error("a plain line is not an event")
	}
}

// The report opens a header per repository under the project's own name,
// counts what happened, and fails the run for the repositories that logged
// an error — or for an exit status nothing explained.
//
// A repository whose lines come back after another's keeps its tally: it is
// one entry in the summary, with everything it reported.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestRenovateReportKeepsTallyOnReturn(t *testing.T) {
	report := newRenovateReport("gitlab", map[string]string{"group/api": "api", "group/worker": "worker"})

	captureOutput(t, func() {
		for _, line := range []string{
			`{"level":30,"msg":"PR created","repository":"group/api","pr":7,"prTitle":"one"}`,
			`{"level":30,"msg":"Repository started","repository":"group/worker"}`,
			`{"level":50,"msg":"an error","repository":"group/api"}`,
		} {
			report.line(line)
		}
	})

	var err error

	summary := captureOutput(t, func() { err = report.finish(nil) })

	if !strings.Contains(summary, "1 MR created, 0 updated, 0 warning(s), 1 error(s)") {
		t.Errorf("summary = %q", summary)
	}

	if err == nil || !strings.Contains(err.Error(), "for api") || strings.Contains(err.Error(), "api, api") {
		t.Errorf("err = %v, want api named once", err)
	}
}

//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestRenovateReportTalliesAndFails(t *testing.T) {
	report := newRenovateReport("gitlab", map[string]string{"group/api": "api", "group/worker": "worker"})

	out := captureOutput(t, func() {
		for _, line := range []string{
			`{"level":30,"msg":"Repository started","repository":"group/api"}`,
			`{"level":30,"msg":"PR created","repository":"group/api","pr":7,"prTitle":"one"}`,
			`{"level":30,"msg":"PR updated","repository":"group/api","pr":8,"prTitle":"two"}`,
			`{"level":30,"msg":"Repository started","repository":"group/worker"}`,
			`{"level":40,"msg":"a warning","repository":"group/worker"}`,
			`{"level":50,"msg":"an error","repository":"group/worker"}`,
			`{"level":40,"msg":"artifactErrors","repository":"group/api",` +
				`"artifactErrors":[{"lockFile":"go.sum","stderr":"go: 401"}]}`,
			"not json at all",
		} {
			report.line(line)
		}
	})

	for _, want := range []string{
		"==> api", "MR !7 created: one", "==> worker", "WARNING: a warning", "not json at all",
		"ERROR: lock file not updated\n      - go.sum: go: 401",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	var err error

	summary := captureOutput(t, func() { err = report.finish(nil) })

	if !strings.Contains(summary, "1 MR created, 1 updated, 1 warning(s), 2 error(s)") {
		t.Errorf("summary = %q", summary)
	}

	// A lock file Renovate could not update is an error of its repository
	// too, though Renovate only warns: the merge request cannot pass.
	if err == nil || !strings.Contains(err.Error(), "renovate reported errors for") ||
		!strings.Contains(err.Error(), "worker") || !strings.Contains(err.Error(), "api") {
		t.Errorf("err = %v, want worker and api named", err)
	}

	// An error before any repository — an authentication failure — is the
	// run's own, counted and named as such.
	global := newRenovateReport("gitlab", nil)

	summary = captureOutput(t, func() {
		global.line(`{"level":60,"msg":"Initialization error","errorMessage":"Authentication failure"}`)
		err = global.finish(errors.New("exit status 1"))
	})

	if !strings.Contains(summary, "ERROR: Initialization error: Authentication failure") ||
		!strings.Contains(summary, "0 MRs created, 0 updated, 0 warning(s), 1 error(s)") {
		t.Errorf("summary = %q", summary)
	}

	if err == nil || !strings.Contains(err.Error(), "renovate reported errors for the run itself") {
		t.Errorf("err = %v", err)
	}

	clean := newRenovateReport("gitlab", nil)

	captureOutput(t, func() { err = clean.finish(errors.New("exit status 1")) })

	if err == nil || !strings.Contains(err.Error(), "renovate exited with exit status 1") {
		t.Errorf("an unexplained exit status must fail the run: %v", err)
	}
}
