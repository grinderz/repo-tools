package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// newRebaseCmd rebases a branch onto its target, resolving conflicting
// submodule pins automatically — the local twin of the CI submodule-rebase
// job, for the rebases MRs ask for after the target branch moved a pin.
//
// A conflicted submodule is pinned to the freshly fetched head of its
// matching branch: the branch named like the branch being rebased when the
// submodule has one (cross-repo feature work), otherwise the branch the
// submodule tracks in .gitmodules. Re-fetching means a submodule branch that
// was itself rebased is picked up at its current state, not at the stale
// commit the superproject still points to.
//
// Safety, ported as-is from the job: the pin coming from the rebase target
// must be reachable from that head, otherwise the submodule branch is not
// rebased onto its own target yet and resolving here would drop commits;
// when falling back to the tracked branch, the pin coming from the rebased
// commit must be reachable too. Any conflict in a non-submodule path aborts
// the rebase and fails the project.
func newRebaseCmd(rctx *run.Ctx, name string) *cobra.Command {
	var branch, onto string

	var opts rebaseOpts

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Rebase a branch onto its target, auto-resolving submodule pins",
		Long: "Rebases --branch (default: whatever is checked out) onto --onto\n" +
			"(default: the project's target branch) and resolves conflicting\n" +
			"submodule pointers the way the CI submodule-rebase job does: the pin\n" +
			"becomes the freshly fetched head of the submodule's branch named like\n" +
			"the branch being rebased, or of the branch tracked in .gitmodules. A\n" +
			"pin that head does not contain stops the run — the submodule branch\n" +
			"has to be rebased first — and a conflict outside a submodule aborts\n" +
			"and is left to a human.\n\n" +
			"The rebased branch is force-pushed (--force-with-lease) after the\n" +
			"usual confirmation; --no-push keeps it local.\n\n" +
			"--submodules first rebases and force-pushes every submodule branch\n" +
			"named like the one being rebased — recursively, deepest first — onto\n" +
			"the branch that submodule tracks in .gitmodules on the target side.\n" +
			"The parent rebase then picks the fresh heads up, and a pin no conflict\n" +
			"refreshed is re-pinned and committed afterwards, so the branch never\n" +
			"points at commits a force-push orphaned. The submodule pushes happen\n" +
			"even under --no-push: without them the re-pinned parent would\n" +
			"reference commits origin has never seen.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))

			for _, p := range projects {
				st, err := planRebase(rctx, p, branch, onto, opts)
				if err != nil {
					return fmt.Errorf("%s: %w", p.Name, err)
				}

				steps = append(steps, st)
			}

			ok, err := rctx.Gate(cmdLabel(cmd), run.Destructive, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().StringVar(&branch, "branch", "", "branch to rebase (default: the branch each project has checked out)")
	c.Flags().StringVar(&onto, "onto", "", "branch to rebase onto (default: the project's target branch)")
	c.Flags().BoolVar(&opts.NoPush, "no-push", false, "leave the rebased branch local, do not force-push")
	c.Flags().BoolVar(&opts.Submodules, "submodules", false,
		"rebase and push the submodules' same-named branches first, then pull the fresh pins in")

	return c
}

// rebaseOpts is what the flags decide about one rebase run.
type rebaseOpts struct {
	NoPush     bool
	Submodules bool
}

func planRebase(rctx *run.Ctx, p *config.Project, branch, onto string, opts rebaseOpts) (run.Step, error) {
	r := repoOf(p)
	st := run.Step{Project: p}

	if reason := missingRepo(r); reason != "" {
		st.Skip, st.Warn = true, reason

		return st, nil
	}

	// A rebase decided on stale refs rebases onto yesterday's target.
	if err := fetch(rctx, p, r); err != nil {
		return st, fetchFailed(err)
	}

	source, targetRef, reason := rebaseTarget(r, p, branch, onto, opts)
	if reason != "" {
		st.Skip, st.Warn = true, reason

		return st, nil
	}

	st.Plan = []string{"checkout " + planRef(source)}

	if opts.Submodules {
		st.Plan = append(st.Plan,
			fmt.Sprintf("rebase every submodule %s branch onto its tracked branch first, deepest first, "+
				"and push --force-with-lease", planRef(source)))
	}

	st.Plan = append(st.Plan,
		fmt.Sprintf("rebase onto %s, auto-resolving submodule pins", planRef(targetRef)))

	if opts.Submodules {
		st.Plan = append(st.Plan, "re-pin the rebased submodules and commit if a pin still moved")
	}

	if opts.NoPush {
		st.Plan = append(st.Plan, "leave the rebased branch local (--no-push)")
	} else {
		st.Plan = append(st.Plan,
			"push --force-with-lease origin "+planRef(source)+ciPlanSuffix(rctx, p))
	}

	st.Exec = func() error { return rebaseProject(rctx, r, p, source, targetRef, opts) }

	return st, nil
}

// rebaseTarget resolves what to rebase onto what, or the reason this project
// sits the run out.
func rebaseTarget(r gitx.Repo, p *config.Project, branch, onto string, opts rebaseOpts) (string, string, string) {
	source := branch
	if source == "" {
		current, err := r.CurrentBranch()
		if err != nil {
			return "", "", "cannot read the current branch: " + err.Error()
		}

		source = current
	}

	target := onto
	if target == "" {
		target = p.TargetBranch()
	}

	if source == target {
		return "", "", source + " is the target branch itself, nothing to rebase"
	}

	// The flow is one-directional: fixes reach a release branch through
	// cherry-pick, never by rebasing the release line onto anything.
	if _, ok := gitx.ParseReleaseBranch(source, p.ReleaseBranchPrefix); ok {
		return "", "", source + " is a release branch; those are cherry-picked into, not rebased"
	}

	if branchRef(r, source) == "" {
		return "", "", fmt.Sprintf("branch %q exists neither locally nor on origin", source)
	}

	targetRef := branchRef(r, target)
	if targetRef == "" {
		return "", "", fmt.Sprintf("branch %q exists neither locally nor on origin", target)
	}

	// With --submodules a parent already on top may still carry submodule
	// branches that are not, so only the plain form can skip here.
	if _, err := r.Git("merge-base", "--is-ancestor", targetRef, branchRef(r, source)); err == nil && !opts.Submodules {
		return "", "", fmt.Sprintf("%s is already on top of %s", source, targetRef)
	}

	return source, targetRef, ""
}

func rebaseProject(rctx *run.Ctx, r gitx.Repo, p *config.Project, source, targetRef string, opts rebaseOpts) error {
	if err := requireClean(r); err != nil {
		return err
	}

	if err := checkoutForRebase(r, source); err != nil {
		return err
	}

	// Align the submodule worktrees with the source branch so the rebase
	// starts clean — the job's prepare step.
	if _, err := r.Git("submodule", "sync"); err != nil {
		return err
	}

	if _, err := r.Git("submodule", "update", "--init", "--force"); err != nil {
		return err
	}

	var rebasedSubs []string

	if opts.Submodules {
		touched, err := rebaseSubBranches(rctx, r, source, targetRef, 0)
		if err != nil {
			return err
		}

		rebasedSubs = touched
	}

	if err := rebaseLoop(rctx, r, source, targetRef); err != nil {
		return err
	}

	// A pin the rebase had no conflict on still points at the pre-rebase
	// submodule commits, which the force-push above just orphaned.
	if err := repinRebasedSubmodules(rctx, r, p, source, rebasedSubs); err != nil {
		return err
	}

	// The rebased commits may pin the submodules elsewhere; bring the
	// worktrees along or the tree looks dirty afterwards.
	if _, err := r.Git("submodule", "update", "--init", "--recursive"); err != nil {
		return err
	}

	rebased, err := r.Lines("log", "--oneline", targetRef+"..HEAD")
	if err != nil {
		return err
	}

	fmt.Printf("    rebased %d commit(s) onto %s\n", len(rebased), planRef(targetRef))

	for _, line := range rebased {
		fmt.Println("      " + line)
	}

	// What actually changed in the patches — resolved pins included — is the
	// thing to read before rewriting the remote branch.
	showRangeDiff(rctx, r, source)

	if opts.NoPush {
		fmt.Println("    " + run.Dim("left local (--no-push): git -C "+r.Dir+" push --force-with-lease origin "+source))

		return nil
	}

	return pushRebased(rctx, r, p, source)
}

// pushRebased is the confirmed force-push of the rebased branch, watched to
// the end of its pipeline like every other push.
func pushRebased(rctx *run.Ctx, r gitx.Repo, p *config.Project, source string) error {
	if rctx.Interactive() {
		ok, err := run.Confirm(fmt.Sprintf("Force-push %s to origin?", source))
		if err != nil {
			return err
		}

		if !ok {
			fmt.Printf("    %s, the rebased %s stays local in %s\n", run.Yellow("declined"), source, r.Dir)

			return errDeclined
		}
	}

	if err := remoteGit(rctx, r, "push", "--force-with-lease", "--no-follow-tags", "origin", source); err != nil {
		return err
	}

	sha, err := r.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}

	return watchCI(rctx, r, p, sha, source, "")
}

