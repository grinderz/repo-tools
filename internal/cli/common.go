package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// Placeholder names shared by the message templates in the config.
const (
	varVersion        = "version"
	varProductVersion = "product_version"
	varCommits        = "commits"
	varCommitHashes   = "commit_hashes"
	varCommitCount    = "commit_count"
	varBranch         = "branch"
	varProject        = "project"
	varRt             = "rt"
	varTask           = "task"
)

// Lengths used when abbreviating hashes for humans.
const (
	shortSHALen = 12
	shortPinLen = 8
)

// errDeclined marks work the operator refused at a review prompt. It covers a
// commit, a tag, a branch and a discard alike, so it says none of them.
var errDeclined = errors.New("declined at the review")

// submoduleLog makes git spell out which submodule commits a pin move crosses
// instead of printing the two raw hashes.
const submoduleLog = "--submodule=log"

// envrcFile is the direnv configuration a repository may carry.
const envrcFile = ".envrc"

// direnvState says whether a project's commands should go through direnv, and
// whether its .envrc is one direnv will actually load: a blocked .envrc is
// ignored silently, which looks exactly like the variables not being set.
//
// The stat and the PATH lookup are cheap and stay live; only the probe that
// forks direnv is cached per directory, because both the plan and every
// command of a step ask, and a batch over thirteen projects would otherwise
// fork it dozens of times. The cache means a direnv allow issued while rt
// waits at a prompt is not seen until the next run — which is when the
// blocked command would be retried anyway.
func direnvState(rctx *run.Ctx, r gitx.Repo) (bool, bool) {
	if !rctx.Cfg.DirenvEnabled() {
		return false, false
	}

	if _, err := os.Stat(filepath.Join(r.Dir, envrcFile)); err != nil {
		return false, false
	}

	if _, err := exec.LookPath("direnv"); err != nil {
		return false, false
	}

	if blocked, ok := direnvBlocked[r.Dir]; ok {
		return true, blocked
	}

	// A blocked .envrc makes direnv refuse outright, and its message is the
	// only difference between "no variables" and "wrong variables".
	//nolint:gosec // r.Dir comes from the config, and direnv is a fixed program
	out, _ := exec.CommandContext(context.Background(), "direnv", "exec", r.Dir, "true").CombinedOutput()

	blocked := strings.Contains(string(out), "is blocked")
	direnvBlocked[r.Dir] = blocked

	return true, blocked
}

// direnvBlocked holds one probe result per project directory; rt runs its
// batch sequentially, so a plain map is enough.
var direnvBlocked = map[string]bool{} //nolint:gochecknoglobals // per-process probe cache

// pipefail makes a shell command fail when any part of a pipeline does. The
// changelog commands are pipelines writing to a file, so without it a missing
// generator exits through sed with status 0 and leaves an empty file behind.
var (
	pipefailOnce sync.Once //nolint:gochecknoglobals // one shell per process
	pipefailOK   bool      //nolint:gochecknoglobals // one shell per process
)

func pipefailPrefix() string {
	pipefailOnce.Do(func() {
		pipefailOK = exec.CommandContext(context.Background(), "sh", "-c", "set -o pipefail").Run() == nil
	})

	if pipefailOK {
		return "set -o pipefail\n"
	}

	return ""
}

// runShell runs one of the project's own commands, through direnv when the
// project has an .envrc.
func runShell(rctx *run.Ctx, r gitx.Repo, command string, env []string) error {
	command = pipefailPrefix() + command

	use, blocked := direnvState(rctx, r)

	switch {
	case blocked:
		// Running without the .envrc would reach the public proxy instead of
		// the internal one and fail later, in a much less obvious way.
		return fmt.Errorf("%s/%s is blocked, run: direnv allow %s", r.Dir, envrcFile, r.Dir)
	case use:
		return r.ShDirenv(command, env)
	default:
		return r.ShEnv(command, env)
	}
}

