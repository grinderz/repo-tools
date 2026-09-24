package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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

			steps, err := flowCommands(cmd.Root(), rctx, args[0])
			if err != nil {
				return err
			}

			printFlowPlan(args[0], args[1:], steps)

			for i, step := range steps {
				fmt.Printf("\n%s %s (%d/%d)\n",
					run.Marker(), run.Bold(step.Label), i+1, len(steps))

				if err := runStep(cmd.Root(), rctx, step, args[1:]); err != nil {
					return fmt.Errorf("%s: %w", step.Label, err)
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
			if target, _, err := resolveFlowStep(root, name, step); err == nil {
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
func printFlowPlan(name string, projects []string, steps []flowStep) {
	scope := ""
	if len(projects) > 0 {
		scope = " (" + strings.Join(projects, ", ") + ")"
	}

	fmt.Printf("%s %s\n", run.Marker(), run.Bold("flow "+name+" plan"+scope))

	for i, step := range steps {
		fmt.Printf("  %d. %s\n", i+1, run.Bold(step.Label))
	}
}

// flowStep is one resolved step: the command, the flags the step carries,
// and the label the plan and the progress lines print.
type flowStep struct {
	Cmd   *cobra.Command
	Flags []string
	Label string
}

// runStep runs one step with its flags in effect and only there. A step's
// flags are parsed right before it runs; the ones that are the root's — --mr,
// --no-diff, --no-fetch — are derived into the context again, and put back
// the way the command line had them once the step is over, so "deps
// submodules --mr" does not turn the changelog step after it into a merge
// request too. A command's own flags need no undoing: each command runs
// once per flow.
func runStep(root *cobra.Command, rctx *run.Ctx, step flowStep, projects []string) error {
	saved := saveFlags(root.PersistentFlags())

	if err := step.Cmd.ParseFlags(step.Flags); err != nil {
		return fmt.Errorf("step flags: %w", err)
	}

	if err := rctx.ApplyFlags(); err != nil {
		return err
	}

	runErr := step.Cmd.RunE(step.Cmd, projects)

	if err := restoreFlags(root.PersistentFlags(), saved); err != nil {
		return err
	}

	if err := rctx.ApplyFlags(); err != nil {
		return err
	}

	return runErr //nolint:wrapcheck // the flow names the step around it
}

// savedFlag is one flag's value and changed mark, as the command line left it.
type savedFlag struct {
	Value   string
	Changed bool
}

func saveFlags(fs *pflag.FlagSet) map[string]savedFlag {
	saved := map[string]savedFlag{}

	fs.VisitAll(func(f *pflag.Flag) {
		saved[f.Name] = savedFlag{Value: f.Value.String(), Changed: f.Changed}
	})

	return saved
}

func restoreFlags(fs *pflag.FlagSet, saved map[string]savedFlag) error {
	var err error

	fs.VisitAll(func(f *pflag.Flag) {
		was, ok := saved[f.Name]
		if !ok || (f.Value.String() == was.Value && f.Changed == was.Changed) {
			return
		}

		if setErr := f.Value.Set(was.Value); setErr != nil && err == nil {
			err = fmt.Errorf("restore --%s: %w", f.Name, setErr)
		}

		f.Changed = was.Changed
	})

	return err
}

// flowCommands resolves a flow's steps against the real command tree, all of
// them before any runs: a typo or a disabled command must stop the flow while
// nothing has happened yet, not in the middle of a release.
func flowCommands(root *cobra.Command, rctx *run.Ctx, name string) ([]flowStep, error) {
	entries, ok := rctx.Cfg.Flows[name]
	if !ok {
		return nil, fmt.Errorf("flow %q %w%s", name, errUnknownFlow, knownFlows(rctx.Cfg))
	}

	steps := make([]flowStep, 0, len(entries))

	for _, entry := range entries {
		target, flags, err := resolveFlowStep(root, name, entry)
		if err != nil {
			return nil, err
		}

		if err := checkDisabled(target, rctx.Cfg, rctx.ConfigPath); err != nil {
			return nil, fmt.Errorf("flow %s: %w", name, err)
		}

		steps = append(steps, flowStep{
			Cmd:   target,
			Flags: flags,
			Label: strings.Join(append([]string{commandKey(target)}, flags...), " "),
		})
	}

	return steps, nil
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

// resolveFlowStep maps one flow entry onto the command tree and returns the
// command with the flags the entry carries after it, unparsed. It is also
// what every command runs over the whole flows block at startup, so a typo
// in a flow is a config error found today, not on release day when the flow
// first runs. The disable list is not checked here: it is enforced when the
// flow actually runs, like everywhere else.
func resolveFlowStep(root *cobra.Command, name, step string) (*cobra.Command, []string, error) {
	words, flags := splitStep(step)
	key := strings.Join(words, " ")

	target, _, err := root.Find(words)
	if err == nil && target.Name() == flowCmdName {
		return nil, nil, fmt.Errorf("flow %s: %w", name, errFlowInFlow)
	}

	if err != nil || commandKey(target) != key {
		return nil, nil, fmt.Errorf("flow %s: %q %w", name, step, errNotACommand)
	}

	if target.RunE == nil {
		return nil, nil, fmt.Errorf("flow %s: %q %w", name, step, errGroupStep)
	}

	if reason, banned := flowBanned[key]; banned {
		return nil, nil, fmt.Errorf("flow %s: %q %w: %s", name, step, errNotAFlowStep, reason)
	}

	// A run-level flag has no meaning per step: the config and --skip are
	// the run's, and a slice flag could not be undone after the step anyway.
	for _, f := range flags {
		if flag := strings.TrimLeft(f, "-"); flag == "config" || flag == "skip" || f == "-c" {
			return nil, nil, fmt.Errorf("flow %s: %q: %s %w", name, step, f, errNotAStepFlag)
		}
	}

	return target, flags, nil
}

// splitStep separates a flow entry into the command words and the flags
// after them: "deps submodules --mr" is the command deps submodules with
// --mr. Everything from the first dash-word on is a flag, values included.
func splitStep(step string) ([]string, []string) {
	fields := strings.Fields(step)

	for i, f := range fields {
		if strings.HasPrefix(f, "-") {
			return fields[:i], fields[i:]
		}
	}

	return fields, nil
}

// checkFlows resolves every step of every flow and parses its flags, for
// the startup validation. The flags are parsed into a scratch command tree:
// parsing marks flags as set, and a value from one flow must not linger on
// the tree the actual command is about to run on.
func checkFlows(cfg *config.Config) error {
	root := NewRoot()

	for _, name := range slices.Sorted(maps.Keys(cfg.Flows)) {
		for _, step := range cfg.Flows[name] {
			target, flags, err := resolveFlowStep(root, name, step)
			if err != nil {
				return err
			}

			if err := target.ParseFlags(flags); err != nil {
				return fmt.Errorf("flow %s: %q: %w", name, step, err)
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