// showRangeDiff prints how the rebase changed the patches against what
// origin still has — the review a --force-with-lease deserves. A branch that
// was never pushed has nothing to compare against, and the commit listing
// above already covers it.
func showRangeDiff(rctx *run.Ctx, r gitx.Repo, source string) {
	if !rctx.ShowDiff() || !r.RemoteBranchExists(source) {
		return
	}

	out, err := r.Git("range-diff", colorArg(), "origin/"+source+"..."+source)
	if err != nil || strings.TrimSpace(out) == "" {
		return
	}

	fmt.Println()
	printCapped(rctx, out, "git -C "+r.Dir+" range-diff origin/"+source+"..."+source)
}

// checkoutForRebase checks the branch out without judging its state: unlike
// the batch commands, a rebase exists to act on a branch that is ahead of,
// behind, or entirely off origin.
func checkoutForRebase(r gitx.Repo, branch string) error {
	if r.LocalBranchExists(branch) {
		_, err := r.Git("checkout", branch)

		return err
	}

	_, err := r.Git("checkout", "-b", branch, "--track", "origin/"+branch)

	return err
}

// rebaseLoop is the job's driver: rebase, and while git stops, resolve the
// submodule conflicts and continue. Whatever the automation cannot resolve
// aborts the rebase — or, on a terminal, may be left in progress for a hand
// that already has the submodule pins resolved for free.
func rebaseLoop(rctx *run.Ctx, r gitx.Repo, source, targetRef string) error {
	out, err := r.Git("rebase", targetRef)
	if err == nil {
		fmt.Println("    " + run.Green("rebase finished") + ", no conflicts")

		return nil
	}

	if !rebaseInProgress(r) {
		return fmt.Errorf("rebase failed to start: %s", firstLine(out))
	}

	for rebaseInProgress(r) {
		conflicted, err := r.Lines("diff", "--name-only", "--diff-filter=U")
		if err != nil {
			return abortRebase(r, err)
		}

		if len(conflicted) > 0 {
			if err := resolveRebasePins(r, source, targetRef, conflicted); err != nil {
				return stopRebase(rctx, r, err)
			}
		}

		if cont, err := r.GitEnv(noEditorEnv(), "rebase", "--continue"); err != nil {
			// Skip only a provably empty commit; anything else is unexpected.
			if !stagedEmpty(r) {
				return abortRebase(r, fmt.Errorf("rebase --continue failed with a non-empty index: %s", firstLine(cont)))
			}

			if _, err := r.GitEnv(noEditorEnv(), "rebase", "--skip"); err != nil {
				return abortRebase(r, fmt.Errorf("unable to continue the rebase: %w", err))
			}

			fmt.Println("    " + run.Warn() + " a commit became empty after resolving, skipped")
		}
	}

	fmt.Println("    " + run.Green("rebase finished") + ", submodule conflicts auto-resolved")

	return nil
}

