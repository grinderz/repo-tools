package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// A push is not the end of a release step: the pipeline it triggers is. A
// project with ci: gitlab or ci: github gets that pipeline watched after
// every push of a commit or a tag, through the system's own CLI — glab or gh,
// which also carry the authentication, so rt holds no tokens. The watch is
// opt-in per project and --no-ci skips it for one run.

// ciState is a pipeline's condition reduced to what the watch acts on.
type ciState int

const (
	ciMissing ciState = iota // no pipeline for this revision (yet)
	ciRunning
	ciSuccess
	ciFailed
	ciSkipped
	ciManual // waits for a person; polling would sit there forever
)

// ciPipeline is the one pipeline the watch follows.
type ciPipeline struct {
	State    ciState
	Label    string // "pipeline #123" or a workflow name, for the messages
	URL      string
	RetryID  string // what a retry addresses: the pipeline, or the failed run
	Progress string // "3/7 jobs" while running, so the wait has a shape
}

// ciPlanSuffix says in the plan that a push does not end the step.
func ciPlanSuffix(rctx *run.Ctx, p *config.Project) string {
	if rctx.NoCI || p.CI == config.CINone {
		return ""
	}

	return ", then watch the " + p.CI + " pipeline"
}

// skipCICommit recognizes the commit-message markers both systems honour, so
// the watch does not wait two minutes for a pipeline that was asked not to
// exist — the changelog commit carries [skip ci] on purpose.
func skipCICommit(msg string) bool {
	lower := strings.ToLower(msg)

	return strings.Contains(lower, "[skip ci]") || strings.Contains(lower, "[ci skip]")
}

// ciAppearWait is how long a pipeline gets to appear after a push before the
// watch concludes there is none coming. Creation is quick; this is slack for
// the webhook and the scheduler, not a second timeout.
const ciAppearWait = 2 * time.Minute

// watchCI polls the pipeline of a pushed revision until it settles. sha is
// the commit that was pushed and ref the branch or tag it was pushed as; msg
// is the commit message, empty for a tag. No pipeline appearing is a warning,
// not an error: a [skip ci] commit and a project without CI are both normal.
func watchCI(rctx *run.Ctx, r gitx.Repo, p *config.Project, sha, ref, msg string) error {
	if rctx.NoCI || p.CI == config.CINone {
		return nil
	}

	if skipCICommit(msg) {
		fmt.Println("    " + run.Dim("commit says [skip ci], not watching the pipeline"))

		return nil
	}

	// No blank line of its own: the answered push question already left one,
	// and a second in a row is the only place the output would double up.
	deadline := time.Now().Add(rctx.Cfg.CIWait())
	appearBy := time.Now().Add(ciAppearWait)
	last := ciPipeline{State: ciMissing}
	retried := 0

	for {
		pipe, err := ciQuery(r, p.CI, sha, ref)
		if err != nil {
			return fmt.Errorf("%s pipeline for %s: %w", p.CI, ref, err)
		}

		// A failure is only final once the retries are spent. The retry
		// restarts the failed jobs, so the same pipeline goes back to
		// running and the watch keeps following it — with a fresh deadline,
		// since the jobs start over.
		if pipe.State == ciFailed && retried < rctx.Cfg.CIRetryLimit() {
			retried++
			fmt.Printf("    %s %s failed, retrying (%d/%d): %s\n",
				p.CI, pipe.Label, retried, rctx.Cfg.CIRetryLimit(), run.Dim(pipe.URL))

			if err := ciRetry(r, p.CI, pipe); err != nil {
				return fmt.Errorf("retry %s %s: %w", p.CI, pipe.Label, err)
			}

			last = ciPipeline{State: ciMissing}
			deadline = time.Now().Add(rctx.Cfg.CIWait())

			time.Sleep(rctx.Cfg.CIPoll())

			continue
		}

		if done, err := ciReport(p.CI, pipe, &last); done {
			return err
		}

		now := time.Now()

		// A pipeline that never appeared has no label or URL to report, so
		// the deadline never speaks for it — the appear window does, even
		// when a short ci_wait_minutes technically expires first.
		switch {
		case pipe.State == ciMissing && (now.After(appearBy) || now.After(deadline)):
			fmt.Printf("    %s no %s pipeline appeared for %s in %s\n",
				run.Warn(), p.CI, ref, ciAppearWait)

			return nil
		case pipe.State != ciMissing && now.After(deadline):
			return fmt.Errorf("%s %s %w %s: %s",
				p.CI, pipe.Label, errStillRunning, rctx.Cfg.CIWait(), pipe.URL)
		}

		time.Sleep(rctx.Cfg.CIPoll())
	}
}

// ciReport prints what changed and says whether the watch is over. The
// terminal states speak for themselves; only failure is also an error.
func ciReport(kind string, pipe ciPipeline, last *ciPipeline) (bool, error) {
	if pipe.State != last.State || pipe.Label != last.Label || pipe.Progress != last.Progress {
		*last = pipe

		if pipe.State == ciRunning {
			progress := ""
			if pipe.Progress != "" {
				progress = " (" + pipe.Progress + ")"
			}

			fmt.Printf("    %s %s running%s: %s\n", kind, pipe.Label, progress, run.Dim(pipe.URL))
		}
	}

	switch pipe.State {
	case ciSuccess:
		fmt.Printf("    %s %s %s\n", kind, pipe.Label, run.Green("succeeded"))

		return true, nil
	case ciFailed:
		return true, fmt.Errorf("%s %s %w: %s", kind, pipe.Label, errPipelineFailed, pipe.URL)
	case ciSkipped:
		fmt.Printf("    %s %s was skipped\n", kind, pipe.Label)

		return true, nil
	case ciManual:
		fmt.Printf("    %s %s waits for a manual action: %s\n", kind, pipe.Label, pipe.URL)

		return true, nil
	case ciMissing, ciRunning:
	}

	return false, nil
}

