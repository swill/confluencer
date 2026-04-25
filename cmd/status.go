package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/swill/confluencer/gitutil"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show outstanding changes that would be pushed to Confluence",
	Long: `Lists every .md file that differs between the current branch and the local
'confluence' branch — i.e., everything 'confluencer push' would attempt to
write to Confluence on its next run.`,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	exists, err := gitutil.BranchExists(root, confluenceBranch)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(out, "No %q branch yet — run `confluencer pull` first to seed it.\n", confluenceBranch)
		return nil
	}

	diffs, err := gitutil.DiffBranches(root, confluenceBranch, "HEAD", "*.md")
	if err != nil {
		return fmt.Errorf("diff %s..HEAD: %w", confluenceBranch, err)
	}
	if len(diffs) == 0 {
		fmt.Fprintf(out, "In sync with %s — nothing to push.\n", confluenceBranch)
		return nil
	}

	fmt.Fprintf(out, "%d file(s) differ from %s:\n", len(diffs), confluenceBranch)
	for _, d := range diffs {
		switch d.Action {
		case gitutil.ActionAdded:
			fmt.Fprintf(out, "  add     %s\n", d.Path)
		case gitutil.ActionModified:
			fmt.Fprintf(out, "  modify  %s\n", d.Path)
		case gitutil.ActionDeleted:
			fmt.Fprintf(out, "  delete  %s\n", d.Path)
		case gitutil.ActionRenamed:
			fmt.Fprintf(out, "  rename  %s → %s\n", d.OldPath, d.Path)
		}
	}
	return nil
}
