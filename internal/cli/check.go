package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func newCheckCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: "Validate the config against the working copies",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			problems := 0

			for _, p := range projects {
				issues := checkProject(rctx, p)

				report(p.Name, issues)

				if len(issues) > 0 {
					problems++
				}
			}

			if problems > 0 {
				return fmt.Errorf("%d project(s) with issues", problems)
			}

			return nil
		},
	}
}

func checkProject(rctx *run.Ctx, p *config.Project) []string {
	r := repoOf(p)
	if reason := missingRepo(r); reason != "" {
		return []string{reason}
	}

	var issues []string

	// Validation is read-only and meant to be quick, so --fetch is opt-in here.
	if rctx.WantFetchFor(p, false) {
		if reason := fetchOrWarn(rctx, p, r); reason != "" {
			issues = append(issues, reason)
		}
	}

	issues = append(issues, checkRemote(r, p)...)
	issues = append(issues, checkReleaseBranch(r, p)...)
	issues = append(issues, checkWorktree(r)...)
	issues = append(issues, checkSubmodules(rctx, r, p)...)
	issues = append(issues, checkDeps(rctx, p)...)
	issues = append(issues, checkCI(p)...)
	issues = append(issues, checkChangelog(rctx, r, p)...)

	return issues
}

// checkChangelog reports a changelog setup that would fail at generation time.
func checkChangelog(rctx *run.Ctx, r gitx.Repo, p *config.Project) []string {
	if !p.ChangelogEnabled() {
		return nil
	}

	var issues []string

	branch, err := changelogBranch(r, p, "")
	if err != nil {
		return []string{err.Error()}
	}

	cmds := changelogCmds(rctx, p, branch)
	if len(cmds) == 0 {
		issues = append(issues, "changelog is enabled but no changelog_cmds are set")
	}

	issues = append(issues, checkPrograms("changelog", cmds)...)

	// A git-cliff config usually lives in a tooling submodule, which is only
	// present once that submodule is checked out.
	cliffConfig := changelogEnv(rctx, p, branch)["GIT_CLIFF_CONFIG"]
	if cliffConfig == "" {
		return issues
	}

	if _, err := os.Stat(filepath.Join(r.Dir, cliffConfig)); err != nil {
		issues = append(issues, fmt.Sprintf(
			"GIT_CLIFF_CONFIG %s is missing, changelog update checks submodules out before generating",
			cliffConfig,
		))
	}

	return issues
}

func report(name string, issues []string) {
	if len(issues) == 0 {
		fmt.Printf("%-20s ok\n", name)

		return
	}

	fmt.Printf("%-20s %d issue(s)\n", name, len(issues))

	for _, i := range issues {
		fmt.Printf("  - %s\n", i)
	}
}

func checkRemote(r gitx.Repo, p *config.Project) []string {
	var issues []string

	url, err := r.Git("remote", "get-url", "origin")

	switch {
	case err != nil:
		issues = append(issues, "no origin remote")
	case url != p.Git:
		issues = append(issues, fmt.Sprintf("origin is %s, config says %s", url, p.Git))
	}

	if !r.RemoteBranchExists(p.DevBranch) {
		issues = append(issues, fmt.Sprintf("origin/%s (dev_branch) does not exist", p.DevBranch))
	}

	// "release" is resolved per project, and the release branch may not exist
	// yet; the pending-branch check above already covers that case.
	if cb := p.ChangelogBranch; cb != "" && cb != changelogBranchRelease && !r.RemoteBranchExists(cb) {
		issues = append(issues, fmt.Sprintf("origin/%s (changelog_branch) does not exist", cb))
	}

	return issues
}

func checkWorktree(r gitx.Repo) []string {
	var issues []string

	if clean, err := r.IsClean(); err == nil && !clean {
		issues = append(issues, "working tree is dirty")
	}

	branch, err := r.CurrentBranch()
	if err != nil {
		return append(issues, fmt.Sprintf("cannot read current branch: %v", err))
	}

	if !r.RemoteBranchExists(branch) {
		return issues
	}

	ahead, behind, err := r.AheadBehind("origin/" + branch)
	if err != nil {
		return issues
	}

	if ahead > 0 {
		issues = append(issues, fmt.Sprintf("%s is %d commit(s) ahead of origin", branch, ahead))
	}

	if behind > 0 {
		issues = append(issues, fmt.Sprintf("%s is %d commit(s) behind origin", branch, behind))
	}

	return issues
}

// checkReleaseBranch reports a release branch the config names but nobody has
// created yet. Every project gets one — release branch does not look at
// deps_freeze — and a library others freeze to needs its branch most of all, so
// this is reported for the whole config rather than only where a freeze runs.
func checkReleaseBranch(r gitx.Repo, p *config.Project) []string {
	_, pending := freezeRef(r, p)
	if pending == "" {
		return nil
	}

	return []string{pending}
}

