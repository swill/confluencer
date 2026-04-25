package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/swill/confluencer/api"
	cfgpkg "github.com/swill/confluencer/config"
	"github.com/swill/confluencer/gitutil"
	"github.com/swill/confluencer/lexer"
)

var pushCmd = &cobra.Command{
	Use:   "push",
	Short: "Write outstanding local Markdown changes to Confluence",
	Long: `Diffs the working branch against the local 'confluence' branch (the
last-known Confluence-mirror state), pushes each change to Confluence, and
advances the confluence branch to reflect what was successfully written.

Failures are not queued: any operation that fails will simply re-appear in
the next push's diff and be retried then.`,
	RunE: runPush,
}

func init() {
	rootCmd.AddCommand(pushCmd)
}

func runPush(cmd *cobra.Command, args []string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	out := cmd.ErrOrStderr()

	cfg, err := cfgpkg.LoadConfig(filepath.Join(root, configFile))
	if err != nil {
		return err
	}
	creds, err := cfgpkg.LoadCredentials(root)
	if err != nil {
		return err
	}

	client := api.NewClient(creds.BaseURL, creds.User, creds.APIToken)

	exists, err := gitutil.BranchExists(root, confluenceBranch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the %q branch doesn't exist — run `confluencer pull` first to seed it", confluenceBranch)
	}

	diffs, err := gitutil.DiffBranches(root, confluenceBranch, "HEAD", "*.md")
	if err != nil {
		return fmt.Errorf("diff %s..HEAD: %w", confluenceBranch, err)
	}
	if len(diffs) == 0 {
		fmt.Fprintf(out, "[confluencer] no changes to push\n")
		return nil
	}

	// Order matters: process index.md files first so directory pages exist
	// on Confluence before any child page tries to look up its parent. Also
	// process deletes after creates/updates within the same push, so a
	// rename + parent-page-empty-out doesn't accidentally cascade-delete.
	sort.Slice(diffs, func(i, j int) bool {
		ai := pushPriority(diffs[i])
		aj := pushPriority(diffs[j])
		if ai != aj {
			return ai < aj
		}
		return diffs[i].Path < diffs[j].Path
	})

	fmt.Fprintf(out, "[confluencer] %d file(s) to push\n", len(diffs))

	var successes []pushOp
	for _, d := range diffs {
		op, err := applyDiff(client, cfg, root, d, &successes, out)
		if err != nil {
			fmt.Fprintf(out, "[confluencer] WARNING: %s %s: %v\n", d.Action, displayPath(d), err)
			continue
		}
		successes = append(successes, op)
	}

	if len(successes) == 0 {
		fmt.Fprintf(out, "[confluencer] nothing pushed (all operations failed)\n")
		return nil
	}

	if err := advanceConfluenceBranch(root, successes, out); err != nil {
		return fmt.Errorf("advance %s branch: %w", confluenceBranch, err)
	}

	fmt.Fprintf(out, "[confluencer] done — %d/%d operation(s) succeeded\n", len(successes), len(diffs))
	return nil
}

// pushPriority orders diffs within a single push run. Lower runs first.
//
//   0: index.md creates/updates (parents)
//   1: non-index .md creates/updates (children)
//   2: renames (after their content updates if applicable)
//   3: deletes
func pushPriority(d gitutil.FileDiff) int {
	switch d.Action {
	case gitutil.ActionDeleted:
		return 3
	case gitutil.ActionRenamed:
		return 2
	}
	if strings.HasSuffix(d.Path, "/index.md") {
		return 0
	}
	return 1
}

func displayPath(d gitutil.FileDiff) string {
	if d.Action == gitutil.ActionRenamed {
		return d.OldPath + " → " + d.Path
	}
	return d.Path
}

// pushOp records a single successful Confluence write so we can replay it on
// the confluence branch afterwards.
type pushOp struct {
	Action  gitutil.FileAction
	OldPath string // populated for Rename, Delete
	NewPath string // populated for Add, Modify, Rename ("" for Delete)
	PageID  string // page ID after the op (for Add: newly assigned)
	Version int    // version after the op
	// HeadContent is the body of the file as it appears on HEAD (without
	// front-matter), kept so advanceConfluenceBranch can rewrite the file
	// on the confluence branch with the canonical front-matter.
	HeadContent string
}

