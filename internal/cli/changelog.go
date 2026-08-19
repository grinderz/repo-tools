package cli

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func newChangelogCmd(rctx *run.Ctx, name string) *cobra.Command {
	var branchOverride string

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Regenerate changelogs, commit and push when files changed",
		Long: "Runs every command of changelog_cmds in order in the project directory,\n" +
			"with changelog_env in the environment, then commits and pushes whatever\n" +
			"the generators changed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))
			for _, p := range projects {
				steps = append(steps, planChangelog(rctx, p, branchOverride))
			}

			ok, err := rctx.Gate(cmdLabel(cmd), run.Destructive, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().StringVar(
		&branchOverride,
		varBranch,
		"",
		"branch to generate the changelog on (default: changelog_branch or dev_branch)",
	)

	return c
}

func planChangelog(rctx *run.Ctx, p *config.Project, branchOverride string) run.Step {
	r := repoOf(p)
	st := run.Step{Project: p}

	switch {
	case !p.ChangelogEnabled():
		st.Skip, st.Warn = true, "changelog is disabled for this project"

		return st
	case missingRepo(r) != "":
		st.Skip, st.Warn = true, missingRepo(r)

		return st
	}

	branch, err := changelogBranch(r, p, branchOverride)
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return st
	}

	st.Plan = append(st.Plan, "checkout "+planRef(branch))

	if rctx.Cfg.InitSubmodulesForChangelog() {
		st.Plan = append(st.Plan, "submodule update --init --recursive")
	}

	st.Plan = append(st.Plan, direnvPlanLines(rctx, r)...)

	env := changelogEnv(rctx, p, branch)
	for _, key := range sortedKeys(env) {
		// Values carry newlines, quote them so the plan stays one line each.
		st.Plan = append(st.Plan, fmt.Sprintf("env %s=%q", key, env[key]))
	}

	for _, cmd := range changelogCmds(rctx, p, branch) {
		st.Plan = append(st.Plan, "run "+cmd)
	}

	st.Plan = append(st.Plan,
		commitPlanLines(rctx, p, branch, "files changed", changelogMessage(rctx, p, branch))...)
	st.Exec = func() error { return runChangelog(rctx, p, branch) }

	return st
}

// changelogBranchRelease is the changelog_branch value meaning "this project's
// release branch": whatever release_branch names, or the highest one on origin.
// It keeps a per-release config from repeating the branch in two fields that
// then drift apart.
const changelogBranchRelease = "release"

// changelogBranch resolves which branch the changelog is generated on: the
// --branch flag, then changelog_branch (with "release" resolved against the
// release branch), then the dev branch.
func changelogBranch(r gitx.Repo, p *config.Project, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	branch := p.ChangelogBranchOrDev()
	if branch != changelogBranchRelease {
		return branch, nil
	}

	release, _, err := latestRelease(r, p, "")
	if err != nil {
		return "", fmt.Errorf("changelog_branch: release: %w", err)
	}

	return release, nil
}

// changelogMessage is the commit message changelog update would use.
func changelogMessage(rctx *run.Ctx, p *config.Project, branch string) string {
	return expand(rctx.Cfg.ChangelogCommitMessage, messageVars(rctx, p, branch))
}

// changelogVars are the placeholders available in commands and env values.
// {rt} lets a command call back into this binary without it being on PATH.
func changelogVars(rctx *run.Ctx, p *config.Project, branch string) map[string]string {
	vars := messageVars(rctx, p, branch)
	vars[varRt] = "rt"

	if self, err := os.Executable(); err == nil {
		vars[varRt] = self
	}

	return vars
}

func changelogCmds(rctx *run.Ctx, p *config.Project, branch string) []string {
	vars := changelogVars(rctx, p, branch)
	cmds := rctx.Cfg.ChangelogCommands(p)
	out := make([]string, 0, len(cmds))

	for _, cmd := range cmds {
		out = append(out, expand(cmd, vars))
	}

	return out
}

func changelogEnv(rctx *run.Ctx, p *config.Project, branch string) map[string]string {
	vars := changelogVars(rctx, p, branch)
	env := rctx.Cfg.ChangelogEnvironment(p)

	for k, v := range env {
		env[k] = expand(v, vars)
	}

	return env
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func envPairs(m map[string]string) []string {
	pairs := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		pairs = append(pairs, k+"="+m[k])
	}

	return pairs
}

func runChangelog(rctx *run.Ctx, p *config.Project, branch string) error {
	r := repoOf(p)
	if err := fetch(rctx, p, r); err != nil {
		return err
	}

	if err := requireClean(r); err != nil {
		return err
	}

	if err := checkoutTracking(r, branch); err != nil {
		return err
	}

	if rctx.Cfg.InitSubmodulesForChangelog() {
		if _, err := r.Git("submodule", "update", "--init", "--recursive"); err != nil {
			return err
		}
	}

	env := envPairs(changelogEnv(rctx, p, branch))
	for _, cmd := range changelogCmds(rctx, p, branch) {
		fmt.Printf("    run %s\n", cmd)

		if err := runShell(rctx, r, cmd, env); err != nil {
			return err
		}
	}

	msg := changelogMessage(rctx, p, branch)

	return commitAndPush(rctx, r, p, branch, msg, "no changelog changes")
}