// stagedEmpty reports an index with nothing staged and nothing conflicted —
// the only state where skipping a commit provably loses no work.
func stagedEmpty(r gitx.Repo) bool {
	if _, err := r.Git("diff", "--cached", "--quiet"); err != nil {
		return false
	}

	conflicted, err := r.Lines("diff", "--name-only", "--diff-filter=U")

	return err == nil && len(conflicted) == 0
}

func abortRebase(r gitx.Repo, cause error) error {
	if _, err := r.Git("rebase", "--abort"); err != nil {
		return fmt.Errorf("%w (and the rebase could not be aborted: %w)", cause, err)
	}

	return cause
}

// stopRebase handles what the automation refuses to resolve: a non-submodule
// conflict, or a pin the chosen branch has not absorbed. The default is the
// abort that leaves the branch untouched; on a terminal the rebase can
// instead be kept in progress — every submodule pin resolved so far stays
// resolved, only the rest is left for a hand.
func stopRebase(rctx *run.Ctx, r gitx.Repo, cause error) error {
	if !rctx.Interactive() {
		return abortRebase(r, cause)
	}

	fmt.Printf("\n    %s %v\n", run.Fail(), cause)

	keep, err := run.Confirm("Keep the rebase in progress for manual resolution?")
	if err != nil || !keep {
		return abortRebase(r, cause)
	}

	fmt.Print("    the rebase is left in progress, resolve it by hand:\n")
	fmt.Println(run.Dim("      cd " + r.Dir))
	fmt.Println(run.Dim("      git status"))
	fmt.Println(run.Dim("      git rebase --continue   # or --abort"))

	return fmt.Errorf("manual resolution required: %w", cause)
}

