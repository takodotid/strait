package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/takodotid/strait/internal/version"
)

type versionView struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func newVersion(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the version of this CLI and the commit it was built from",
		Long: `version shows the version of this CLI and the commit it was built from. It needs no cluster.

strait status shows the version of each site's controller as VERSION, to compare with this one.`,
		Example: `  strait version
  strait version -o json

  # The same, on one line.
  strait --version`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v := versionView{Version: version.Version, Commit: version.Commit}
			return o.write(cmd.OutOrStdout(), v, func(w io.Writer) {
				fields(w, [2]string{"Version", v.Version}, [2]string{"Commit", v.Commit})
			})
		},
	}
}
