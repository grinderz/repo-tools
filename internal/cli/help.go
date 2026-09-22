package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// newHelpCmd replaces cobra's built-in help with one that can dump the whole
// tree. Twelve commands each carrying their own flags are hard to review one
// screen at a time — and impossible to grep — which is what --all is for.
func newHelpCmd(root *cobra.Command) *cobra.Command {
	var all bool

	c := &cobra.Command{
		Use:         helpCmdName + " [command]",
		Short:       "Help about any command, or --all for every command at once",
		Annotations: map[string]string{skipConfigAnnotation: annotationSet},
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return printTreeHelp(root, cmd.OutOrStdout())
			}

			target, _, err := root.Find(args)
			if err != nil {
				return fmt.Errorf("%w %q", errUnknownCmd, strings.Join(args, " "))
			}

			return target.Help()
		},
	}
	c.Flags().BoolVar(&all, "all", false, "print the help of every command, parents and leaves alike")

	return c
}

// printTreeHelp walks the whole command tree, cobra's own order, skipping the
// generated shell-completion branch nobody reads this way.
func printTreeHelp(root *cobra.Command, out io.Writer) error {
	// The rule is as wide as the indented command path it underlines.
	const indent = "    "

	rule := func(path string) string { return strings.Repeat("=", len(indent)*2+len(path)) }

	var walk func(cmd *cobra.Command) error

	walk = func(cmd *cobra.Command) error {
		path := cmd.CommandPath()
		if _, err := fmt.Fprintf(out, "\n%s\n%s\n%s\n\n", rule(path), indent+path, rule(path)); err != nil {
			return fmt.Errorf("write help: %w", err)
		}

		cmd.SetOut(out)

		if err := cmd.Help(); err != nil {
			return fmt.Errorf("help for %s: %w", cmd.CommandPath(), err)
		}

		for _, sub := range cmd.Commands() {
			if !sub.IsAvailableCommand() || sub.Name() == helpCmdName || sub.Name() == completionName {
				continue
			}

			if err := walk(sub); err != nil {
				return err
			}
		}

		return nil
	}

	return walk(root)
}
