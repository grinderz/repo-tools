package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// A protected dev branch takes no direct pushes: the commit has to arrive
// as a merge request. In that mode the committing commands — deps
// submodules, deps freeze, changelog update — put the commit on a branch of
// their own, push it, open the request into the target branch through glab
// or gh, and watch the pipeline the request starts. The working copy is back
// on the target branch when they are done; the local branch is what repo
// prune deletes once the request is merged.

// The committing commands, as their names appear in merge request branches
// through {command}. They are spelled here rather than read from cobra so a
// branch name does not change when a command is mounted under another name.
const (
	// mrCreate is the subcommand both CLIs spell the same way.
	mrCreate = "create"

	cmdDepsSubmodules  = "deps submodules"
	cmdDepsFreeze      = "deps freeze"
	cmdChangelogUpdate = "changelog update"
)

// mergeRequest is one command's request as it will be opened: the branch the
// commit goes to, where it merges into, and the title.
type mergeRequest struct {
	Branch      string // the branch the commit lands on, from the branch template
	Target      string // the branch the request merges into
	Title       string
	Description string   // empty: the body is left to the system
	BodyFrom    string   // the template file the body came from, for the review
	BodyErr     error    // a configured template the branch does not have
	Assignees   []string // usernames
	Reviewers   []string
}

// mrMode says whether commits go through merge requests for this project.
// mr: none is final — a project whose dev branch takes pushes, or whose CI
// runs no request pipelines, stays out even under --mr; otherwise the flag
// decides, then mr: always.
func mrMode(rctx *run.Ctx, p *config.Project) bool {
	if p.MR == config.MRNone {
		return false
	}

	if rctx.MRFlag != nil {
		return *rctx.MRFlag
	}

	return p.MR == config.MRAlways
}

// planMergeRequest renders the request a command would open for a commit on
// target, nil when the mode is off. command names the command in the branch
// template, so two commands run on the same day get branches of their own.
func planMergeRequest(rctx *run.Ctx, p *config.Project, command, target, msg string) *mergeRequest {
	if !mrMode(rctx, p) {
		return nil
	}

	settings := rctx.Cfg.MergeRequest

	vars := messageVars(rctx, p, target)
	vars[varCommand] = strings.ReplaceAll(command, " ", "-")
	vars[varDate] = time.Now().Format(time.DateOnly)
	vars[varMessage] = firstLine(msg)

	m := &mergeRequest{
		Branch:      expand(settings.Branch, vars),
		Target:      target,
		Title:       expand(settings.Title, vars),
		Description: expand(settings.Description, vars),
		Assignees:   rctx.Cfg.MRAssigneesFor(p),
		Reviewers:   rctx.Cfg.MRReviewersFor(p),
	}

	// The web form fills the body from the repository's template; the API
	// does not, so the template named in the config is read and expanded
	// here. A template the branch lacks is an error at the request, and repo
	// check reports it before that.
	if m.Description == "" && settings.Template != "" {
		body, err := templateBody(p, repoOf(p), target, settings.Template)
		if err != nil {
			m.BodyErr = err
		} else {
			m.Description = expand(body, vars)
			m.BodyFrom = templatePath(p.CI, settings.Template)
		}
	}

	return m
}

// templatePath is where the system keeps a named request template.
func templatePath(ci, name string) string {
	if ci == config.CIGitHub {
		return ".github/PULL_REQUEST_TEMPLATE/" + name + ".md"
	}

	return ".gitlab/merge_request_templates/" + name + ".md"
}

// templateBody reads the named template as the target branch has it.
func templateBody(p *config.Project, r gitx.Repo, target, name string) (string, error) {
	path := templatePath(p.CI, name)

	body, found, err := r.FileAt(branchRef(r, target), path)
	if err != nil {
		return "", err
	}

	if !found {
		return "", fmt.Errorf("%w: %s is not on %s", errNoMRTemplate, path, target)
	}

	return body, nil
}

// mrPlan is the plan's wording of the request.
func (m *mergeRequest) plan() string {
	return fmt.Sprintf("commit to %s, push, open MR into %s", planRef(m.Branch), planRef(m.Target))
}

