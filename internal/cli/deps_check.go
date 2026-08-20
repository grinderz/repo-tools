package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/run"
)

// newDepsCheckCmd runs each deps kind's freshness checks — commands that exit
// non-zero when the dependency files are not what the real commands would
// write, like "go mod tidy -diff" or "uv lock --check". The checks come from
// deps_check_cmds in the config; nothing about a language is built in.
func newDepsCheckCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: "Check that the dependency files are current",
		Long: "Runs the deps kind's check commands from deps_check_cmds in each\n" +
			"project's working copy, through direnv where there is an .envrc — the\n" +
			"same way deps freeze runs the real commands. A check that exits\n" +
			"non-zero marks the project stale, its output says why, and the run\n" +
			"ends with an error naming how many projects need attention. Typical\n" +
			"checks: \"go mod tidy -diff\", \"uv lock --check\". A project whose kind\n" +
			"has no checks is skipped.",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			stale := 0

			for _, p := range projects {
				ok, ran := depsCheckProject(rctx, p)
				if ran && !ok {
					stale++
				}
			}

			if stale > 0 {
				return fmt.Errorf("%d project(s) with stale dependency files", stale)
			}

			return nil
		},
	}
}

// depsCheckProject runs one project's checks; ok says they all passed, ran
// says there was anything to run at all.
func depsCheckProject(rctx *run.Ctx, p *config.Project) (bool, bool) {
	header := run.Cyan("==>") + " " + run.Bold(p.Name)

	cmds := rctx.Cfg.DepsCheckCommands(p)
	if len(cmds) == 0 {
		fmt.Println(header + "  " + run.Dim("no checks for deps kind "+orNoneKind(p.Deps)))

		return true, false
	}

	r := repoOf(p)
	if reason := missingRepo(r); reason != "" {
		fmt.Println(header + "  " + reason)

		return false, true
	}

	fmt.Println(header)

	ok := true
	vars := changelogVars(rctx, p, p.TargetBranch())

	for _, cmd := range cmds {
		cmd = expand(cmd, vars)
		fmt.Printf("    check (%s): %s\n", p.Deps, cmd)

		if err := runShell(rctx, r, cmd, nil); err != nil {
			fmt.Printf("    %s %s\n", run.Fail(), firstLine(err.Error()))

			ok = false
		}
	}

	if ok {
		fmt.Println("    " + run.Green("current"))
	}

	return ok, true
}

// orNoneKind names an unset deps kind the way the config would.
func orNoneKind(kind string) string {
	if kind == "" || kind == config.DepsNone {
		return config.DepsNone
	}

	return kind
}