// applyDiff dispatches a single FileDiff to the right Confluence-side handler.
func applyDiff(client *api.Client, cfg *cfgpkg.Config, root string, d gitutil.FileDiff, prevSuccesses *[]pushOp, out io.Writer) (pushOp, error) {
	switch d.Action {
	case gitutil.ActionAdded:
		return applyAdded(client, cfg, root, d.Path, prevSuccesses, out)
	case gitutil.ActionModified:
		return applyModified(client, cfg, root, d.Path, out)
	case gitutil.ActionDeleted:
		return applyDeleted(client, root, d.Path, out)
	case gitutil.ActionRenamed:
		return applyRenamed(client, cfg, root, d.OldPath, d.Path, prevSuccesses, out)
	}
	return pushOp{}, fmt.Errorf("unknown action %q", d.Action)
}

// applyAdded handles a new .md file at path on HEAD. If HEAD's front-matter
// already names a confluence_page_id, we adopt that page (validated via a
// GetPage round-trip); otherwise we POST a new page.
func applyAdded(client *api.Client, cfg *cfgpkg.Config, root, path string, prevSuccesses *[]pushOp, out io.Writer) (pushOp, error) {
	body, fm, err := readBodyAtHead(root, path)
	if err != nil {
		return pushOp{}, fmt.Errorf("read HEAD:%s: %w", path, err)
	}

	// If HEAD's front-matter names a page that genuinely exists, treat as adopt-and-update.
	if fm.PageID != "" {
		if _, getErr := client.GetPage(fm.PageID); getErr == nil {
			return updateExistingPage(client, cfg, root, path, fm.PageID, body, out)
		}
		// page_id stale or wrong — fall through and create fresh.
	}

	storageXML, err := lexer.MdToCf(body, lexer.MdToCfOpts{})
	if err != nil {
		return pushOp{}, fmt.Errorf("convert %s: %w", path, err)
	}

	parentID := ensurePushParents(client, cfg, root, path, prevSuccesses, out)
	title := lexer.ReverseSlugify(filepath.Base(path))

	page, err := client.CreatePage(cfg.SpaceKey, parentID, title, storageXML)
	if err != nil {
		return pushOp{}, fmt.Errorf("create page %s: %w", path, err)
	}

	fmt.Fprintf(out, "[confluencer] create: %s (page %s)\n", path, page.PageID)
	return pushOp{
		Action: gitutil.ActionAdded, NewPath: path,
		PageID: page.PageID, Version: page.Version,
		HeadContent: body,
	}, nil
}

// applyModified handles content changes to an existing tracked .md file.
// The page ID is read from the confluence branch's copy of the file — that's
// the canonical bridge between path and Confluence page.
func applyModified(client *api.Client, cfg *cfgpkg.Config, root, path string, out io.Writer) (pushOp, error) {
	pageID, err := pageIDOnConfluenceBranch(root, path)
	if err != nil {
		return pushOp{}, fmt.Errorf("locate page ID for %s on %s: %w", path, confluenceBranch, err)
	}
	body, _, err := readBodyAtHead(root, path)
	if err != nil {
		return pushOp{}, fmt.Errorf("read HEAD:%s: %w", path, err)
	}
	return updateExistingPage(client, cfg, root, path, pageID, body, out)
}

// applyDeleted handles a .md file that's gone from HEAD but present on the
// confluence branch — meaning the user wants the corresponding Confluence
// page deleted.
func applyDeleted(client *api.Client, root, path string, out io.Writer) (pushOp, error) {
	pageID, err := pageIDOnConfluenceBranch(root, path)
	if err != nil {
		return pushOp{}, fmt.Errorf("locate page ID for deleted %s: %w", path, err)
	}
	if err := client.DeletePage(pageID); err != nil && !api.IsNotFound(err) {
		return pushOp{}, fmt.Errorf("delete page %s: %w", pageID, err)
	}
	fmt.Fprintf(out, "[confluencer] delete: %s (page %s)\n", path, pageID)
	return pushOp{
		Action: gitutil.ActionDeleted, OldPath: path, PageID: pageID,
	}, nil
}