// direnvPlanLines mention direnv in the plan, since it changes what the
// commands see.
func direnvPlanLines(rctx *run.Ctx, r gitx.Repo) []string {
	use, blocked := direnvState(rctx, r)
	if !use {
		return nil
	}

	if blocked {
		return []string{"env from .envrc: BLOCKED, run direnv allow in " + r.Dir}
	}

	return []string{"env from .envrc via direnv"}
}

// colorArg tells git whether to colour its own output. Git only colours a
// terminal, and everything here goes through a pipe first, so it has to be
// asked explicitly.
func colorArg() string {
	if run.ColorEnabled() {
		return "--color=always"
	}

	return "--color=never"
}

// A plan line names three kinds of thing, and each keeps one colour in every
// command: a ref — branch, tag or revision — is cyan, a commit or tag message
// is green, everything else stays plain. Enough to find the branch in a wall of
// text without turning the plan into a rainbow.
func planRef(name string) string { return run.Cyan(name) }

func planMsg(msg string) string { return run.Green(msg) }

// planHash paints a commit hash, the yellow git log uses.
func planHash(short string) string { return run.Yellow(short) }

// commitPlanLines describe the commit step: what would be pushed where, and
// with which message — the message is as much a decision as the diff, and
// waiting for the prompt to see it is too late in a dry run.
func commitPlanLines(rctx *run.Ctx, p *config.Project, branch, condition, msg string) []string {
	line := fmt.Sprintf("commit+push to %s if %s", planRef("origin/"+branch), condition)
	if rctx.ShowDiff() {
		line += " (diff shown first)"
	}

	line += ciPlanSuffix(rctx, p)

	return []string{line, "message: " + planMsg(fmt.Sprintf("%q", msg))}
}

// updateSubmoduleRemote moves a submodule to the head of the branch it tracks
// in .gitmodules. The submodule's own clone is fetched first: git only fetches
// there on demand, so a branch created after the clone is missing and
// --remote fails with a raw "Unable to find refs/remotes/origin/X" error.
func updateSubmoduleRemote(rctx *run.Ctx, p *config.Project, r gitx.Repo, path, branch string) error {
	if _, err := r.Git("submodule", "sync", "--", path); err != nil {
		return err
	}

	sub := gitx.Repo{Dir: filepath.Join(r.Dir, path)}

	// An uninitialised submodule is an empty directory inside the parent, so
	// the question is whether a working tree starts here — not whether one
	// exists somewhere above.
	if !sub.IsRepoRoot() {
		if _, err := r.Git("submodule", "update", "--init", "--", path); err != nil {
			return err
		}
	}

	var fetchErr error
	if rctx.WantFetchFor(p, true) {
		_, fetchErr = sub.Git("fetch", "--prune", "origin")
	}

	if !sub.RemoteBranchExists(branch) {
		if fetchErr != nil {
			return fmt.Errorf("submodule %s: cannot fetch origin: %s", path, firstLine(fetchErr.Error()))
		}

		return fmt.Errorf("submodule %s: origin/%s does not exist", path, branch)
	}

	if _, err := r.Git("submodule", "update", "--init", "--remote", "--", path); err != nil {
		return err
	}

	// --remote is not recursive: the submodule just moved to a commit that
	// may record submodule pins of its own, and those have to follow — to
	// the recorded pins, not to branch heads. Left behind, they read as
	// "modified content", dirt inside the submodule that no commit of the
	// parent can absorb, and the next command refuses to start on it.
	if _, err := sub.Git("submodule", "sync", "--recursive"); err != nil {
		return err
	}

	if _, err := sub.Git("submodule", "update", "--init", "--recursive"); err != nil {
		return err
	}

	return nil
}

// pushPlanLine describes the push step, noting the diff review when it is on.
func pushPlanLine(rctx *run.Ctx, p *config.Project, branch string) string {
	line := "push to " + planRef("origin/"+branch)
	if rctx.ShowDiff() {
		line = "review the picked commits, then " + line
	}

	return line + ciPlanSuffix(rctx, p)
}

