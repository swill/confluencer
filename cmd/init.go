package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/swill/confluencer/api"
	cfgpkg "github.com/swill/confluencer/config"
	"github.com/swill/confluencer/tree"
)

var (
	initPageID    string
	initLocalRoot string
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Populate a local repo from an existing Confluence page tree",
	Long: `Fetches a Confluence page tree rooted at --page-id and writes it to
--local-root as a tree of Markdown files. Each file gets a confluence_page_id
and confluence_version front-matter block. Also writes .confluencer.json
(including the cached space key) and the hook shims, then installs them.

After init, review the files and create your initial commit. The first
post-commit hook will seed the local 'confluence' branch from your tree.`,
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringVar(&initPageID, "page-id", "", "Confluence root page ID (required)")
	initCmd.Flags().StringVar(&initLocalRoot, "local-root", "docs/", "Local root directory for the mirrored tree")
	_ = initCmd.MarkFlagRequired("page-id")
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, args []string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	cfgPath := filepath.Join(root, configFile)
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("%s already exists — confluencer is already initialised in this repository", configFile)
	}

	localRoot := initLocalRoot
	if !strings.HasSuffix(localRoot, "/") {
		localRoot += "/"
	}
	attachmentsDir := localRoot + "_attachments"

	creds, err := cfgpkg.LoadCredentials(root)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Fetching page tree from %s (root page %s)...\n", creds.BaseURL, initPageID)

	client := api.NewClient(creds.BaseURL, creds.User, creds.APIToken)

	ct, err := client.FetchTree(initPageID, true)
	if err != nil {
		return fmt.Errorf("fetch tree: %w", err)
	}

	fmt.Fprintf(out, "Found %d pages.\n", ct.Size())

	pm := tree.ComputePaths(ct, localRoot)

	cfg := &cfgpkg.Config{
		RootPageID:     initPageID,
		SpaceKey:       ct.Root.SpaceKey,
		LocalRoot:      localRoot,
		AttachmentsDir: attachmentsDir,
	}

	var fileCount int
	ct.Walk(func(n *tree.CfNode) {
		localPath, ok := pm.Path(n.PageID)
		if !ok {
			fmt.Fprintf(out, "  WARNING: no path computed for page %s (%s)\n", n.PageID, n.Title)
			return
		}

		opts := resolverForPage(localPath, creds.BaseURL, cfg, ct, pm)
		content, err := renderPage(n.PageID, n.Body, n.Version, opts)
		if err != nil {
			fmt.Fprintf(out, "  WARNING: %v\n", err)
			content = ""
		}

		absPath := filepath.Join(root, filepath.FromSlash(localPath))
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			fmt.Fprintf(out, "  ERROR: mkdir for %s: %v\n", localPath, err)
			return
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			fmt.Fprintf(out, "  ERROR: write %s: %v\n", localPath, err)
			return
		}
		fileCount++
	})

	// Download attachments for every page.
	attCount := 0
	ct.Walk(func(n *tree.CfNode) {
		atts, err := client.GetAttachments(n.PageID, "")
		if err != nil {
			fmt.Fprintf(out, "  WARNING: cannot list attachments for %s: %v\n", n.PageID, err)
			return
		}
		if len(atts) == 0 {
			return
		}
		localPath, ok := pm.Path(n.PageID)
		if !ok {
			return
		}
		attDir := tree.AttachmentDir(localPath, localRoot, attachmentsDir)
		for _, att := range atts {
			data, err := client.DownloadAttachment(att.DownloadPath)
			if err != nil {
				fmt.Fprintf(out, "  WARNING: download %s for page %s: %v\n", att.Filename, n.PageID, err)
				continue
			}
			attPath := filepath.Join(root, filepath.FromSlash(attDir), att.Filename)
			if err := os.MkdirAll(filepath.Dir(attPath), 0o755); err != nil {
				fmt.Fprintf(out, "  ERROR: mkdir for %s: %v\n", attPath, err)
				continue
			}
			if err := os.WriteFile(attPath, data, 0o644); err != nil {
				fmt.Fprintf(out, "  ERROR: write %s: %v\n", attPath, err)
				continue
			}
			attCount++
		}
	})

	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("write %s: %w", configFile, err)
	}

	writeGitignoreStub(root)

	if err := writeHookShims(root); err != nil {
		return fmt.Errorf("write hook shims: %w", err)
	}
	if err := installHooks(root, out); err != nil {
		return fmt.Errorf("install hooks: %w", err)
	}

	fmt.Fprintf(out, "\nInitialised confluencer:\n")
	fmt.Fprintf(out, "  Pages:       %d\n", fileCount)
	fmt.Fprintf(out, "  Attachments: %d\n", attCount)
	fmt.Fprintf(out, "  Config:      %s\n", configFile)
	fmt.Fprintf(out, "  Hooks:       .confluencer/hooks/ → .git/hooks/\n")
	fmt.Fprintln(out, "\nReview the files, then `git add` and commit.")

	return nil
}

// writeHookShims creates .confluencer/hooks/ and writes the Git hook shims.
// pre-push runs push (which is read-only on the local working tree); the
// post-* hooks run pull, guarded against re-entry by CONFLUENCER_HOOK_ACTIVE
// since pull creates its own commits on the confluence branch.
func writeHookShims(root string) error {
	hooksDir := filepath.Join(root, ".confluencer", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}

	guard := `
if [ -n "$CONFLUENCER_HOOK_ACTIVE" ]; then
  exit 0
fi
export CONFLUENCER_HOOK_ACTIVE=1
`
	shims := map[string]string{
		"pre-push":     "#!/bin/sh\nset -e\nconfluencer push\n",
		"post-commit":  "#!/bin/sh\nset -e\n" + guard + "confluencer pull\n",
		"post-merge":   "#!/bin/sh\nset -e\n" + guard + "confluencer pull\n",
		"post-rewrite": "#!/bin/sh\nset -e\n" + guard + "confluencer pull\n",
	}
	for name, content := range shims {
		path := filepath.Join(hooksDir, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// writeGitignoreStub appends confluencer entries to .gitignore if missing.
// Best effort — does not fail the init on errors.
func writeGitignoreStub(root string) {
	gitignorePath := filepath.Join(root, ".gitignore")
	existing, _ := os.ReadFile(gitignorePath)
	content := string(existing)

	entries := []string{".env"}
	var toAdd []string
	for _, e := range entries {
		if !strings.Contains(content, e) {
			toAdd = append(toAdd, e)
		}
	}
	if len(toAdd) == 0 {
		return
	}

	f, err := os.OpenFile(gitignorePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(content) > 0 && content[len(content)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString("# confluencer\n")
	for _, e := range toAdd {
		f.WriteString(e + "\n")
	}
}

// stubAttachmentResolver resolves attachment image references during
// cf_to_md conversion. It's used by both init and pull via resolverForPage.
type stubAttachmentResolver struct {
	localPath      string
	attachmentsDir string
	localRoot      string
}

func (r *stubAttachmentResolver) AttachmentSrc(filename string) string {
	attDir := tree.AttachmentDir(r.localPath, r.localRoot, r.attachmentsDir)
	return attDir + "/" + filename
}