// resolveRebasePins pins every conflicted submodule to the fresh head of its
// matching branch. A conflict anywhere else is a human's job, and aborting
// beats leaving a half-resolved rebase behind.
func resolveRebasePins(r gitx.Repo, source, targetRef string, conflicted []string) error {
	paths, err := r.SubmodulePaths()
	if err != nil {
		return err
	}

	isSubmodule := make(map[string]bool, len(paths))
	for _, path := range paths {
		isSubmodule[path] = true
	}

	for _, path := range conflicted {
		if !isSubmodule[path] {
			return fmt.Errorf("conflict in non-submodule path %q", path)
		}

		if err := resolveOnePin(r, source, targetRef, path); err != nil {
			return err
		}
	}

	return nil
}

func resolveOnePin(r gitx.Repo, source, targetRef, path string) error {
	sub := gitx.Repo{Dir: filepath.Join(r.Dir, path)}

	tracked, err := r.SubmoduleBranch(path)
	if err != nil {
		return err
	}

	branch := rebasePinBranch(sub, source, tracked)
	if branch == "" {
		return fmt.Errorf("submodule %s tracks no branch and has no %q branch of its own", path, source)
	}

	head, err := fetchSubmoduleHead(sub, path, branch)
	if err != nil {
		return err
	}

	// The target side pin must already be part of the branch we resolve to,
	// otherwise that branch still has to be rebased itself.
	if err := checkPinReachable(sub, path, branch, head,
		conflictingPin(r, "2", path), targetRef); err != nil {
		return err
	}

	// No parallel submodule branch: the rebased pin is expected to be merged
	// into the tracked branch; dropping it otherwise would lose work.
	if branch == tracked {
		if err := checkPinReachable(sub, path, branch, head,
			conflictingPin(r, "3", path), "the rebased commit"); err != nil {
			return err
		}
	}

	fmt.Printf("    submodule %s: pinned to head of %s (%s)\n",
		path, planRef(branch), planHash(shorten(head, shortSHALen)))

	_, err = r.Git("update-index", "--cacheinfo", "160000,"+head+","+path)

	return err
}

