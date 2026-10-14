package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
)

// newConfigCmd prints the config as rt reads it: one document, the extends
// chain merged, so what an overlay ends up with can be seen and diffed. It
// does not load the config first: a load error names a line of the merged
// document, and this is where to look that line up.
func newConfigCmd(name string) *cobra.Command {
	return &cobra.Command{
		Use:         name,
		Annotations: map[string]string{skipConfigAnnotation: annotationSet},
		Short:       "Print the effective config, its extends chain merged",
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Root().PersistentFlags().GetString("config")
			if path == "" {
				path = defaultConfigPath()
			}

			doc, err := config.Document(path)
			if err != nil {
				return err
			}

			fmt.Print(string(doc))

			return nil
		},
	}
}