// ciQuery asks the system's CLI about the pipeline of one revision.
func ciQuery(r gitx.Repo, kind, sha, ref string) (ciPipeline, error) {
	switch kind {
	case config.CIGitLab:
		out, err := r.Output("glab", "api",
			fmt.Sprintf("projects/:id/pipelines?sha=%s&ref=%s", sha, url.QueryEscape(ref)))
		if err != nil {
			return ciPipeline{}, err
		}

		pipe, err := parseGitLabPipelines(out)
		if err != nil || pipe.State != ciRunning {
			return pipe, err
		}

		// The job list is what gives the wait a shape; losing it is no
		// reason to stop watching the pipeline itself.
		if jobs, jobsErr := r.Output("glab", "api",
			"projects/:id/pipelines/"+pipe.RetryID+"/jobs?per_page=100"); jobsErr == nil {
			pipe.Progress, _ = parseGitLabJobs(jobs)
		}

		return pipe, nil
	case config.CIGitHub:
		out, err := r.Output("gh", "run", "list",
			"--commit", sha, "--json", "databaseId,status,conclusion,url,workflowName")
		if err != nil {
			return ciPipeline{}, err
		}

		return parseGitHubRuns(out)
	}

	return ciPipeline{}, fmt.Errorf("%w %q", errUnknownCI, kind)
}

// ciRetry restarts the failed jobs of what just failed, the same thing the
// retry button does: the GitLab pipeline, or the GitHub run that went red.
func ciRetry(r gitx.Repo, kind string, pipe ciPipeline) error {
	switch kind {
	case config.CIGitLab:
		_, err := r.Output("glab", "api", "-X", "POST", "projects/:id/pipelines/"+pipe.RetryID+"/retry")

		return err
	case config.CIGitHub:
		_, err := r.Output("gh", "run", "rerun", pipe.RetryID, "--failed")

		return err
	}

	return fmt.Errorf("%w %q", errUnknownCI, kind)
}

// parseGitLabPipelines reads what GET /pipelines?sha=&ref= returned. The
// first entry is the newest, which is the one a fresh push is about.
func parseGitLabPipelines(out string) (ciPipeline, error) {
	var pipelines []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		WebURL string `json:"web_url"` //nolint:tagliatelle // GitLab's own field name
	}

	if err := json.Unmarshal([]byte(out), &pipelines); err != nil {
		return ciPipeline{}, fmt.Errorf("unexpected glab output: %w\n%s", err, out)
	}

	if len(pipelines) == 0 {
		return ciPipeline{State: ciMissing}, nil
	}

	newest := pipelines[0]
	pipe := ciPipeline{
		Label:   fmt.Sprintf("pipeline #%d", newest.ID),
		URL:     newest.WebURL,
		RetryID: strconv.FormatInt(newest.ID, 10),
	}

	pipe.State = gitlabState(newest.Status)

	return pipe, nil
}

// parseGitLabJobs folds the pipeline's job list into "3/7 jobs". Done is any
// state a job will not leave on its own; a manual gate is not done, but the
// pipeline status turns manual/blocked and the watch reports that instead.
func parseGitLabJobs(out string) (string, error) {
	var jobs []struct {
		Status string `json:"status"`
	}

	if err := json.Unmarshal([]byte(out), &jobs); err != nil {
		return "", fmt.Errorf("unexpected glab jobs output: %w\n%s", err, out)
	}

	if len(jobs) == 0 {
		return "", nil
	}

	done := 0

	for _, job := range jobs {
		switch job.Status {
		case "success", "failed", "canceled", "skipped":
			done++
		}
	}

	return fmt.Sprintf("%d/%d jobs", done, len(jobs)), nil
}

// gitlabState maps a GitLab pipeline status onto the watch's states.
func gitlabState(status string) ciState {
	switch status {
	case "success":
		return ciSuccess
	case "failed", "canceled":
		return ciFailed
	case "skipped":
		return ciSkipped
	case "manual", "blocked":
		return ciManual
	default: // created, waiting_for_resource, preparing, pending, running, scheduled
		return ciRunning
	}
}

// parseGitHubRuns folds every workflow run of a commit into one state: GitHub
// has no single pipeline, so the push is green when all its runs are.
func parseGitHubRuns(out string) (ciPipeline, error) {
	var runs []struct {
		ID           int64  `json:"databaseId"` //nolint:tagliatelle // gh's own field name
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		URL          string `json:"url"`
		WorkflowName string `json:"workflowName"`
	}

	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return ciPipeline{}, fmt.Errorf("unexpected gh output: %w\n%s", err, out)
	}

	if len(runs) == 0 {
		return ciPipeline{State: ciMissing}, nil
	}

	pipe := ciPipeline{
		Label: fmt.Sprintf("%s (%d run(s))", runs[0].WorkflowName, len(runs)),
		URL:   runs[0].URL,
	}

	done := 0

	for _, one := range runs {
		if one.Status == "completed" {
			done++
		}
	}

	if done < len(runs) {
		pipe.State = ciRunning
		pipe.Progress = fmt.Sprintf("%d/%d runs", done, len(runs))

		return pipe, nil
	}

	for _, one := range runs {
		switch one.Conclusion {
		case "failure", "cancelled", "timed_out", "startup_failure":
			pipe.State = ciFailed
			pipe.URL = one.URL
			pipe.RetryID = strconv.FormatInt(one.ID, 10)

			return pipe, nil
		case "action_required":
			pipe.State = ciManual
			pipe.URL = one.URL

			return pipe, nil
		}
	}

	pipe.State = ciSuccess

	return pipe, nil
}
