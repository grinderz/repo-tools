// Package cli wires the CLI commands.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func defaultConfigPath() string {
	if v := os.Getenv("REPO_TOOLS_CONFIG"); v != "" {
		return v
	}

	if wd, err := os.Getwd(); err == nil {
		if p := filepath.Join(wd, "config.yaml"); fileExists(p) {
			return p
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "repo-tools", "config.yaml")
	}

	return "config.yaml"
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)

	return err == nil && !fi.IsDir()
}

func NewRoot() *cobra.Command {
	var (
		cfgPath        string
		diff, noDiff   bool
		fetch, noFetch bool
		color, noColor bool
	)

	rctx := &run.Ctx{}

	root := &cobra.Command{
		Use:           "rt",
		Short:         "Batch workflows over a configured set of git repositories",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == helpCmdName || cmd.Name() == completionName || skipsConfig(cmd) {
				return nil
			}

			path := cfgPath
			if path == "" {
				path = defaultConfigPath()
			}

			cfg, err := config.Load(path)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			rctx.Cfg = cfg
			rctx.ConfigPath = path

			if err := checkDisabled(cmd, cfg, path); err != nil {
				return err
			}

			if err := checkFlows(cmd.Root(), cfg); err != nil {
				return err
			}

			if err := rejectBothFlags(diff, noDiff, "--diff", "--no-diff"); err != nil {
				return err
			}

			if err := rejectBothFlags(fetch, noFetch, "--fetch", "--no-fetch"); err != nil {
				return err
			}

			if err := rejectBothFlags(color, noColor, "--color", "--no-color"); err != nil {
				return err
			}

			rctx.DiffFlag = override(diff, noDiff)
			rctx.FetchFlag = override(fetch, noFetch)
			rctx.ColorFlag = override(color, noColor)
			run.SetColor(rctx.UseColor())
			gitx.SetPlainOutput(!rctx.UseColor())

			reportInputs(path, rctx)

			return nil
		},
	}

	flags := root.PersistentFlags()
	flags.StringVarP(
		&cfgPath,
		"config",
		"c",
		"",
		"path to config (default: ./config.yaml or ~/.config/repo-tools/config.yaml)",
	)
	flags.BoolVar(&rctx.DryRun, "dry-run", false, "print the plan and exit without executing")
	flags.BoolVarP(&rctx.Yes, "yes", "y", false, "do not ask for confirmation")
	flags.BoolVar(&rctx.ForceConfirm, "confirm", false, "ask for confirmation even when config says never")
	flags.BoolVar(&fetch, "fetch", false, "fetch from origin even where a command would not")
	flags.BoolVar(&noFetch, "no-fetch", false, "do not fetch, work with the refs already present")
	flags.BoolVar(&diff, "diff", false, "show the staged diff and ask before every commit (on by default)")
	flags.BoolVar(&noDiff, "no-diff", false, "commit without showing the diff and asking")
	flags.BoolVar(&color, "color", false, "colour diffs, warnings and errors (default: when on a terminal)")
	flags.BoolVar(&noColor, "no-color", false, "plain output, no escape sequences")
	flags.BoolVar(&rctx.NoCI, "no-ci", false,
		"do not watch the pipeline after pushes, whatever the projects' ci settings say")
	flags.BoolVar(&rctx.KeepGoing, "keep-going", false,
		"carry on with the remaining projects after one fails (default: stop at the first)")
	flags.StringSliceVar(&rctx.Skip, "skip", nil, "projects to exclude (comma separated or repeated)")

	root.SetHelpCommand(newHelpCmd(root))
	addCommands(root, rctx)

	return root
}

