package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/run"
)

const flowCmdName = "flow"

// newFlowCmd runs a named sequence of commands from the config over one
// project list, so a routine like sync, check, branch, freeze, rc, changelog
// is one command instead of six typed in the right order from memory.
//
// The whole sequence is printed first, then every command runs exactly as it
// would on its own — same plan, same confirmation, same colours. The flow
// stops at the first failure and at the first declined plan: the commands
// after a refused release branch would act on the release that was just
// refused.
func newFlowCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [<name> [project...]]",
		Short: "Run a configured sequence of commands over the same projects",
		Long: "Run the commands a flow lists in the config, in order, over the same\n" +
			"projects. Each command behaves exactly as if it were typed by hand:\n" +
			"it plans, asks and executes on its own. The first failure or declined\n" +
			"plan stops the flow.\n\n" +
			"Without arguments: lists the flows this config defines, steps and all.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				listFlows(cmd.Root(), rctx)

				return nil
			}

			cmds, err := flowCommands(cmd.Root(), rctx, args[0])
			if err != nil {
				return err
			}

			printFlowPlan(args[0], args[1:], cmds)

			for i, step := range cmds {
				fmt.Printf("\n%s %s (%d/%d)\n",
					run.Marker(), run.Bold(cmdLabel(step)), i+1, len(cmds))

				if err := step.RunE(step, args[1:]); err != nil {
					return fmt.Errorf("%s: %w", cmdLabel(step), err)
				}

				if rctx.Aborted {
					fmt.Println(run.Dim("flow stopped, the remaining commands were not run"))

					return nil
				}
			}

			return nil
		},
	}
}

// listFlows prints every flow the config defines, so the sequence is readable
// without opening the yaml. A step the disable list blocks is marked: that
// flow will refuse to start.
func listFlows(root *cobra.Command, rctx *run.Ctx) {
	if len(rctx.Cfg.Flows) == 0 {
		fmt.Println("the config defines no flows")

		return
	}

	for _, name := range slices.Sorted(maps.Keys(rctx.Cfg.Flows)) {
		fmt.Printf("%s %s\n", run.Marker(), run.Bold(name))

		for i, step := range rctx.Cfg.Flows[name] {
			line := fmt.Sprintf("  %d. %s", i+1, step)

			// Startup validation has already resolved every step; here only
			// the disable list can still say no.
			if target, err := resolveFlowStep(root, name, step); err == nil {
				if checkDisabled(target, rctx.Cfg, rctx.ConfigPath) != nil {
					line += "  " + run.Yellow("(disabled — this flow will refuse to start)")
				}
			}

			fmt.Println(line)
		}
	}
}

// printFlowPlan lists the whole sequence before the first command starts
// asking questions, so what the flow is about to do is on the table up front.
func printFlowPlan(name string, projects []string, cmds []*cobra.Command) {
	scope := ""
	if len(projects) > 0 {
		scope = " (" + strings.Join(projects, ", ") + ")"
	}

	fmt.Printf("%s %s\n", run.Marker(), run.Bold("flow "+name+" plan"+scope))

	for i, c := range cmds {
		fmt.Printf("  %d. %s\n", i+1, run.Bold(cmdLabel(c)))
	}
}

// flowCommands resolves a flow's steps against the real command tree, all of
// them before any runs: a typo or a disabled command must stop the flow while
// nothing has happened yet, not in the middle of a release.
func flowCommands(root *cobra.Command, rctx *run.Ctx, name string) ([]*cobra.Command, error) {
	steps, ok := rctx.Cfg.Flows[name]
	if !ok {
		return nil, fmt.Errorf("flow %q %w%s", name, errUnknownFlow, knownFlows(rctx.Cfg))
	}

	cmds := make([]*cobra.Command, 0, len(steps))

	for _, step := range steps {
		target, err := resolveFlowStep(root, name, step)
		if err != nil {
			return nil, err
		}

		if err := checkDisabled(target, rctx.Cfg, rctx.ConfigPath); err != nil {
			return nil, fmt.Errorf("flow %s: %w", name, err)
		}

		cmds = append(cmds, target)
	}

	return cmds, nil
}

// flowBanned names the commands a flow cannot express or should never batch,
// each with the reason the config error carries. Checked at startup like
// everything else about flows: found today, not on release day.
//
//nolint:gochecknoglobals // a fixed table, same as the command tree itself
var flowBanned = map[string]string{
	"git cherry-pick": "it needs one project and a choice of commits, which a flow cannot pass",
	"git rebase":      "it would force-push whatever branch each project happens to have checked out",
	"repo exec":       "the command after -- cannot be written in a flow step",
	"changelog gen":   "it prints one repository's document and works outside the config",
}

// resolveFlowStep maps one flow entry onto the command tree. It is also what
// every command runs over the whole flows block at startup, so a typo in a
// flow is a config error found today, not on release day when the flow first
// runs. The disable list is not checked here: it is enforced when the flow
// actually runs, like everywhere else.
func resolveFlowStep(root *cobra.Command, name, step string) (*cobra.Command, error) {
	key := strings.Join(strings.Fields(step), " ")

	target, _, err := root.Find(strings.Fields(step))
	if err == nil && target.Name() == flowCmdName {
		return nil, fmt.Errorf("flow %s: %w", name, errFlowInFlow)
	}

	if err != nil || commandKey(target) != key {
		return nil, fmt.Errorf("flow %s: %q %w", name, step, errNotACommand)
	}

	if target.RunE == nil {
		return nil, fmt.Errorf("flow %s: %q %w", name, step, errGroupStep)
	}

	if reason, banned := flowBanned[key]; banned {
		return nil, fmt.Errorf("flow %s: %q %w: %s", name, step, errNotAFlowStep, reason)
	}

	return target, nil
}

// checkFlows resolves every step of every flow, for the startup validation.
func checkFlows(root *cobra.Command, cfg *config.Config) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.Flows)) {
		for _, step := range cfg.Flows[name] {
			if _, err := resolveFlowStep(root, name, step); err != nil {
				return err
			}
		}
	}

	return nil
}

// knownFlows names what the config does define, so a typo in a flow name is
// answered with the list to pick from rather than a bare refusal.
func knownFlows(cfg *config.Config) string {
	if len(cfg.Flows) == 0 {
		return ", and it defines no flows at all"
	}

	return " (defined: " + strings.Join(slices.Sorted(maps.Keys(cfg.Flows)), ", ") + ")"
}