// applyRenamed handles a path-mismatch between confluence branch and HEAD.
// Updates the Confluence page's title (subject to the Title Stability Rule)
// and parent (if the rename crossed a directory boundary), and pushes the
// current body content along with it.
func applyRenamed(client *api.Client, cfg *cfgpkg.Config, root, oldPath, newPath string, prevSuccesses *[]pushOp, out io.Writer) (pushOp, error) {
	pageID, err := pageIDOnConfluenceBranch(root, oldPath)
	if err != nil {
		return pushOp{}, fmt.Errorf("locate page ID for renamed %s: %w", oldPath, err)
	}
	body, _, err := readBodyAtHead(root, newPath)
	if err != nil {
		return pushOp{}, fmt.Errorf("read HEAD:%s: %w", newPath, err)
	}

	page, err := client.GetPage(pageID)
	if err != nil {
		return pushOp{}, fmt.Errorf("fetch page %s for rename: %w", pageID, err)
	}

	// Title Stability Rule: only change the title if the new filename slug
	// no longer matches the current Confluence title's slug.
	newSlug := strings.TrimSuffix(filepath.Base(newPath), ".md")
	title := page.Title
	if !lexer.TitleSlugsMatch(page.Title, newSlug) {
		title = lexer.ReverseSlugify(filepath.Base(newPath))
	}

	// Parent only changes if the directory changed.
	var newParentID string
	if filepath.Dir(oldPath) != filepath.Dir(newPath) {
		newParentID = ensurePushParents(client, cfg, root, newPath, prevSuccesses, out)
	}

	storageXML, err := lexer.MdToCf(body, lexer.MdToCfOpts{})
	if err != nil {
		return pushOp{}, fmt.Errorf("convert %s: %w", newPath, err)
	}

	newVersion, err := updatePageWithRetry(client, pageID, page.Version, title, storageXML, newParentID)
	if err != nil {
		return pushOp{}, err
	}

	fmt.Fprintf(out, "[confluencer] rename: %s → %s (page %s)\n", oldPath, newPath, pageID)
	return pushOp{
		Action: gitutil.ActionRenamed, OldPath: oldPath, NewPath: newPath,
		PageID: pageID, Version: newVersion,
		HeadContent: body,
	}, nil
}

// updateExistingPage is the shared inner of "modify content of page X" used
// by applyModified and the adopt-then-update branch of applyAdded.
func updateExistingPage(client *api.Client, cfg *cfgpkg.Config, root, path, pageID, body string, out io.Writer) (pushOp, error) {
	page, err := client.GetPage(pageID)
	if err != nil {
		return pushOp{}, fmt.Errorf("fetch page %s: %w", pageID, err)
	}
	storageXML, err := lexer.MdToCf(body, lexer.MdToCfOpts{})
	if err != nil {
		return pushOp{}, fmt.Errorf("convert %s: %w", path, err)
	}

	newVersion, err := updatePageWithRetry(client, pageID, page.Version, page.Title, storageXML, "")
	if err != nil {
		return pushOp{}, err
	}

	fmt.Fprintf(out, "[confluencer] update: %s (page %s)\n", path, pageID)
	return pushOp{
		Action: gitutil.ActionModified, NewPath: path,
		PageID: pageID, Version: newVersion,
		HeadContent: body,
	}, nil
}

// updatePageWithRetry tries UpdatePage at version+1 once, and if the server
// returns 409 (concurrent edit between our GET and our PUT), refetches the
// page and retries once with the new version. The retry is best-effort —
// we'll overwrite the conflicting edit. Multi-write conflict resolution is
// out of scope; users coordinate via pull-before-push.
func updatePageWithRetry(client *api.Client, pageID string, currentVersion int, title, storageXML, parentID string) (int, error) {
	newVersion := currentVersion + 1
	err := client.UpdatePage(pageID, newVersion, title, storageXML, parentID)
	if err == nil {
		return newVersion, nil
	}
	if !api.IsConflict(err) {
		return 0, fmt.Errorf("update page %s: %w", pageID, err)
	}
	// 409: refetch and retry once.
	latest, getErr := client.GetPage(pageID)
	if getErr != nil {
		return 0, fmt.Errorf("refetch page %s after 409: %w", pageID, getErr)
	}
	retryVersion := latest.Version + 1
	if retryErr := client.UpdatePage(pageID, retryVersion, title, storageXML, parentID); retryErr != nil {
		return 0, fmt.Errorf("update page %s after 409 retry: %w", pageID, retryErr)
	}
	return retryVersion, nil
}