// addCommands builds the grouped command tree.
func addCommands(root *cobra.Command, rctx *run.Ctx) {
	root.AddCommand(
		group("repo", "Inspect and refresh the working copies",
			newStatusCmd(rctx, "status"),
			newSyncCmd(rctx, "sync"),
			newCheckCmd(rctx, "check"),
			newCleanCmd(rctx, "clean"),
			newReportCmd(rctx, "report"),
			newPruneCmd(rctx, "prune"),
			newExecCmd(rctx, "exec"),
		),
		group("changelog", "Generate and publish changelogs",
			newChangelogCmd(rctx, "update"),
			newGenChangelogCmd("gen"),
		),
		group("release", "Cut release branches and tag releases",
			newReleaseBranchCmd(rctx, "branch"),
			newRcCmd(rctx, "rc"),
			newReleaseCmd(rctx, "tag"),
			newReleaseStatusCmd(rctx, "status"),
			newReleaseNotesCmd(rctx, "notes"),
		),
		group("ci", "Inspect and watch pipelines without pushing",
			newCIStatusCmd(rctx, "status"),
			newCIWatchCmd(rctx, "watch"),
		),
		group("deps", "Manage submodule pins and language dependencies",
			newFreezeDepsCmd(rctx, "freeze"),
			newSubmodulesCmd(rctx, "submodules"),
			newDepsStatusCmd(rctx, "status"),
			newDepsCheckCmd(rctx, "check"),
		),
		group("git", "Move commits between branches",
			newCherryPickCmd(rctx, "cherry-pick"),
			newCompareCmd(rctx, "compare"),
			newRebaseCmd(rctx, "rebase"),
		),
		newFlowCmd(rctx, flowCmdName),
	)
}

// checkDisabled enforces the config's disable list. Every entry is resolved
// against the real command tree, so a typo is a config error rather than a
// prohibition that silently never applies; naming a group disables everything
// under it.
func checkDisabled(cmd *cobra.Command, cfg *config.Config, path string) error {
	running := commandKey(cmd)

	for _, entry := range cfg.Disable {
		want := strings.Join(strings.Fields(entry), " ")

		target, _, err := cmd.Root().Find(strings.Fields(entry))
		if err != nil || commandKey(target) != want {
			return fmt.Errorf("disable: %q is not a command", entry)
		}

		if running == want || strings.HasPrefix(running, want+" ") {
			return fmt.Errorf("%s is disabled in %s", running, path)
		}
	}

	return nil
}

// commandKey is a command's name under the root, the way a config writes it.
func commandKey(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// reportInputs names the inputs a run cannot show anywhere else: which of the
// four candidate config files was picked, and the switches that quietly change
// what the commands do. It goes to stderr so listings stay pipeable.
func reportInputs(path string, rctx *run.Ctx) {
	parts := []string{
		fmt.Sprintf("%s (%d projects)", path, len(rctx.Cfg.Projects)),
		"confirm=" + rctx.Cfg.Confirm,
	}

	if !rctx.ShowDiff() {
		parts = append(parts, "diff=off")
	}

	if rctx.FetchFlag != nil {
		parts = append(parts, "fetch="+map[bool]string{true: "on", false: "off"}[*rctx.FetchFlag])
	}

	if rctx.Yes {
		parts = append(parts, "yes")
	}

	if rctx.DryRun {
		parts = append(parts, "dry-run")
	}

	if rctx.KeepGoing {
		parts = append(parts, "keep-going")
	}

	fmt.Fprintln(os.Stderr, run.Dim("config: "+strings.Join(parts, ", ")))
}

// rejectBothFlags refuses a pair of opposite switches given together: that is a
// contradiction, not a preference.
func rejectBothFlags(on, off bool, onName, offName string) error {
	if on && off {
		return fmt.Errorf("%s and %s are mutually exclusive", onName, offName)
	}

	return nil
}

// override turns a pair of opposite switches into a value, or nil when neither
// was given and the default stands. The values are fresh: returning &off would
// hand back a pointer to true, since off is what the negative flag set.
func override(on, off bool) *bool {
	enabled := true
	disabled := false

	switch {
	case on:
		return &enabled
	case off:
		return &disabled
	default:
		return nil
	}
}

// group bundles related commands under one parent verb.
func group(use, short string, subs ...*cobra.Command) *cobra.Command {
	parent := &cobra.Command{Use: use, Short: short}
	parent.AddCommand(subs...)

	return parent
}
