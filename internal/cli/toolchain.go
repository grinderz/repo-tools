package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// A language version is spread over a repository — the go directive, the CI
// variables, .python-version, requires-python — and over the repositories
// that share a tooling submodule. The jump to the next version is a
// decision, and deps toolchain carries it out: the config names the
// versions, the pins say which line of which file holds each, the commands
// of cmds.toolchain do what is not a line (uv lock), and the commit goes out
// the way every other deps commit does.

type toolchainOpts struct {
	NoCommit   bool
	AllowDirty bool
}

func newToolchainCmd(rctx *run.Ctx, name string) *cobra.Command {
	var opts toolchainOpts

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Put the projects on the language and tool versions the config names",
		Long: "Writes the versions of the toolchain block — {go}, {go_minor}, {python},\n" +
			"whatever keys the config defines — into the lines the pins name, on each\n" +
			"project's target branch, runs its cmds.toolchain commands after them,\n" +
			"then commits and pushes what changed. A project already on the\n" +
			"configured versions commits nothing; a pin whose pattern matches nothing\n" +
			"in a file the project has is an error unless the pin is optional. A\n" +
			"project with no pinned file and no commands is skipped.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))

			for _, p := range projects {
				st, err := planToolchain(rctx, p, opts)
				if err != nil {
					return fmt.Errorf("%s: %w", p.Name, err)
				}

				steps = append(steps, st)
			}

			class := run.Destructive
			if opts.NoCommit {
				class = run.LocalMutate
			}

			ok, err := rctx.Gate(cmdLabel(cmd), class, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().BoolVar(&opts.NoCommit, "no-commit", false,
		"leave the changes in the working tree instead of committing and pushing")
	c.Flags().BoolVar(&opts.AllowDirty, "allow-dirty", false,
		"start on a working tree that already has changes, and commit them along (needs the diff review)")

	return c
}

func planToolchain(rctx *run.Ctx, p *config.Project, opts toolchainOpts) (run.Step, error) {
	r := repoOf(p)
	st := run.Step{Project: p}

	if reason := missingRepo(r); reason != "" {
		st.Skip, st.Warn = true, reason

		return st, nil
	}

	if err := fetch(rctx, p, r); err != nil {
		return st, fetchFailed(err)
	}

	branch := p.TargetBranch()

	// Without the branch the plan would read the checkout in its name.
	ref := branchRef(r, branch)
	if ref == "" {
		st.Skip, st.Warn = true, fmt.Sprintf("branch %s %v", branch, errNoBranch)

		return st, nil
	}

	pins, err := pinsAt(r, ref, rctx.Cfg.PinsFor(p))
	if err != nil {
		return st, err
	}

	cmds := rctx.Cfg.ToolchainCommands(p)

	if len(pins) == 0 && len(cmds) == 0 {
		st.Skip, st.Warn = true, fmt.Sprintf("%s %s", errNoToolchainCmds, orNoneKind(p.Deps))

		return st, nil
	}

	vars := toolchainVars(rctx, p, branch)
	message := expand(rctx.Cfg.ToolchainCommitMessageFor(p), vars)

	templates := append(slices.Clone(cmds), config.PinValues(pins)...)

	st.Plan = append(st.Plan, "checkout "+planRef(branch), "toolchain: "+toolchainSummary(rctx, p, templates))
	st.Plan = append(st.Plan, pinPlanLines(pins, vars)...)
	st.Plan = append(st.Plan, direnvPlanLines(rctx, r)...)

	for _, kind := range rctx.Cfg.ToolchainKinds(p) {
		for _, cmd := range kindCommands(rctx.Cfg, p, kind) {
			st.Plan = append(st.Plan, fmt.Sprintf("toolchain (%s): %s", kind, expand(cmd, vars)))
		}
	}

	if opts.NoCommit {
		st.Plan = append(st.Plan, "leave the changes uncommitted")
	} else {
		st.Plan = append(st.Plan,
			commitPlanLines(rctx, p, cmdDepsToolchain, branch, "anything changed", message)...)
	}

	st.Exec = func() error { return bumpToolchain(rctx, p, branch, opts) }

	return st, nil
}

// kindCommands is one kind's list for a project: its own, or the global one.
func kindCommands(cfg *config.Config, p *config.Project, kind string) []string {
	if list, ok := p.Cmds.Toolchain[kind]; ok {
		return list
	}

	return cfg.Cmds.Toolchain[kind]
}

// toolchainSummary names the versions the project's pins and commands
// actually use, so the plan says "go 1.26, uv 0.12" rather than the whole
// block.
func toolchainSummary(rctx *run.Ctx, p *config.Project, templates []string) string {
	versions := rctx.Cfg.ToolchainFor(p)

	var parts []string

	for _, key := range config.ToolchainKeysIn(templates) {
		if version, ok := versions[key]; ok {
			parts = append(parts, key+" "+version)
		}
	}

	if len(parts) == 0 {
		return run.Dim("no version placeholders in the pins or commands")
	}

	return strings.Join(parts, ", ")
}

// toolchainVars are the command placeholders plus {key} and {key_minor} for
// every toolchain version the project is held to.
func toolchainVars(rctx *run.Ctx, p *config.Project, branch string) map[string]string {
	vars := changelogVars(rctx, p, branch)

	for key, version := range rctx.Cfg.ToolchainFor(p) {
		vars[key] = version
		vars[key+"_minor"] = config.MinorOf(version)

		// Load refused the placeholder for a version without one.
		if next, ok := config.NextMinorOf(version); ok {
			vars[key+"_next_minor"] = next
		}
	}

	return vars
}

// toolchainUnchanged is said of a project the bump found nothing to do in.
const toolchainUnchanged = "already on the configured toolchain"

func bumpToolchain(rctx *run.Ctx, p *config.Project, branch string, opts toolchainOpts) error {
	r := repoOf(p)
	if err := startDirty(rctx, r, opts.AllowDirty); err != nil {
		return err
	}

	if err := checkoutTracking(r, branch); err != nil {
		return err
	}

	vars := toolchainVars(rctx, p, branch)

	paths, err := r.FilesAt("")
	if err != nil {
		return err
	}

	hits, _, err := applyPins(r.Dir, paths, rctx.Cfg.PinsFor(p), vars, true)
	for _, hit := range hits {
		fmt.Printf("    pin %s\n", hit)
	}

	if err != nil {
		return withLeftovers(r, err)
	}

	for _, cmd := range rctx.Cfg.ToolchainCommands(p) {
		cmd = expand(cmd, vars)
		fmt.Printf("    toolchain: %s\n", cmd)

		if err := runShell(rctx, r, cmd, nil); err != nil {
			return withLeftovers(r, err)
		}
	}

	if opts.NoCommit {
		return reportUncommitted(r, toolchainUnchanged)
	}

	return commitAndPush(rctx, r, p, cmdDepsToolchain, branch, expand(rctx.Cfg.ToolchainCommitMessageFor(p), vars),
		toolchainUnchanged)
}

// toolchainCheckProject reads a project's pins and runs its
// cmds.toolchain_check, for deps check: ok says the project is on the
// configured versions, ran says there was anything to look at.
func toolchainCheckProject(rctx *run.Ctx, r gitx.Repo, p *config.Project) (bool, bool) {
	ok := true
	vars := toolchainVars(rctx, p, p.TargetBranch())

	paths, err := r.FilesAt("")
	if err != nil {
		fmt.Printf("    %s %s\n", run.Fail(), err)

		return false, true
	}

	hits, applied, err := applyPins(r.Dir, paths, rctx.Cfg.PinsFor(p), vars, false)

	for _, hit := range hits {
		fmt.Printf("    %s pin %s\n", run.Fail(), hit)

		ok = false
	}

	if err != nil {
		for line := range strings.SplitSeq(err.Error(), "\n") { // one per pin that found no line
			fmt.Printf("    %s %s\n", run.Fail(), line)
		}

		ok = false
	}

	if applied > 0 && len(hits) == 0 && err == nil {
		fmt.Printf("    pins: %d on the configured versions\n", applied)
	}

	cmds := rctx.Cfg.ToolchainCheckCommands(p)
	if len(cmds) == 0 {
		return ok, applied > 0 || err != nil
	}

	for _, cmd := range cmds {
		cmd = expand(cmd, vars)
		fmt.Printf("    toolchain check: %s\n", cmd)

		if err := runShell(rctx, r, cmd, nil); err != nil {
			fmt.Printf("    %s %s\n", run.Fail(), firstLine(err.Error()))

			ok = false
		}
	}

	return ok, true
}