// rebasePinBranch is the branch a conflicted submodule is pinned to: the one
// named like the branch being rebased when the submodule's origin has it —
// cross-repo feature work — otherwise the branch tracked in .gitmodules.
func rebasePinBranch(sub gitx.Repo, source, tracked string) string {
	if subHasBranch(sub, source) {
		return source
	}

	return tracked
}

// subHasBranch asks the submodule's origin, not the clone: a branch pushed
// after the clone still counts, a branch deleted on origin no longer does.
func subHasBranch(sub gitx.Repo, branch string) bool {
	out, err := sub.Git("ls-remote", "--exit-code", "--heads", "origin", branch)

	return err == nil && strings.TrimSpace(out) != ""
}

// maxSubRebaseDepth stops a submodule cycle from recursing forever; real
// trees here are two levels deep.
const maxSubRebaseDepth = 5

// rebaseSubBranches rebases, deepest first, every submodule branch named like
// the branch being rebased onto the branch that submodule tracks on the
// target side, and force-pushes each — the cross-repo half of the rebase.
// It returns the paths whose branch was handled, for the re-pin afterwards.
func rebaseSubBranches(rctx *run.Ctx, r gitx.Repo, source, targetRef string, depth int) ([]string, error) {
	if depth >= maxSubRebaseDepth {
		return nil, fmt.Errorf("submodules nest deeper than %d levels, refusing to recurse further", maxSubRebaseDepth)
	}

	paths, err := r.SubmodulePaths()
	if err != nil {
		return nil, err
	}

	var touched []string

	for _, path := range paths {
		sub := gitx.Repo{Dir: filepath.Join(r.Dir, path)}

		if reason := missingRepo(sub); reason != "" {
			fmt.Printf("    %s submodule %s: %s\n", run.Warn(), path, reason)

			continue
		}

		if !subHasBranch(sub, source) {
			continue
		}

		if err := rebaseOneSub(rctx, r, sub, path, source, targetRef, depth); err != nil {
			return nil, fmt.Errorf("submodule %s: %w", path, err)
		}

		touched = append(touched, path)
	}

	return touched, nil
}

func rebaseOneSub(rctx *run.Ctx, r, sub gitx.Repo, path, source, targetRef string, depth int) error {
	// The branch the parent's target side expects the submodule on — the
	// same branch the conflict resolver would fall back to.
	target, err := r.SubmoduleBranchAt(targetRef, path)
	if err != nil || target == "" {
		target, err = r.SubmoduleBranch(path)
		if err != nil || target == "" {
			return errors.New("tracks no branch in .gitmodules, run deps freeze first") //nolint:err113 // human-facing
		}
	}

	fmt.Printf("    submodule %s: rebase %s onto %s\n", path, planRef(source), planRef("origin/"+target))

	if _, err := sub.Git("fetch", "--prune", "origin"); err != nil {
		return fetchFailed(err)
	}

	if err := checkoutForRebase(sub, source); err != nil {
		return err
	}

	if _, err := sub.Git("submodule", "sync"); err != nil {
		return err
	}

	if _, err := sub.Git("submodule", "update", "--init", "--force"); err != nil {
		return err
	}

	// Deepest first: this submodule's own submodule branches go before it.
	if _, err := rebaseSubBranches(rctx, sub, source, branchRef(sub, target), depth+1); err != nil {
		return err
	}

	if _, err := sub.Git("merge-base", "--is-ancestor", "origin/"+target, source); err != nil {
		if err := rebaseLoop(rctx, sub, source, "origin/"+target); err != nil {
			return err
		}
	} else {
		fmt.Println(run.Dim("      already on top of origin/" + target))
	}

	return pushSubBranch(rctx, sub, path, source)
}