// readBodyAtHead returns the body of `path` at HEAD with its front-matter
// stripped, plus the parsed front-matter (so callers that adopt an existing
// page_id can read it).
func readBodyAtHead(root, path string) (body string, fm lexer.FrontMatter, err error) {
	data, err := gitutil.ReadFileAtRef(root, "HEAD", path)
	if err != nil {
		return "", lexer.FrontMatter{}, err
	}
	fm, body, err = lexer.ExtractFrontMatter(string(data))
	if err != nil {
		// Treat malformed front-matter as "no front-matter" — body is the
		// whole file. The caller will still try to push it.
		return string(data), lexer.FrontMatter{}, nil
	}
	return body, fm, nil
}

// pageIDOnConfluenceBranch returns the confluence_page_id stored in the
// front-matter of `path` on the confluence branch. This is the canonical
// bridge between local paths and Confluence page IDs.
func pageIDOnConfluenceBranch(root, path string) (string, error) {
	data, err := gitutil.ReadFileAtRef(root, confluenceBranch, path)
	if err != nil {
		return "", err
	}
	fm, _, err := lexer.ExtractFrontMatter(string(data))
	if err != nil {
		return "", fmt.Errorf("front-matter on %s:%s: %w", confluenceBranch, path, err)
	}
	if fm.PageID == "" {
		return "", fmt.Errorf("%s:%s has no confluence_page_id in front-matter", confluenceBranch, path)
	}
	return fm.PageID, nil
}

// ensurePushParents climbs from filePath up to localRoot, creating
// intermediate Confluence pages (and writing intermediate index.md files
// to disk so subsequent pushes in the same run can find them) for any
// directory whose index.md doesn't yet exist. Returns the page ID of the
// immediate parent (or cfg.RootPageID if the file sits directly in the
// local root).
//
// Newly-created intermediates get appended to *prevSuccesses so that
// advanceConfluenceBranch picks them up too — without that step, the
// confluence branch wouldn't know about the intermediate pages.
func ensurePushParents(client *api.Client, cfg *cfgpkg.Config, root, filePath string, prevSuccesses *[]pushOp, out io.Writer) string {
	localRoot := strings.TrimSuffix(cfg.LocalRoot, "/")
	dir := filepath.Dir(filePath)
	if filepath.Base(filePath) == "index.md" {
		dir = filepath.Dir(dir)
	}
	if dir == localRoot || dir == "." || !strings.HasPrefix(dir, localRoot) {
		return cfg.RootPageID
	}
	indexPath := dir + "/index.md"

	// Look in earlier successes first (so a single push that creates
	// nested directories finds intermediate parents created moments ago).
	for _, op := range *prevSuccesses {
		if op.NewPath == indexPath && op.PageID != "" {
			return op.PageID
		}
	}

	// Then check the working tree's existing file (created by an earlier
	// pull or a previous run).
	if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(indexPath))); err == nil {
		if fm, _, fmErr := lexer.ExtractFrontMatter(string(data)); fmErr == nil && fm.PageID != "" {
			return fm.PageID
		}
	}
	// And the confluence branch.
	if data, err := gitutil.ReadFileAtRef(root, confluenceBranch, indexPath); err == nil {
		if fm, _, fmErr := lexer.ExtractFrontMatter(string(data)); fmErr == nil && fm.PageID != "" {
			return fm.PageID
		}
	}

	// Recurse to make sure the grandparent exists.
	grandparentID := ensurePushParents(client, cfg, root, indexPath, prevSuccesses, out)

	title := lexer.ReverseSlugify(filepath.Base(dir) + ".md")
	page, err := client.CreatePage(cfg.SpaceKey, grandparentID, title, "")
	if err != nil {
		fmt.Fprintf(out, "[confluencer] WARNING: create intermediate %s: %v\n", indexPath, err)
		return grandparentID
	}

	// Write the intermediate index.md to the working tree so the file the
	// user committed to (if any) lands in the right place after their next
	// commit. Also so any following file in this push run can read its
	// front-matter for parent lookup.
	fm := lexer.FrontMatter{PageID: page.PageID, Version: page.Version}
	content := lexer.ApplyFrontMatter(fm, "")
	_ = writeLocalFile(root, indexPath, content)

	fmt.Fprintf(out, "[confluencer] create (intermediate): %s (page %s)\n", indexPath, page.PageID)
	*prevSuccesses = append(*prevSuccesses, pushOp{
		Action: gitutil.ActionAdded, NewPath: indexPath,
		PageID: page.PageID, Version: page.Version,
		HeadContent: "",
	})
	return page.PageID
}