// question is what the review asks: one answer covers the commit, the push
// and the request, since a pushed branch without a request would sit there
// with no pipeline and nobody to merge it.
func (m *mergeRequest) question() string {
	return fmt.Sprintf("Commit, push and open MR into %s?", m.Target)
}

// review is what the diff review shows about the request, next to the commit
// message: the title and the branches are as much a decision as the diff.
func (m *mergeRequest) review() string {
	text := fmt.Sprintf("merge request: %s\n               %s -> %s",
		run.Green(m.Title), run.Cyan(m.Branch), run.Cyan(m.Target))

	if m.BodyFrom != "" {
		text += "\n               body from " + m.BodyFrom
	}

	if len(m.Assignees) > 0 {
		text += "\n               assigned to " + strings.Join(m.Assignees, ", ")
	}

	if len(m.Reviewers) > 0 {
		text += "\n               reviewed by " + strings.Join(m.Reviewers, ", ")
	}

	return text
}

// args are the flags that describe the request to the CLI: the body only
// when the config gives one — the system's own template applies otherwise —
// and the people. bodyFlag is the one spelling the two CLIs disagree on.
func (m *mergeRequest) args(bodyFlag string) []string {
	var args []string

	if m.Description != "" {
		args = append(args, bodyFlag, m.Description)
	}

	if len(m.Assignees) > 0 {
		args = append(args, "--assignee", strings.Join(m.Assignees, ","))
	}

	if len(m.Reviewers) > 0 {
		args = append(args, "--reviewer", strings.Join(m.Reviewers, ","))
	}

	return args
}

// mrCheck reports a merge request mode that cannot open one: the request
// goes through the system's CLI, so the project has to name it in ci.
func mrCheck(rctx *run.Ctx, p *config.Project) []string {
	if !mrMode(rctx, p) {
		return nil
	}

	if p.CI == config.CINone {
		return []string{"mr is on but ci is none, so no CLI can open a merge request"}
	}

	settings := rctx.Cfg.MergeRequest
	if settings.Description != "" || settings.Template == "" {
		return nil
	}

	if _, err := templateBody(p, repoOf(p), p.TargetBranch(), settings.Template); err != nil {
		return []string{"merge_request: " + err.Error()}
	}

	return nil
}

