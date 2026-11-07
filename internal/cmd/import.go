package cmd

import "github.com/spf13/cobra"

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import session history from other tools",
	Long: `Import conversation history recorded by other coding assistants into
Crush, so past sessions show up in the session picker and spend reports.

Each importer is a subcommand; run one with --dry-run first to preview
what it would bring over.`,
}

func init() {
	importCmd.AddCommand(importOpenCodeCmd)
}
