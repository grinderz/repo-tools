package cli

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// freezeToRelease is the freeze_to value meaning "the submodule's own highest
// release branch".
const freezeToRelease = "release"

func newFreezeDepsCmd(rctx *run.Ctx, name string) *cobra.Command {
	var (
		releaseBranch      string
		noDeps, allowDirty bool
	)

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Pin submodules to release branches, update deps, commit and push",
		Long: "On the release branch: rewrite submodule branches in .gitmodules,\n" +
			"run git submodule update --remote, then run the dependency commands\n" +
			"deps_cmds defines for the project's deps kind and push.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))

			for _, p := range projects {
				st, err := planFreezeDeps(rctx, p, releaseBranch, noDeps, allowDirty)
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
	c.Flags().StringVar(
		&releaseBranch,
		"release-branch",
		"",
		"release branch to freeze (default: the highest release branch on origin)",
	)
	c.Flags().BoolVar(&noDeps, "no-deps", false, "skip the deps_cmds, only move the submodule pins")
	c.Flags().BoolVar(&allowDirty, "allow-dirty", false,
		"start on a working tree that already has changes, and commit them along (needs the diff review)")

	return c
}

func planFreezeDeps(
	rctx *run.Ctx,
	p *config.Project,
	releaseBranch string,
	noDeps, allowDirty bool,
) (run.Step, error) {
	r := repoOf(p)
	st := run.Step{Project: p}

	if !p.DepsFreezeEnabled() {
		st.Skip, st.Warn = true, "deps_freeze is disabled for this project"

		return st, nil
	}

	if reason := missingRepo(r); reason != "" {
		st.Skip, st.Warn = true, reason

		return st, nil
	}

	if err := fetch(rctx, p, r); err != nil {
		return st, fetchFailed(err)
	}

	branch, _, err := latestRelease(r, p, releaseBranch)
	if err != nil {
		// A project with no release branch is one to skip with a reason, not a
		// failure of the batch: the others still have branches to freeze.
		st.Skip, st.Warn = true, err.Error()

		return st, nil //nolint:nilerr // reported as a skip, see above
	}

	// Planning against a branch nobody has created yet would read .gitmodules
	// from the working tree and show a freeze that checkout cannot even start.
	ref, pending := releaseRef(r, branch)
	if pending != "" {
		st.Skip, st.Warn = true, pending

		return st, nil
	}

	st.Plan = append(st.Plan, "checkout "+planRef(branch))
	st.Plan = append(st.Plan, planSubmoduleFreeze(rctx, r, p, ref)...)

	pins, err := planPins(rctx, r, p, ref)
	if err != nil {
		return st, err
	}

	st.Plan = append(st.Plan, pins...)

	// Both halves judge the branch being frozen, not the current checkout: a
	// release branch may carry no submodules at all while the dev branch does.
	if frozen, _ := freezeList(rctx, r, p, ref); len(frozen) == 0 {
		if paths, _ := r.SubmodulePathsAt(ref); len(paths) > 0 {
			st.Warn = "project has submodules but none are configured for freeze"
		}
	}

	if !noDeps {
		st.Plan = append(st.Plan, direnvPlanLines(rctx, r)...)
		st.Plan = append(st.Plan, planDeps(rctx, p, branch)...)
	}

	st.Plan = append(st.Plan,
		commitPlanLines(rctx, p, branch, "anything changed", freezeMessage(rctx, p, branch))...)
	st.Exec = func() error { return freezeDeps(rctx, p, branch, noDeps, allowDirty) }

	return st, nil
}

// freezeList is what a project freezes: the submodules it lists, or — when it
// lists none — every submodule of it that another configured project provides,
// each following that project's own branch.
func freezeList(rctx *run.Ctx, r gitx.Repo, p *config.Project, ref string) ([]config.Submodule, error) {
	if len(p.Submodules) > 0 || !rctx.Cfg.DeriveSubmodulesEnabled() {
		return p.Submodules, nil
	}

	paths, err := r.SubmodulePathsAt(ref)
	if err != nil {
		return nil, err
	}

	derived := make([]config.Submodule, 0, len(paths))

	for _, path := range paths {
		url, err := r.SubmoduleURLAt(ref, path)
		if err != nil {
			continue
		}

		if rctx.Cfg.ProjectByGit(url) == nil {
			continue
		}

		derived = append(derived, config.Submodule{Path: path, FreezeTo: config.FreezeToProject})
	}

	return derived, nil
}

// planSubmoduleFreeze describes the freeze. ref is the revision whose
// .gitmodules the plan reads: the release branch this will run on, not the
// branch that happens to be checked out while planning.
func planSubmoduleFreeze(rctx *run.Ctx, r gitx.Repo, p *config.Project, ref string) []string {
	submodules, err := freezeList(rctx, r, p, ref)
	if err != nil {
		return []string{fmt.Sprintf("cannot read .gitmodules: %v", err)}
	}

	plan := make([]string, 0, len(submodules))

	present, err := r.SubmodulePathsAt(ref)
	if err != nil {
		return []string{fmt.Sprintf("cannot read .gitmodules: %v", err)}
	}

	for _, sm := range submodules {
		// A configured submodule the branch does not have would fail mid-run,
		// so the plan says it instead of promising the work.
		if !slices.Contains(present, sm.Path) {
			plan = append(plan, fmt.Sprintf("submodule %s: MISSING from .gitmodules on %s",
				sm.Path, planRef(refName(ref))))

			continue
		}

		target, err := resolveFreezeBranch(rctx, r, p, sm, ref)
		if err != nil {
			plan = append(plan, fmt.Sprintf("submodule %s: UNRESOLVED (%v)", sm.Path, err))

			continue
		}

		plan = append(plan, fmt.Sprintf("submodule %s -> branch %s, update --remote", sm.Path, planRef(target)))
	}

	return plan
}

// depsCmds returns the dependency commands of a project with the message
// placeholders expanded. Nothing about a language is built in: the commands
// come from deps_cmds in the config.
func depsCmds(rctx *run.Ctx, p *config.Project, branch string) []string {
	cmds := rctx.Cfg.DepsCommands(p)
	vars := changelogVars(rctx, p, branch)
	out := make([]string, 0, len(cmds))

	for _, cmd := range cmds {
		out = append(out, expand(cmd, vars))
	}

	return out
}

// planDeps describes the dependency commands, naming the kind they came from.
func planDeps(rctx *run.Ctx, p *config.Project, branch string) []string {
	cmds := depsCmds(rctx, p, branch)
	plan := make([]string, 0, len(cmds))

	for _, cmd := range cmds {
		plan = append(plan, fmt.Sprintf("deps (%s): %s", p.Deps, cmd))
	}

	return plan
}

// runDeps runs the project's dependency commands in its working copy.
func runDeps(rctx *run.Ctx, r gitx.Repo, p *config.Project, branch string) error {
	for _, cmd := range depsCmds(rctx, p, branch) {
		fmt.Printf("    deps: %s\n", cmd)

		if err := runShell(rctx, r, cmd, nil); err != nil {
			return err
		}
	}

	return nil
}

// resolveFreezeBranch turns a submodule's freeze_to into a concrete branch.
// "release" means the highest release branch of the submodule's own remote.
// When the submodule is itself a configured project with a local clone, its
// remote-tracking branches answer that without hitting the network.
func resolveFreezeBranch(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	sm config.Submodule,
	ref string,
) (string, error) {
	if sm.FreezeTo != freezeToRelease && sm.FreezeTo != config.FreezeToProject {
		return sm.FreezeTo, nil
	}

	url, err := r.SubmoduleURLAt(ref, sm.Path)
	if err != nil {
		return "", err
	}

	// "project" means: whatever branch that project works on in this config,
	// written once in its own definition instead of in every consumer.
	if sm.FreezeTo == config.FreezeToProject {
		provider := rctx.Cfg.ProjectByGit(url)
		if provider == nil {
			return "", fmt.Errorf("%s is not a project in this config, so %q has no branch to follow",
				url, config.FreezeToProject)
		}

		return provider.TargetBranch(), nil
	}

	prefix, branches := submoduleReleaseSource(rctx, p, url)

	if branches == nil {
		branches, err = gitx.LsRemoteBranches(url)
		if err != nil {
			return "", fmt.Errorf("%s: %s", url, firstLine(err.Error())) //nolint:err113 // compact, human-facing
		}
	}

	name, _, ok := gitx.LatestReleaseBranch(branches, prefix)
	if !ok {
		return "", fmt.Errorf("no %sX.Y branch in %s", prefix, url)
	}

	return name, nil
}

// submoduleReleaseSource returns the release prefix to use for a submodule and,
// when the submodule is a configured project cloned locally, its known branches.
func submoduleReleaseSource(rctx *run.Ctx, p *config.Project, url string) (string, []string) {
	for _, other := range rctx.Cfg.Projects {
		if !config.SameRemote(other.Git, url) {
			continue
		}

		if or := repoOf(other); missingRepo(or) == "" {
			branches, _ := or.RemoteBranches()

			return other.ReleaseBranchPrefix, branches
		}

		return other.ReleaseBranchPrefix, nil
	}

	return p.ReleaseBranchPrefix, nil
}

func freezeDeps(rctx *run.Ctx, p *config.Project, branch string, noDeps, allowDirty bool) error {
	r := repoOf(p)
	if err := startDirty(rctx, r, allowDirty); err != nil {
		return err
	}

	if err := checkoutTracking(r, branch); err != nil {
		return err
	}

	// The branch is checked out now, so this is the set the freeze can touch;
	// a configured submodule that is not in it was reported by the plan.
	present, err := r.SubmodulePaths()
	if err != nil {
		return err
	}

	submodules, err := freezeList(rctx, r, p, "")
	if err != nil {
		return err
	}

	for _, sm := range submodules {
		if !slices.Contains(present, sm.Path) {
			fmt.Printf("    %s %s is not in .gitmodules on %s, skipping\n",
				run.Warn(), sm.Path, branch)

			continue
		}

		if err := freezeSubmodule(rctx, r, p, sm); err != nil {
			return withLeftovers(r, err)
		}
	}

	// The refs the deps commands resolve have to be the frozen ones before
	// those commands run, or go.mod would be written from dev heads.
	if err := freezePins(rctx, r, p); err != nil {
		return withLeftovers(r, err)
	}

	if !noDeps {
		if err := runDeps(rctx, r, p, branch); err != nil {
			return withLeftovers(r, err)
		}
	}

	msg := freezeMessage(rctx, p, branch)

	return commitAndPush(rctx, r, p, branch, msg, "nothing to freeze, already up to date")
}

func freezeSubmodule(rctx *run.Ctx, r gitx.Repo, p *config.Project, sm config.Submodule) error {
	// The branch is already checked out here, so the working tree is the right
	// .gitmodules to read.
	target, err := resolveFreezeBranch(rctx, r, p, sm, "")
	if err != nil {
		return fmt.Errorf("submodule %s: %w", sm.Path, err)
	}

	name, err := r.SubmoduleNameByPath(sm.Path)
	if err != nil {
		return err
	}

	if _, err := r.Git("config", "-f", ".gitmodules", "submodule."+name+".branch", target); err != nil {
		return err
	}

	if err := updateSubmoduleRemote(rctx, p, r, sm.Path, target); err != nil {
		return err
	}

	fmt.Printf("    %s -> %s\n", sm.Path, target)

	return nil
}

// freezeMessage is the commit message deps freeze would use, needed both to
// plan and to commit.
func freezeMessage(rctx *run.Ctx, p *config.Project, branch string) string {
	return expand(rctx.Cfg.FreezeCommitMessage, messageVars(rctx, p, branch))
}

// commitAndPush commits everything in the working tree and pushes it, or
// prints noChanges when there is nothing to commit. Unless disabled, the
// staged diff is shown and confirmed first; a project with ci set then has
// the pipeline of the pushed commit watched.
func commitAndPush(rctx *run.Ctx, r gitx.Repo, p *config.Project, branch, msg, noChanges string) error {
	changed, err := r.Git("status", "--porcelain")
	if err != nil {
		return err
	}

	if changed == "" {
		fmt.Println("    " + noChanges)

		return nil
	}

	fmt.Printf("    changed:\n%s\n", changed)

	if _, err := r.Git("add", "-A"); err != nil {
		return err
	}

	ok, err := reviewStaged(rctx, r, p.Name, msg)
	if err != nil {
		return err
	}

	if !ok {
		fmt.Printf("    %s, changes are left staged in %s\n", run.Yellow("declined"), r.Dir)
		fmt.Println(run.Dim(fmt.Sprintf("      git -C %s diff --cached %s   # inspect", r.Dir, submoduleLog)))
		fmt.Println(run.Dim(fmt.Sprintf("      git -C %s reset --hard    # discard", r.Dir)))

		return errDeclined
	}

	if _, err := r.Git("commit", "-m", msg); err != nil {
		return err
	}

	if err := remoteGit(rctx, r, "push", "--no-follow-tags", "origin", branch); err != nil {
		return err
	}

	sha, err := r.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}

	return watchCI(rctx, r, p, sha, branch, msg)
}