// advanceConfluenceBranch checks out the confluence branch, applies every
// successful push op as a file-level change (with canonical front-matter),
// commits them as a single sync commit, and returns to the original branch.
//
// Working-tree state on the original branch is preserved via stash/pop if
// dirty. The confluence branch's tip after this is the new "last-known
// Confluence-mirror state" against which the next push will diff.
func advanceConfluenceBranch(root string, ops []pushOp, out io.Writer) error {
	origBranch, err := gitutil.CurrentBranch(root)
	if err != nil {
		return fmt.Errorf("current branch: %w", err)
	}

	stashed := false
	if clean, _ := gitutil.IsClean(root); !clean {
		if err := gitutil.StashPush(root); err != nil {
			return fmt.Errorf("stash before advancing %s: %w", confluenceBranch, err)
		}
		stashed = true
	}

	if err := gitutil.Checkout(root, confluenceBranch); err != nil {
		if stashed {
			_ = gitutil.StashPop(root)
		}
		return fmt.Errorf("checkout %s: %w", confluenceBranch, err)
	}

	for _, op := range ops {
		if err := applyOpToConfluenceBranch(root, op, out); err != nil {
			fmt.Fprintf(out, "[confluencer] WARNING: replay %s on %s: %v\n", op.NewPath, confluenceBranch, err)
		}
	}

	commitMsg := gitutil.SyncPrefix + "-push @ " + time.Now().UTC().Format(time.RFC3339)
	if _, err := gitutil.CommitAllOnHead(root, commitMsg); err != nil {
		_ = gitutil.Checkout(root, origBranch)
		if stashed {
			_ = gitutil.StashPop(root)
		}
		return fmt.Errorf("commit on %s: %w", confluenceBranch, err)
	}

	if err := gitutil.Checkout(root, origBranch); err != nil {
		return fmt.Errorf("return to %s: %w", origBranch, err)
	}
	if stashed {
		if err := gitutil.StashPop(root); err != nil {
			return fmt.Errorf("stash pop: %w", err)
		}
	}
	return nil
}

// applyOpToConfluenceBranch is the per-op replay on the confluence branch's
// working tree. We're already checked out there.
func applyOpToConfluenceBranch(root string, op pushOp, out io.Writer) error {
	switch op.Action {
	case gitutil.ActionAdded, gitutil.ActionModified:
		return writeManagedFile(root, op.NewPath, op.PageID, op.Version, op.HeadContent)
	case gitutil.ActionDeleted:
		// Best-effort: ignore "not found" so a noop replay doesn't fail.
		if err := gitutil.Remove(root, op.OldPath); err != nil {
			fmt.Fprintf(out, "[confluencer] WARNING: git rm %s on %s: %v\n", op.OldPath, confluenceBranch, err)
		}
		return nil
	case gitutil.ActionRenamed:
		// git mv old → new on the confluence branch, then rewrite the file
		// at the new path with updated front-matter and the latest body.
		if err := gitutil.Move(root, op.OldPath, op.NewPath); err != nil {
			return fmt.Errorf("git mv %s → %s: %w", op.OldPath, op.NewPath, err)
		}
		return writeManagedFile(root, op.NewPath, op.PageID, op.Version, op.HeadContent)
	}
	return fmt.Errorf("unknown action %q", op.Action)
}

// writeManagedFile writes a managed .md file with canonical front-matter
// (page_id, version) followed by a normalised copy of the body.
func writeManagedFile(root, path, pageID string, version int, body string) error {
	fm := lexer.FrontMatter{PageID: pageID, Version: version}
	content := lexer.ApplyFrontMatter(fm, lexer.Normalise(body))
	return writeLocalFile(root, path, content)
}

// stdinIsTerminal returns true if stdin is connected to a terminal (interactive),
// as opposed to a pipe (e.g. from a Git hook). Kept for the pull-side lock
// behaviour; unused by the new push.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
