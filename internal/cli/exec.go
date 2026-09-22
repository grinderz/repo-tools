package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/run"
)

// newExecCmd runs one shell command in every selected project — the batch
// escape hatch that otherwise becomes a hand-written for loop over thirteen
// directories.
func newExecCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...] -- <command...>",
		Short: "Run one shell command in every project",
		Long: "Runs the command after -- in each project's directory, through direnv\n" +
			"where there is an .envrc — the project's own environment, the way its\n" +
			"deps and changelog commands run. {project}, {branch} (the project's\n" +
			"target branch), {product_version} and {rt} expand per project first.\n" +
			"The command is arbitrary, so the plan is shown and confirmed like any\n" +
			"destructive step.\n\n" +
			"  rt repo exec -- git gc\n" +
			"  rt repo exec api worker -- git log -1 --oneline\n" +
			"  rt repo exec -- echo \"{project} works on {branch}\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash < 0 || dash == len(args) {
				return fmt.Errorf("%w: %s [project...] -- <command...>", errUsage, cmdLabel(cmd))
			}

			command := strings.Join(args[dash:], " ")

			projects, err := rctx.Select(args[:dash])
			if err != nil {
				return err
			}

			steps := execSteps(rctx, projects, command)

			ok, err := rctx.Gate(cmdLabel(cmd), run.Destructive, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
}

// execSteps is one step per project: run the command in its directory, with
// the message placeholders expanded per project — a batch loop can then say
// which project it is in, or call back into rt via {rt}.
func execSteps(rctx *run.Ctx, projects []*config.Project, command string) []run.Step {
	steps := make([]run.Step, 0, len(projects))

	for _, p := range projects {
		r := repoOf(p)
		st := run.Step{Project: p}

		if reason := missingRepo(r); reason != "" {
			st.Skip, st.Warn = true, reason

			steps = append(steps, st)

			continue
		}

		cmd := expand(command, changelogVars(rctx, p, p.TargetBranch()))

		st.Plan = append([]string{"run: " + cmd}, direnvPlanLines(rctx, r)...)
		st.Exec = func() error {
			fmt.Printf("    $ %s\n", cmd)

			return runShell(rctx, r, cmd, nil)
		}

		steps = append(steps, st)
	}

	return steps
}