func checkSubmodules(rctx *run.Ctx, r gitx.Repo, p *config.Project) []string {
	var issues []string

	// The freeze these settings describe runs on the release branch, so that is
	// the .gitmodules to judge them against — the checked-out branch may carry a
	// different set of submodules entirely.
	ref, pending := freezeRef(r, p)

	// A config that names a release branch nobody has created yet is the normal
	// state before release branch runs; there is simply nothing to compare the
	// submodule settings against, and checkReleaseBranch has already said so.
	if pending != "" {
		return nil
	}

	actual, err := r.SubmodulePathsAt(ref)
	if err != nil {
		return append(issues, fmt.Sprintf("cannot read .gitmodules: %v", err))
	}

	if ref != "" {
		issues = append(issues, prefixed(ref, checkSubmoduleSet(rctx, r, p, actual, ref))...)

		return issues
	}

	return checkSubmoduleSet(rctx, r, p, actual, ref)
}

// freezeRef is the revision deps freeze would work on. It returns an empty ref
// when there is no release branch at all and the working tree has to do, and
// the pending-branch complaint when the config names one that does not exist
// yet.
func freezeRef(r gitx.Repo, p *config.Project) (string, string) {
	branch, _, err := latestRelease(r, p, "")
	if err != nil {
		return "", ""
	}

	return releaseRef(r, branch)
}

// prefixed names the revision an issue was found on, so a complaint about a
// branch nobody has checked out is not mistaken for one about the working copy.
func prefixed(ref string, issues []string) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, fmt.Sprintf("on %s: %s", ref, i))
	}

	return out
}

func checkSubmoduleSet(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	actual []string,
	ref string,
) []string {
	var issues []string

	have := make(map[string]bool, len(actual))
	for _, a := range actual {
		have[a] = true
	}

	submodules, err := freezeList(rctx, r, p, ref)
	if err != nil {
		return append(issues, fmt.Sprintf("cannot read .gitmodules: %v", err))
	}

	configured := make(map[string]bool, len(submodules))

	for _, sm := range submodules {
		configured[sm.Path] = true

		if !have[sm.Path] {
			issues = append(issues, fmt.Sprintf("submodule %s is in the config but not in .gitmodules", sm.Path))

			continue
		}

		if sm.FreezeTo != freezeToRelease && sm.FreezeTo != config.FreezeToProject {
			continue
		}

		if _, err := resolveFreezeBranch(rctx, r, p, sm, ref); err != nil {
			issues = append(issues, fmt.Sprintf("submodule %s: freeze_to %s unresolvable: %v",
				sm.Path, sm.FreezeTo, err))
		}
	}

	for _, a := range actual {
		if !configured[a] && p.DepsFreezeEnabled() {
			issues = append(issues, fmt.Sprintf("submodule %s is in .gitmodules but not configured for freeze", a))
		}
	}

	return issues
}

// checkCI reports a pipeline watch that cannot run: ci names the system and
// the watch goes through its CLI, so that program has to be reachable — and
// finding out at the first push after a release tag is the worst time.
func checkCI(p *config.Project) []string {
	programs := map[string]string{config.CIGitLab: "glab", config.CIGitHub: "gh"}

	program, ok := programs[p.CI]
	if !ok {
		return nil
	}

	if _, err := exec.LookPath(program); err != nil {
		return []string{fmt.Sprintf("ci (%s): %s is not in PATH", p.CI, program)}
	}

	return nil
}

// checkDeps reports dependency commands that cannot run here: the config says
// which commands a project needs, so the only thing worth verifying is that the
// program each one starts with is actually reachable.
func checkDeps(rctx *run.Ctx, p *config.Project) []string {
	return checkPrograms(fmt.Sprintf("deps (%s)", p.Deps), depsCmds(rctx, p, p.DevBranch))
}

// checkPrograms reports the commands whose program is not on PATH. A missing
// generator is otherwise found only at run time, and a pipeline can even hide
// it: git-cliff | sed exits through sed and writes an empty file.
func checkPrograms(label string, cmds []string) []string {
	var issues []string

	for _, cmd := range cmds {
		for _, program := range pipelinePrograms(cmd) {
			if _, err := exec.LookPath(program); err != nil {
				issues = append(issues, fmt.Sprintf("%s: %s is not in PATH", label, program))
			}
		}
	}

	return issues
}

// pipelinePrograms names the programs a shell command runs: the first word of
// every stage of a pipeline, skipping anything that is shell syntax rather
// than a program.
func pipelinePrograms(cmd string) []string {
	var programs []string

	for stage := range strings.SplitSeq(cmd, "|") {
		fields := strings.Fields(stage)
		if len(fields) == 0 {
			continue
		}

		program := fields[0]
		if strings.ContainsAny(program, "=<>&$(`\"'") {
			continue // a redirection or an assignment, not a program name
		}

		programs = append(programs, program)
	}

	return programs
}
