package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/changelog"
	"github.com/grinderz/repo-tools/internal/gitx"
)

// skipConfigAnnotation marks a command that must not load the config: it runs
// with a project directory as the working directory, where none exists.
const skipConfigAnnotation = "repo-tools/skip-config"

// annotationSet is the value an annotation carries when it is on, and the
// names of the two commands cobra generates for itself.
const (
	annotationSet  = "true"
	helpCmdName    = "help"
	completionName = "completion"
)

// skipsConfig reports whether a command opted out of config loading.
func skipsConfig(cmd *cobra.Command) bool {
	return cmd.Annotations[skipConfigAnnotation] == annotationSet
}

func newGenChangelogCmd(name string) *cobra.Command {
	var (
		opts changelog.Options
		dir  string
	)

	c := &cobra.Command{
		Use:         name,
		Annotations: map[string]string{skipConfigAnnotation: annotationSet},
		Short:       "Print the plain git-log changelog of a repository to stdout",
		Long: "Groups commits by commit date, newest day first. This is the built-in\n" +
			"replacement for the shared changelog-gen.sh script, so it needs neither\n" +
			"bash nor a checked out tooling submodule.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := changelog.GitLog(gitx.Repo{Dir: dir}, opts)
			if err != nil {
				return err
			}

			if _, err := fmt.Fprint(cmd.OutOrStdout(), out); err != nil {
				return fmt.Errorf("write changelog: %w", err)
			}

			return nil
		},
	}
	c.Flags().StringVar(&opts.Header, "header", "", "first line of the document (default: "+changelog.DefaultHeader+")")
	c.Flags().
		StringVar(&opts.Since, "since", "", "earliest commit date, YYYY-MM-DD (default: "+changelog.DefaultSince+")")
	c.Flags().StringVar(&opts.Until, "until", "", "latest commit date, YYYY-MM-DD (default: today)")
	c.Flags().StringVar(&dir, "repo", ".", "repository directory")

	return c
}