// commitToMergeRequest commits the staged changes on the request's branch,
// pushes it, opens the request and leaves the working copy on the target
// branch again. It returns the pushed commit for the pipeline watch and the
// request's URL for the wait.
func commitToMergeRequest(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	m *mergeRequest,
	msg string,
) (string, string, error) {
	if p.CI == config.CINone {
		return "", "", fmt.Errorf("%w: ci is none, no CLI to open one with", errNoMRTool)
	}

	// Found before the branch is made: nothing to undo then.
	if m.BodyErr != nil {
		return "", "", m.BodyErr
	}

	// -B: a rerun on the same day rewrites the branch from today's target
	// head, so the request always carries one fresh commit, not a chain of
	// stale bumps on top of each other.
	if _, err := r.Git("checkout", "--quiet", "-B", m.Branch); err != nil {
		return "", "", err
	}

	if _, err := r.Git("commit", "-m", msg); err != nil {
		return "", "", err
	}

	sha, err := r.Git("rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}

	// force-with-lease: the branch is this command's own, and the plan-time
	// fetch made the remote-tracking ref current, so the only rewrite it
	// permits is of what the previous run pushed.
	if err := remoteGit(rctx, r, "push", "--no-follow-tags", "--force-with-lease", "-u", "origin", m.Branch); err != nil {
		return "", "", err
	}

	url, err := openMergeRequest(r, p, m)
	if err != nil {
		return "", "", err
	}

	fmt.Printf("    merge request: %s\n", url)

	// Back to the target branch, submodules on its pins, so the working copy
	// looks as it did before — what the next command or the operator expects.
	if _, err := r.Git("checkout", "--quiet", m.Target); err != nil {
		return "", "", err
	}

	if _, err := r.Git("submodule", "update", "--recursive"); err != nil {
		return "", "", err
	}

	return sha, url, nil
}

// openMergeRequest opens the request through the project's CLI, or finds the
// one already open for the branch — a rerun updates the branch, not the
// request. It returns the request's URL.
func openMergeRequest(r gitx.Repo, p *config.Project, m *mergeRequest) (string, error) {
	if url := existingMergeRequest(r, p, m); url != "" {
		return url, nil
	}

	var (
		out string
		err error
	)

	switch p.CI {
	case config.CIGitLab:
		args := []string{
			"mr", mrCreate,
			"--source-branch", m.Branch, "--target-branch", m.Target,
			"--title", m.Title, "--remove-source-branch", "--yes",
		}
		out, err = r.Output("glab", append(args, m.args("--description")...)...)
	case config.CIGitHub:
		args := []string{"pr", mrCreate, "--head", m.Branch, "--base", m.Target, "--title", m.Title}
		out, err = r.Output("gh", append(args, m.args("--body")...)...)
	default:
		return "", fmt.Errorf("%w %q", errUnknownCI, p.CI)
	}

	if err != nil {
		return "", fmt.Errorf("open merge request: %w", err)
	}

	return lastLine(out), nil
}

// existingMergeRequest is the URL of the open request for the branch, empty
// when there is none or the lookup fails — a failed lookup falls through to
// creating one, which then says clearly whether one exists.
func existingMergeRequest(r gitx.Repo, p *config.Project, m *mergeRequest) string {
	var (
		out string
		err error
	)

	switch p.CI {
	case config.CIGitLab:
		out, err = r.Output("glab", "api",
			"projects/:id/merge_requests?state=opened&source_branch="+m.Branch)
	case config.CIGitHub:
		out, err = r.Output("gh", "pr", "list", "--state", "open", "--head", m.Branch, "--json", "url")
	default:
		return ""
	}

	if err != nil {
		return ""
	}

	// The two CLIs spell the field differently; both are read.
	var found []struct {
		WebURL string `json:"web_url"` //nolint:tagliatelle // GitLab's own field name
		URL    string `json:"url"`
	}

	if json.Unmarshal([]byte(out), &found) != nil || len(found) == 0 {
		return ""
	}

	return firstNonEmptyString(found[0].WebURL, found[0].URL)
}

// lastLine is the last non-empty line of a CLI's output: where glab and gh
// print the URL of what they created, after whatever they say on the way.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// The request is not the end of the step for a library: the projects after
// it in the run pull its dev branch, so the consumer's bump only sees the
// change once the request is merged. The wait below holds the batch there —
// the operator merges in the browser, rt notices — and, when the config asks,
// a little longer for what the merge sets in motion to land.

// mrDependents names the projects still to come in the run that depend on
// p: those whose .gitmodules on the target branch carries p's url, or whose
// deps_pins variable lists p's module.
func mrDependents(rctx *run.Ctx, p *config.Project) []string {
	var dependents []string

	after := false

	for _, next := range rctx.Selection {
		if next == p {
			after = true

			continue
		}

		if after && dependsOn(rctx, next, p) {
			dependents = append(dependents, next.Name)
		}
	}

	return dependents
}

// dependsOn says whether consumer's target branch pulls p in, as a submodule
// or as a pinned module. A missing clone or an unreadable .gitmodules reads
// as no dependency: the wait is a courtesy to the next project, not a gate.
func dependsOn(rctx *run.Ctx, consumer, p *config.Project) bool {
	r := repoOf(consumer)
	if missingRepo(r) != "" {
		return false
	}

	ref := branchRef(r, consumer.TargetBranch())

	paths, _ := r.SubmodulePathsAt(ref)
	for _, path := range paths {
		if url, err := r.SubmoduleURLAt(ref, path); err == nil && config.SameRemote(url, p.Git) {
			return true
		}
	}

	pin, ok := rctx.Cfg.DepsPinsFor(consumer)
	if !ok {
		return false
	}

	text, found, err := r.FileAt(ref, pin.File)
	if err != nil || !found {
		return false
	}

	lines := strings.Split(text, "\n")

	first, last, ok := variableLines(lines, pin.Var)
	if !ok {
		return false
	}

	for i := first; i <= last; i++ {
		for _, match := range modulePin.FindAllString(lines[i], -1) {
			module, _, _ := strings.Cut(match, "@")
			if config.SameRemote(module, p.Git) {
				return true
			}
		}
	}

	return false
}

// mrWaitFor says whether the step waits for the request to be merged, and
// for whom: the names the plan and the wait message carry, or nil when there
// is no wait.
func mrWaitFor(rctx *run.Ctx, p *config.Project) []string {
	switch rctx.Cfg.MergeRequest.Wait {
	case config.MRWaitNone:
		return nil
	case config.MRWaitAlways:
		return []string{"wait: always"}
	default:
		return mrDependents(rctx, p)
	}
}

// waitPlan is the plan's wording of the wait, empty when there is none.
func waitPlan(rctx *run.Ctx, p *config.Project) string {
	needs := mrWaitFor(rctx, p)
	if len(needs) == 0 {
		return ""
	}

	line := ", then wait for the MR to be merged (" + needsLabel(needs) + ")"

	if settle := rctx.Cfg.MergeRequest.Settle(); settle > 0 {
		line += fmt.Sprintf(" and %s more", settle)
	}

	return line
}

// needsLabel says why the wait is there: which projects need the merge, or
// that the config always waits.
func needsLabel(needs []string) string {
	if len(needs) == 1 && strings.HasPrefix(needs[0], "wait: ") {
		return needs[0]
	}

	return "needed by " + strings.Join(needs, ", ")
}

// mrState is a request's condition as far as the wait cares.
type mrState int

const (
	mrOpen mrState = iota
	mrMerged
	mrClosed // closed without a merge: the change is not coming
)

// waitMerged polls the request at url until it is merged, then settles for
// the configured pause. A closed request and the deadline are errors: the
// next project would pull a dev branch without the change.
func waitMerged(rctx *run.Ctx, r gitx.Repo, p *config.Project, url string, needs []string) error {
	settings := rctx.Cfg.MergeRequest
	deadline := time.Now().Add(settings.WaitFor())

	fmt.Printf("    waiting for %s to be merged (%s)\n", run.Dim(url), needsLabel(needs))
	run.Fire(run.EventWait, map[string]string{
		run.EnvMessage: "waiting for the merge request to be merged (" + needsLabel(needs) + ")",
		run.EnvURL:     url,
	})

	for {
		state, err := mrQuery(r, p, url)
		if err != nil {
			return fmt.Errorf("merge request %s: %w", url, err)
		}

		switch state {
		case mrMerged:
			fmt.Println("    " + run.Green("merged"))

			if settle := settings.Settle(); settle > 0 {
				fmt.Printf("    settling for %s\n", settle)
				time.Sleep(settle)
			}

			return nil
		case mrClosed:
			return fmt.Errorf("%w: %s", errMRClosed, url)
		case mrOpen:
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s: %s", errMRNotMerged, settings.WaitFor(), url)
		}

		time.Sleep(rctx.Cfg.CIPoll())
	}
}

// mrQuery asks the project's CLI for the request's state. GitLab is asked by
// the iid at the end of the URL, GitHub by the URL itself.
func mrQuery(r gitx.Repo, p *config.Project, url string) (mrState, error) {
	var (
		out string
		err error
	)

	switch p.CI {
	case config.CIGitLab:
		out, err = r.Output("glab", "api", "projects/:id/merge_requests/"+lastSegment(url))
	case config.CIGitHub:
		out, err = r.Output("gh", "pr", "view", url, "--json", "state")
	default:
		return mrOpen, fmt.Errorf("%w %q", errUnknownCI, p.CI)
	}

	if err != nil {
		return mrOpen, err
	}

	var answer struct {
		State string `json:"state"`
	}

	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return mrOpen, fmt.Errorf("parse the state: %w", err)
	}

	// GitLab: opened, merged, closed, locked. GitHub: OPEN, MERGED, CLOSED.
	switch strings.ToLower(answer.State) {
	case "merged":
		return mrMerged, nil
	case "closed":
		return mrClosed, nil
	default:
		return mrOpen, nil
	}
}

// lastSegment is the tail of a URL path: the iid of a GitLab request.
func lastSegment(url string) string {
	trimmed := strings.TrimRight(url, "/")

	return trimmed[strings.LastIndex(trimmed, "/")+1:]
}
