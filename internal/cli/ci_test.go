package cli

import (
	"testing"
)

// The GitLab answer is a list with the newest pipeline first; every status
// falls into one of the states the watch acts on.
func TestParseGitLabPipelines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, status string
		want         ciState
	}{
		{"running", "running", ciRunning},
		{"queued", "pending", ciRunning},
		{"green", "success", ciSuccess},
		{"red", "failed", ciFailed},
		{"canceled counts as red", "canceled", ciFailed},
		{"skipped", "skipped", ciSkipped},
		{"manual gate", "manual", ciManual},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := `[{"id": 42, "status": "` + tc.status + `", "web_url": "https://gl/p/42"}]`

			pipe, err := parseGitLabPipelines(out)
			if err != nil {
				t.Fatal(err)
			}

			if pipe.State != tc.want || pipe.Label != "pipeline #42" || pipe.URL != "https://gl/p/42" {
				t.Errorf("pipe = %+v", pipe)
			}
		})
	}
}

// A retry addresses what failed: the GitLab pipeline itself, or exactly the
// GitHub run that went red among green ones.
func TestParsersNameWhatARetryAddresses(t *testing.T) {
	t.Parallel()

	gl, err := parseGitLabPipelines(`[{"id": 42, "status": "failed", "web_url": "u"}]`)
	if err != nil || gl.RetryID != "42" {
		t.Errorf("gitlab retry id = %q, %v", gl.RetryID, err)
	}

	out := `[{"databaseId": 7, "status": "completed", "conclusion": "success", "url": "u7", "workflowName": "a"},` +
		`{"databaseId": 9, "status": "completed", "conclusion": "failure", "url": "u9", "workflowName": "b"}]`

	gh, err := parseGitHubRuns(out)
	if err != nil || gh.RetryID != "9" || gh.URL != "u9" {
		t.Errorf("github retry id = %q url = %q, %v", gh.RetryID, gh.URL, err)
	}
}

// The wait has a shape: jobs that will not move again count as done, a
// manual gate does not — the pipeline itself turns manual and says so.
func TestParseGitLabJobs(t *testing.T) {
	t.Parallel()

	out := `[{"status": "success"}, {"status": "failed"}, {"status": "skipped"},` +
		`{"status": "running"}, {"status": "created"}, {"status": "manual"}]`

	progress, err := parseGitLabJobs(out)
	if err != nil || progress != "3/6 jobs" {
		t.Errorf("progress = %q, %v", progress, err)
	}

	if progress, _ := parseGitLabJobs("[]"); progress != "" {
		t.Errorf("no jobs, no shape: %q", progress)
	}

	if _, err := parseGitLabJobs("glab: 404"); err == nil {
		t.Error("garbage must be an error")
	}
}

// A commit still building reports how many of its runs have finished.
func TestParseGitHubRunsProgress(t *testing.T) {
	t.Parallel()

	out := `[{"databaseId": 1, "status": "completed", "conclusion": "success", "url": "u", "workflowName": "a"},` +
		`{"databaseId": 2, "status": "in_progress", "conclusion": "", "url": "u", "workflowName": "b"},` +
		`{"databaseId": 3, "status": "queued", "conclusion": "", "url": "u", "workflowName": "c"}]`

	pipe, err := parseGitHubRuns(out)
	if err != nil || pipe.State != ciRunning || pipe.Progress != "1/3 runs" {
		t.Errorf("pipe = %+v, %v", pipe, err)
	}
}

func TestParseGitLabPipelinesEdges(t *testing.T) {
	t.Parallel()

	if pipe, err := parseGitLabPipelines("[]"); err != nil || pipe.State != ciMissing {
		t.Errorf("no pipeline yet: %+v, %v", pipe, err)
	}

	// The newest pipeline decides; an older rerun of the same sha does not.
	out := `[{"id": 2, "status": "running", "web_url": "u2"},` +
		`{"id": 1, "status": "failed", "web_url": "u1"}]`
	if pipe, _ := parseGitLabPipelines(out); pipe.State != ciRunning || pipe.Label != "pipeline #2" {
		t.Errorf("the newest pipeline must win: %+v", pipe)
	}

	if _, err := parseGitLabPipelines("glab: not logged in"); err == nil {
		t.Error("garbage must be an error, not a silent success")
	}
}

// GitHub has no single pipeline: a commit is green when every workflow run of
// it is, red as soon as one failed, and still running while any run is.
func TestParseGitHubRuns(t *testing.T) {
	t.Parallel()

	completed := func(conclusion string) string {
		return `{"status": "completed", "conclusion": "` + conclusion +
			`", "url": "https://gh/r", "workflowName": "ci"}`
	}

	cases := []struct {
		name, out string
		want      ciState
	}{
		{"all green", "[" + completed("success") + "," + completed("success") + "]", ciSuccess},
		{"one red", "[" + completed("success") + "," + completed("failure") + "]", ciFailed},
		{"cancelled counts as red", "[" + completed("cancelled") + "]", ciFailed},
		{"needs approval", "[" + completed("action_required") + "]", ciManual},
		{"still running", `[` + completed("success") + `,{"status": "in_progress", "conclusion": "",` +
			`"url": "https://gh/r", "workflowName": "ci"}]`, ciRunning},
		{"nothing yet", "[]", ciMissing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pipe, err := parseGitHubRuns(tc.out)
			if err != nil {
				t.Fatal(err)
			}

			if pipe.State != tc.want {
				t.Errorf("state = %v, want %v", pipe.State, tc.want)
			}
		})
	}
}

// The changelog commit carries [skip ci] on purpose; waiting two minutes for
// its pipeline would punish exactly the configuration the tool itself writes.
func TestSkipCICommit(t *testing.T) {
	t.Parallel()

	if !skipCICommit("chore(changelog): update changelog [skip ci]") {
		t.Error("[skip ci] must be recognized")
	}

	if !skipCICommit("build: something [CI SKIP]") {
		t.Error("[ci skip] must be recognized case-insensitively")
	}

	if skipCICommit("feat: skip ci setup for now") {
		t.Error("the words without the brackets are just words")
	}
}