// reviewUnpushed shows the commits that are on branch but not on origin yet
// and asks whether to push them.
func reviewUnpushed(rctx *run.Ctx, r gitx.Repo, label, branch string) (bool, error) {
	if !rctx.ShowDiff() {
		return true, nil
	}

	rng := "origin/" + branch + ".." + branch

	// --submodule=log turns a pin move from two opaque hashes into the list of
	// submodule commits it crosses, which is the point of the review here.
	log, err := r.Git("log", "--patch", "--stat", colorArg(), submoduleLog, rng)
	if err != nil {
		return false, err
	}

	if log == "" {
		return true, nil
	}

	fmt.Printf("\n%s is about to push to %s:\n", run.Bold(label), run.Bold(branch))
	printCapped(rctx, log, "git -C "+r.Dir+" log -p "+submoduleLog+" "+rng)

	if !rctx.Interactive() {
		return true, nil
	}

	return run.Confirm("Push?")
}

// reviewTag shows what a tag will say and where it will point, and asks before
// creating it. A tag is the one thing here that cannot be amended after the
// push, so it gets the same look-before-you-send treatment as a commit.
func reviewTag(rctx *run.Ctx, r gitx.Repo, label, tag, branch, msg, warn string) (bool, error) {
	if !rctx.ShowDiff() {
		return true, nil
	}

	target, err := r.Git("log", "-1", "--format=%h %s", "origin/"+branch)
	if err != nil {
		return false, err
	}

	fmt.Printf("\n%s is about to tag %s on origin/%s (%s):\n",
		run.Bold(label), run.Bold(tag), branch, run.Dim(target))

	if warn != "" {
		fmt.Println("  " + run.Warn() + " " + warn)
	}

	// The message is a block of its own, not a continuation of the header line
	// above it — it has to read the way it will read in the tag.
	fmt.Println()
	printCapped(rctx, highlightMessage(msg), "git -C "+r.Dir+" log origin/"+branch)

	if !rctx.Interactive() {
		return true, nil
	}

	return run.Confirm("Create and push this tag?")
}

// reviewBranch shows the commit a release branch would start at and asks
// before it is created: everything the release ships is decided here.
func reviewBranch(rctx *run.Ctx, r gitx.Repo, p *config.Project, branch string) (bool, error) {
	if !rctx.ShowDiff() {
		return true, nil
	}

	fmt.Printf("\n%s is about to create %s:\n", run.Bold(p.Name), run.Bold(branch))

	for _, line := range branchSource(r, p) {
		fmt.Println("  " + line)
	}

	if !rctx.Interactive() {
		return true, nil
	}

	return run.Confirm("Create and push this branch?")
}

// commitLine matches a "<short hash> <subject>" line, the shape {commits}
// produces in a tag message.
var commitLine = regexp.MustCompile(`(?m)^([0-9a-f]{7,40}) `)

// highlightMessage paints a tag message for reading: the subject stands out
// from the body, and commit hashes are yellow the way git prints them. It is
// display only — the message pushed with the tag is the plain text, so no
// escape sequence can ever reach the tag itself.
func highlightMessage(msg string) string {
	subject, rest, found := strings.Cut(msg, "\n")

	out := run.Bold(subject)
	if found {
		out += "\n" + commitLine.ReplaceAllStringFunc(rest, func(match string) string {
			return run.Yellow(strings.TrimSpace(match)) + " "
		})
	}

	return out
}

// printCapped shows a review body. On a terminal it goes through a pager, so a
// regenerated changelog can be read in full and scrolled back; piped or with
// no pager it is printed, and then diff_lines keeps a log from filling up.
func printCapped(rctx *run.Ctx, text, more string) {
	if pageText(rctx, text) {
		return
	}

	limit := rctx.Cfg.DiffLimit()

	lines := strings.Split(text, "\n")
	if limit <= 0 || len(lines) <= limit {
		fmt.Println(text)

		return
	}

	fmt.Println(strings.Join(lines[:limit], "\n"))
	fmt.Println(run.Dim(fmt.Sprintf("... %d more lines (diff_lines: %d), see them with: %s",
		len(lines)-limit, limit, more)))
}