// pushSubBranch force-pushes the submodule branch, unless origin already has
// exactly this head — a rerun must not ask again about work already pushed.
func pushSubBranch(rctx *run.Ctx, sub gitx.Repo, path, source string) error {
	head, err := sub.Git("rev-parse", source)
	if err != nil {
		return err
	}

	if remote, err := sub.Git("rev-parse", "origin/"+source); err == nil && remote == head {
		fmt.Println(run.Dim("      origin/" + source + " is already at " + shorten(head, shortSHALen)))

		return nil
	}

	if rctx.Interactive() {
		ok, err := run.Confirm(fmt.Sprintf("Force-push submodule %s branch %s to its origin?", path, source))
		if err != nil {
			return err
		}

		if !ok {
			return fmt.Errorf("declined pushing submodule %s; the parent would pin commits origin has never seen", path) //nolint:err113,lll // human-facing
		}
	}

	return remoteGit(rctx, sub, "push", "--force-with-lease", "--no-follow-tags", "origin", source)
}

// repinRebasedSubmodules moves the parent's pins to the freshly pushed heads
// and commits, for the pins the rebase itself had no conflict to refresh on.
// The message comes from rebase_pin_commit_message in the config.
func repinRebasedSubmodules(rctx *run.Ctx, r gitx.Repo, p *config.Project, source string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	for _, path := range paths {
		sub := gitx.Repo{Dir: filepath.Join(r.Dir, path)}

		if _, err := sub.Git("checkout", "--detach", "refs/remotes/origin/"+source); err != nil {
			return fmt.Errorf("submodule %s: %w", path, err)
		}

		if _, err := r.Git("add", "--", path); err != nil {
			return err
		}
	}

	if _, err := r.Git("diff", "--cached", "--quiet"); err == nil {
		return nil // every pin was already refreshed by the rebase itself
	}

	msg := expand(rctx.Cfg.RebasePinMessage(), messageVars(rctx, p, source))
	if _, err := r.Git("commit", "-m", msg); err != nil {
		return err
	}

	fmt.Printf("    re-pinned %s and committed\n", strings.Join(paths, ", "))

	return nil
}

// fetchSubmoduleHead fetches the branch with an explicit refspec — a failed
// fetch must not silently leave a stale head behind — and returns its sha.
func fetchSubmoduleHead(sub gitx.Repo, path, branch string) (string, error) {
	refspec := "refs/heads/" + branch + ":refs/remotes/origin/" + branch
	if _, err := sub.Git("fetch", "--force", "origin", refspec); err != nil {
		return "", fmt.Errorf("cannot fetch branch %q of submodule %s: %s", branch, path, gitReason(err))
	}

	sha, err := sub.Git("rev-parse", "refs/remotes/origin/"+branch)
	if err != nil {
		return "", err
	}

	return sha, nil
}

// conflictingPin is the sha of the conflicting gitlink at the given merge
// stage: stage 2 comes from the rebase target, stage 3 from the commit being
// replayed. Empty when that side has no entry.
func conflictingPin(r gitx.Repo, stage, path string) string {
	lines, err := r.Lines("ls-files", "-u", "--", path)
	if err != nil {
		return ""
	}

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == stage {
			return fields[1]
		}
	}

	return ""
}

// checkPinReachable refuses to resolve over a pin the chosen branch does not
// contain: that branch has not absorbed the commit yet, and pinning past it
// would drop it from the superproject.
func checkPinReachable(sub gitx.Repo, path, branch, head, sha, origin string) error {
	if sha == "" {
		return nil
	}

	if _, err := sub.Git("cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := sub.Git("fetch", "origin", sha); err != nil {
			return fmt.Errorf("submodule %s: commit %s pinned by %s is unknown to the remote",
				path, shorten(sha, shortSHALen), origin)
		}
	}

	if _, err := sub.Git("merge-base", "--is-ancestor", sha, head); err != nil {
		return fmt.Errorf(
			"submodule %s: branch %q (%s) does not contain commit %s pinned by %s, rebase the submodule branch first",
			path, branch, shorten(head, shortSHALen), shorten(sha, shortSHALen), origin)
	}

	return nil
}

// rebaseInProgress mirrors the job's check: git keeps one of two state
// directories for as long as a rebase is unfinished.
func rebaseInProgress(r gitx.Repo) bool {
	gitDir, err := r.Git("rev-parse", "--absolute-git-dir")
	if err != nil {
		return false
	}

	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, state)); err == nil {
			return true
		}
	}

	return false
}