// pageText hands the text to the pager and reports whether that worked. It
// only pages a terminal: a pager in a pipe would either block or emit control
// sequences into a log. A single line is printed instead — a one-line tag
// message has nothing to scroll, and a pager for it is a screen to dismiss.
func pageText(rctx *run.Ctx, text string) bool {
	argv := strings.Fields(rctx.Cfg.PagerCommand())
	if len(argv) == 0 || !run.AutoColor() || !strings.Contains(strings.TrimSuffix(text, "\n"), "\n") {
		return false
	}

	if _, err := exec.LookPath(argv[0]); err != nil {
		return false
	}

	//nolint:gosec // the pager comes from the config or $PAGER, as everywhere else
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(text + "\n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run() == nil
}

// reviewStaged shows what is about to be committed and asks to go ahead.
// It returns false when the operator declines.
func reviewStaged(rctx *run.Ctx, r gitx.Repo, label, msg string) (bool, error) {
	if !rctx.ShowDiff() {
		return true, nil
	}

	stat, err := r.Git("diff", "--cached", "--stat", colorArg())
	if err != nil {
		return false, err
	}

	fmt.Printf("\n%s is about to commit:\n%s\n", run.Bold(label), stat)

	diff, err := r.Git("diff", "--cached", colorArg(), submoduleLog)
	if err != nil {
		return false, err
	}

	printCapped(rctx, diff, "git -C "+r.Dir+" diff --cached "+submoduleLog)

	// The message comes last, next to the question it belongs to: after a long
	// diff it would otherwise have scrolled away by the time anyone answers.
	fmt.Printf("\nmessage: %s\n", run.Green(msg))

	if !rctx.Interactive() {
		return true, nil
	}

	return run.Confirm("Commit and push?")
}

// withLeftovers names the work a failed run left behind. Without it the only
// trace is the next run refusing to start on a dirty tree, long after the
// error that caused it has scrolled away.
func withLeftovers(r gitx.Repo, cause error) error {
	changed, err := r.Git("status", "--porcelain")
	if err != nil || changed == "" {
		return cause
	}

	fmt.Printf("    %s stopped with changes left in %s:\n%s\n", run.Warn(), r.Dir, changed)
	fmt.Println(run.Dim(fmt.Sprintf("      git -C %s diff %s        # inspect", r.Dir, submoduleLog)))
	fmt.Println(run.Dim(fmt.Sprintf(
		"      git -C %s checkout -- . && git -C %s submodule update   # discard", r.Dir, r.Dir)))

	return cause
}

func repoOf(p *config.Project) gitx.Repo { return gitx.Repo{Dir: p.Dir()} }

// cmdLabel is how a command names itself in plans and usage: its full path
// without the binary name, so a grouped command reads "release rc".
func cmdLabel(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

func shorten(sha string, n int) string {
	if len(sha) > n {
		return sha[:n]
	}

	return sha
}

// messageVars are the placeholders every template gets: which project, which
// branch, which product release the run belongs to, and the ticket id the
// branch name carries, if any.
func messageVars(rctx *run.Ctx, p *config.Project, branch string) map[string]string {
	return map[string]string{
		varProject:        p.Name,
		varBranch:         branch,
		varProductVersion: rctx.Cfg.ProductVersion,
		varTask:           taskFromBranch(branch),
	}
}

// taskRe is a ticket id the way branch names carry one: feat/AB-123 names
// AB-123. Uppercase on purpose — a looser pattern would read "release-1" out
// of release-1.4.
var taskRe = regexp.MustCompile(`[A-Z][A-Z0-9]*-[0-9]+`)

// taskFromBranch is the ticket id in a branch name, empty when there is none
// — a template using {task} on a plain dev branch renders without it.
func taskFromBranch(branch string) string {
	return taskRe.FindString(branch)
}

// expand replaces {key} placeholders in a message template.
func expand(tpl string, vars map[string]string) string {
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}

	return out
}

// missingRepo reports a step-level reason when the working copy is unusable.
func missingRepo(r gitx.Repo) string {
	if !r.Exists() {
		return fmt.Sprintf("not cloned at %s, run repo sync first", r.Dir)
	}

	if !r.IsRepoRoot() {
		return r.Dir + " is not a git repository"
	}

	return ""
}

// latestRelease resolves the release branch to operate on, in order: the
// --release-branch flag, the project's release_branch in the config, and
// failing both the highest <prefix>X.Y known to origin. The first two are
// taken as given and need not exist yet.
func latestRelease(r gitx.Repo, p *config.Project, override string) (string, gitx.ReleaseVer, error) {
	if override == "" {
		override = p.ReleaseBranch
	}

	if override != "" {
		v, ok := gitx.ParseReleaseBranch(override, p.ReleaseBranchPrefix)
		if !ok {
			return "", gitx.ReleaseVer{}, fmt.Errorf("branch %q does not match %sX.Y", override, p.ReleaseBranchPrefix)
		}

		return override, v, nil
	}

	branches, err := r.RemoteBranches()
	if err != nil {
		return "", gitx.ReleaseVer{}, err
	}

	name, v, ok := gitx.LatestReleaseBranch(branches, p.ReleaseBranchPrefix)
	if !ok {
		return "", gitx.ReleaseVer{}, fmt.Errorf(
			"no %sX.Y branch on origin, run release branch first",
			p.ReleaseBranchPrefix,
		)
	}

	return name, v, nil
}

// releaseRef resolves a release branch to the revision to work on, and reports
// a branch the config names before anyone has created it. Only --release-branch
// or release_branch can name one: the fallback picks the highest branch that
// already exists on origin.
func releaseRef(r gitx.Repo, branch string) (string, string) {
	if ref := branchRef(r, branch); ref != "" {
		return ref, ""
	}

	return "", pendingReleaseWarn(branch)
}

// pendingReleaseWarn is what every command says about a release branch the
// config names before anyone has created it, so they all say it the same way.
func pendingReleaseWarn(branch string) string {
	return fmt.Sprintf("origin/%s does not exist yet, run release branch to create it", branch)
}

// checkoutTracking checks out branch, creating a local tracking branch when
// it only exists on origin, and fast-forwards it to origin.
func checkoutTracking(r gitx.Repo, branch string) error {
	switch {
	case r.LocalBranchExists(branch):
		if _, err := r.Git("checkout", branch); err != nil {
			return err
		}
	case r.RemoteBranchExists(branch):
		if _, err := r.Git("checkout", "-b", branch, "--track", "origin/"+branch); err != nil {
			return err
		}
	default:
		return fmt.Errorf("branch %q exists neither locally nor on origin", branch)
	}

	if !r.RemoteBranchExists(branch) {
		return nil
	}

	if _, err := r.Git("merge", "--ff-only", "origin/"+branch); err != nil {
		return fmt.Errorf("cannot fast-forward %s to origin: %w", branch, err)
	}

	// Committing on top of unpushed local work would push that work too, which
	// is never what a batch command was asked to do.
	ahead, _, err := r.AheadBehind("origin/" + branch)
	if err != nil {
		return err
	}

	if ahead > 0 {
		return fmt.Errorf("%s has %d local commit(s) that are not on origin, push or reset them first",
			branch, ahead)
	}

	return nil
}

// startDirty decides how a command begins on a working tree that is not
// clean. Allowing it means whatever is already there joins the commit, so it
// is only offered to a run someone is watching: the diff review is what turns
// "start dirty" into an informed choice rather than a silent one.
func startDirty(rctx *run.Ctx, r gitx.Repo, allow bool) error {
	if !allow {
		return requireClean(r)
	}

	if !rctx.ShowDiff() || !rctx.Interactive() {
		return errors.New("--allow-dirty needs the diff review and someone to answer it: " +
			"drop --no-diff/--yes, or clean the tree with repo clean")
	}

	changed, err := r.Git("status", "--porcelain")
	if err != nil {
		return err
	}

	if changed != "" {
		fmt.Printf("    %s, these are already here and will be part of the commit:\n%s\n",
			run.Yellow("starting on a dirty tree"), changed)
	}

	return nil
}

// requireClean fails when the working tree has uncommitted changes, naming
// them: the usual cause is a previous run that stopped half way, and "commit
// or stash first" on its own leaves the operator to go and look.
func requireClean(r gitx.Repo) error {
	changed, err := r.Git("status", "--porcelain")
	if err != nil {
		return err
	}

	if changed == "" {
		return nil
	}

	return fmt.Errorf("working tree is dirty, commit or discard first:\n%s\n%s", changed,
		run.Dim(fmt.Sprintf("      git -C %s diff %s        # inspect\n"+
			"      git -C %s checkout -- . && git -C %s submodule update   # discard",
			r.Dir, submoduleLog, r.Dir, r.Dir)))
}

// fetch refreshes remote refs for a command that works against origin, so it
// fetches unless --no-fetch says otherwise.
// remoteAttempts is how many times an unattended run tries a remote git
// command before reporting the failure: the fetch or push of a step is also
// its single point of network flakiness, and a release flow once lost its
// tagging step to a blip that a second attempt would have absorbed.
const (
	remoteAttempts   = 3
	remoteRetryPause = 2 * time.Second
)

// remoteGit runs a git command that talks to origin and retries a failure.
// Interactively it asks first — an ssh key on a hardware token wants a touch
// that is easy to miss, so the operator touches the key and answers Enter,
// as many times as needed. An unattended run retries on its own with a pause
// and gives up after remoteAttempts.
func remoteGit(rctx *run.Ctx, r gitx.Repo, args ...string) error {
	var err error

	for attempt := 1; ; attempt++ {
		if _, err = r.Git(args...); err == nil {
			return nil
		}

		if rctx.Interactive() {
			// The whole output, not a one-line summary: "failed to push some
			// refs" once hid the rejected tag that was the actual problem.
			fmt.Println("\n" + run.Dim(indentLines(gitOutput(err))))

			retry, askErr := run.ConfirmYes(fmt.Sprintf(
				"git %s failed — touch the key if it was waiting. Retry?", args[0]))
			if askErr != nil || !retry {
				return err
			}

			continue
		}

		if attempt >= remoteAttempts {
			return err
		}

		fmt.Println("    " + run.Dim(fmt.Sprintf("git %s failed (%s), retrying (%d/%d)",
			args[0], gitReason(err), attempt, remoteAttempts-1)))
		time.Sleep(remoteRetryPause)
	}
}

func fetch(rctx *run.Ctx, p *config.Project, r gitx.Repo) error {
	if !rctx.WantFetchFor(p, true) {
		return nil
	}

	return remoteGit(rctx, r, "fetch", "--prune", "--tags", "origin")
}

// fetchFailed is the error a mutating command reports when its fetch fails
// even after the retries: a skip here would let a flow sail past the step —
// which is how one release lost its rc tag to a missed hardware-key touch.
func fetchFailed(err error) error {
	return fmt.Errorf("fetch failed: %s", gitReason(err)) //nolint:err113 // human-facing
}

// fetchOrWarn refreshes remote refs, returning a compact reason on failure so
// a read-only report can name it and move to the next project.
func fetchOrWarn(rctx *run.Ctx, p *config.Project, r gitx.Repo) string {
	if err := fetch(rctx, p, r); err != nil {
		return "fetch failed: " + gitReason(err)
	}

	return ""
}

// gitReason is the line git actually complains with. The wrapped error leads
// with the command and "exit status 128", which says nothing; and the summary
// "error: failed to push some refs" says little more — the fatal: line or the
// "! [rejected]" detail buried in the output is what the operator needs.
func gitReason(err error) string {
	byPrefix := func(prefix string) string {
		for line := range strings.Lines(err.Error()) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}

		return ""
	}

	for _, prefix := range []string{"fatal:", "! [", "error:"} {
		if line := byPrefix(prefix); line != "" {
			return line
		}
	}

	return firstLine(err.Error())
}

// gitOutput is everything git printed, without the wrapped command line.
func gitOutput(err error) string {
	_, rest, found := strings.Cut(err.Error(), "\n")
	if !found || strings.TrimSpace(rest) == "" {
		return err.Error()
	}

	return rest
}

// indentLines sets a block four spaces in, the step-detail indent.
func indentLines(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}

	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")

	return strings.TrimSpace(line)
}
