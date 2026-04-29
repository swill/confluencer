package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/swill/confluencer/api"
	cfgpkg "github.com/swill/confluencer/config"
	"github.com/swill/confluencer/gitutil"
	"github.com/swill/confluencer/tree"
)

var pullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Sync Confluence state into the local confluence branch and merge it",
	Long: `Updates the local 'confluence' branch to mirror the current state of
Confluence (creating, updating, renaming, or deleting Markdown files as
needed), commits the result, and merges it into the current working branch.

Conflicts during the merge are left for you to resolve with your normal
git tools (git status, your editor, git merge --continue / --abort).`,
	RunE: runPull,
}

func init() {
	rootCmd.AddCommand(pullCmd)
}

func runPull(cmd *cobra.Command, args []string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	out := cmd.ErrOrStderr()

	gitDir, err := gitutil.GitDir(root)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(gitDir, "confluencer-pull.lock")
	lock, err := acquireLock(lockPath)
	if err != nil {
		if stdinIsTerminal() {
			removeStaleLock(lockPath)
			lock, err = acquireLock(lockPath)
			if err != nil {
				return fmt.Errorf("acquire pull lock: %w", err)
			}
		} else {
			// Another pull is in progress (e.g. fired by a sibling hook).
			// The holder will do the work; exit silently.
			return nil
		}
	}
	defer releaseLock(lock, lockPath)

	cfg, err := cfgpkg.LoadConfig(filepath.Join(root, configFile))
	if err != nil {
		return err
	}
	creds, err := cfgpkg.LoadCredentials(root)
	if err != nil {
		return err
	}

	origBranch, err := gitutil.CurrentBranch(root)
	if err != nil {
		return fmt.Errorf("get current branch: %w", err)
	}
	if origBranch == "" {
		return fmt.Errorf("cannot pull in detached HEAD state")
	}
	if origBranch == confluenceBranch {
		return fmt.Errorf("currently on the %q branch — switch to your work branch before running pull", confluenceBranch)
	}

	// Refuse to operate with a dirty tree on the working branch — tracked
	// changes might collide with the merge from confluence. Untracked files
	// are fine: git carries them across checkouts as long as they don't
	// shadow tracked files on the destination branch.
	if clean, err := gitutil.IsClean(root); err != nil {
		return err
	} else if !clean {
		return fmt.Errorf("working tree has uncommitted changes — commit or stash them before running pull")
	}

	// Seed the confluence branch on first run. After this it persists.
	if err := gitutil.EnsureBranchFromHead(root, confluenceBranch); err != nil {
		return fmt.Errorf("ensure %s branch: %w", confluenceBranch, err)
	}

	fmt.Fprintf(out, "[confluencer] fetching page tree from Confluence...\n")

	client := api.NewClient(creds.BaseURL, creds.User, creds.APIToken)

	ct, err := client.FetchTree(cfg.RootPageID, false)
	if err != nil {
		return fmt.Errorf("fetch tree: %w", err)
	}
	pm := tree.ComputePaths(ct, cfg.LocalRoot)

	// Switch to the confluence branch to apply Confluence's state to it.
	if err := gitutil.Checkout(root, confluenceBranch); err != nil {
		return fmt.Errorf("checkout %s: %w", confluenceBranch, err)
	}
	// We have to make sure we always end up back on origBranch.
	switchedBack := false
	defer func() {
		if !switchedBack {
			_ = gitutil.Checkout(root, origBranch)
		}
	}()

	// Build the current state of the confluence branch by reading front-matter
	// out of every managed file in localRoot.
	currentFiles, err := scanManagedFiles(root, cfg.LocalRoot)
	if err != nil {
		return fmt.Errorf("scan managed files: %w", err)
	}
	currentByID := make(map[string]localManagedFile, len(currentFiles))
	for _, f := range currentFiles {
		currentByID[f.PageID] = f
	}

	// Plan the work.
	plan := planPull(ct, pm, currentByID)

	// Confirm deletes via direct GET (a page missing from a tree fetch could
	// be a transient API issue, an out-of-scope move, or a real delete).
	plan.Deletes, plan.Orphaned, plan.Unknown = classifyMissing(client, plan.Deletes)

	for _, o := range plan.Orphaned {
		fmt.Fprintf(out, "[confluencer] WARNING: page %s (%s) moved outside sync scope — leaving local file in place\n", o.PageID, o.Path)
	}
	for _, u := range plan.Unknown {
		fmt.Fprintf(out, "[confluencer] WARNING: page %s (%s) status indeterminate — skipping this run\n", u.PageID, u.Path)
	}

	if plan.IsNoOp() {
		fmt.Fprintf(out, "[confluencer] no changes from Confluence\n")
		switchedBack = true
		return gitutil.Checkout(root, origBranch)
	}

	// Execute the plan on the confluence branch.
	if err := executePullPlan(client, root, creds.BaseURL, cfg, ct, pm, plan, out); err != nil {
		return err
	}

	// Commit on the confluence branch. If nothing actually changed (e.g.
	// every page on Confluence is already byte-identical to its local form),
	// CommitAllOnHead returns "" and we skip the merge.
	commitMsg := gitutil.SyncPrefix + " @ " + time.Now().UTC().Format(time.RFC3339)
	sha, err := gitutil.CommitAllOnHead(root, commitMsg)
	if err != nil {
		return fmt.Errorf("commit on %s: %w", confluenceBranch, err)
	}

	// Switch back to the working branch.
	if err := gitutil.Checkout(root, origBranch); err != nil {
		return fmt.Errorf("checkout %s: %w", origBranch, err)
	}
	switchedBack = true

	if sha == "" {
		fmt.Fprintf(out, "[confluencer] no changes from Confluence\n")
		return nil
	}

	// Merge the confluence branch into the working branch. Conflicts are
	// the user's to resolve; we just surface them clearly.
	conflict, err := gitutil.MergeFrom(root, confluenceBranch)
	if err != nil {
		return fmt.Errorf("merge %s into %s: %w", confluenceBranch, origBranch, err)
	}
	if conflict {
		fmt.Fprintf(out, "[confluencer] CONFLICT: merge from %s has conflicts.\n", confluenceBranch)
		fmt.Fprintf(out, "[confluencer]   Resolve with your editor and `git merge --continue`,\n")
		fmt.Fprintf(out, "[confluencer]   or abort with `git merge --abort`.\n")
		return nil
	}

	fmt.Fprintf(out, "[confluencer] done — synced %d page(s) from Confluence\n", plan.ActionCount())
	return nil
}

// pullPlan is the structured description of what pull intends to do, computed
// before any side effects so we can short-circuit on no-op runs and report
// summaries cleanly.
type pullPlan struct {
	// Renames are pages that exist on both sides but at different paths.
	// The page's content might also need updating; PendingWrites will pick
	// that up by version comparison.
	Renames []renameOp

	// Deletes are pages on the confluence branch whose IDs are absent from
	// the freshly-fetched tree. Until classifyMissing runs, this list is
	// "candidates"; afterwards it's confirmed-deleted-on-Confluence.
	Deletes []localManagedFile

	// Orphaned are pages that exist on Confluence but moved outside our
	// sync scope; we leave the local file alone.
	Orphaned []localManagedFile

	// Unknown are pages whose status couldn't be confirmed (network/5xx).
	Unknown []localManagedFile

	// PendingWrites are pages we need to fetch the body for and write —
	// covers both creates (no local copy yet) and updates (version differs).
	PendingWrites []pendingWrite
}

type renameOp struct {
	PageID string
	From   string
	To     string
}

type pendingWrite struct {
	PageID     string
	TargetPath string
	Node       *tree.CfNode
}

func (p pullPlan) IsNoOp() bool {
	return len(p.Renames) == 0 && len(p.Deletes) == 0 && len(p.PendingWrites) == 0
}

func (p pullPlan) ActionCount() int {
	return len(p.Renames) + len(p.Deletes) + len(p.PendingWrites)
}

// planPull diffs the freshly-fetched Confluence tree against the
// confluence-branch local state and produces an action plan.
func planPull(ct *tree.CfTree, pm *tree.PathMap, currentByID map[string]localManagedFile) pullPlan {
	var plan pullPlan

	// Pages present on Confluence: classify into rename/update/create.
	ct.Walk(func(node *tree.CfNode) {
		targetPath, ok := pm.Path(node.PageID)
		if !ok {
			return
		}
		cur, found := currentByID[node.PageID]
		if !found {
			plan.PendingWrites = append(plan.PendingWrites, pendingWrite{
				PageID: node.PageID, TargetPath: targetPath, Node: node,
			})
			return
		}
		if cur.Path != targetPath {
			plan.Renames = append(plan.Renames, renameOp{
				PageID: node.PageID, From: cur.Path, To: targetPath,
			})
		}
		if cur.Version != node.Version {
			plan.PendingWrites = append(plan.PendingWrites, pendingWrite{
				PageID: node.PageID, TargetPath: targetPath, Node: node,
			})
		}
	})

	// Pages on the confluence branch but not in the tree → delete candidates.
	for _, cur := range currentFilesSorted(currentByID) {
		if !ct.Contains(cur.PageID) {
			plan.Deletes = append(plan.Deletes, cur)
		}
	}
	return plan
}

// currentFilesSorted returns the values of currentByID sorted by path so that
// plan output (and any commit-message ordering) is deterministic.
func currentFilesSorted(currentByID map[string]localManagedFile) []localManagedFile {
	out := make([]localManagedFile, 0, len(currentByID))
	for _, f := range currentByID {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// classifyMissing partitions delete-candidates into truly-deleted (404),
// orphaned (still exists but moved out of sync scope), and unknown (network /
// 5xx — we can't tell, so we leave them alone this run).
func classifyMissing(client *api.Client, candidates []localManagedFile) (deleted, orphaned, unknown []localManagedFile) {
	for _, c := range candidates {
		_, err := client.GetPage(c.PageID)
		switch {
		case err == nil:
			orphaned = append(orphaned, c)
		case api.IsNotFound(err):
			deleted = append(deleted, c)
		default:
			unknown = append(unknown, c)
		}
	}
	return
}

// executePullPlan applies the plan to the working tree (already on the
// confluence branch). Renames are applied first using a two-phase staging
// protocol whenever any rename's destination is another rename's source;
// then deletes; then writes.
func executePullPlan(client *api.Client, root, baseURL string, cfg *cfgpkg.Config, ct *tree.CfTree, pm *tree.PathMap, plan pullPlan, out io.Writer) error {
	// Two-phase rename: if any rename's destination is another rename's
	// source, route everyone through a staging dir to avoid intermediate
	// path collisions. Otherwise direct git mv is fine.
	if len(plan.Renames) > 0 {
		if err := applyRenames(root, cfg, plan.Renames, out); err != nil {
			return err
		}
	}

	// Deletes (file + per-page attachments dir).
	for _, d := range plan.Deletes {
		fmt.Fprintf(out, "[confluencer] delete: %s\n", d.Path)
		if err := gitutil.Remove(root, d.Path); err != nil {
			fmt.Fprintf(out, "[confluencer] WARNING: git rm %s: %v\n", d.Path, err)
		}
		attDir := tree.AttachmentDir(d.Path, cfg.LocalRoot, cfg.AttachmentsDir)
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(attDir))); err == nil && info.IsDir() {
			_ = gitutil.Remove(root, attDir)
		}
	}

	// Pending writes (creates and updates). Process in path order so commit
	// output is deterministic and parents land before children for any
	// downstream tooling that reads in walk order.
	sort.Slice(plan.PendingWrites, func(i, j int) bool {
		return plan.PendingWrites[i].TargetPath < plan.PendingWrites[j].TargetPath
	})
	for _, pw := range plan.PendingWrites {
		page, err := client.GetPage(pw.PageID)
		if err != nil {
			fmt.Fprintf(out, "[confluencer] WARNING: fetch page %s: %v\n", pw.PageID, err)
			continue
		}
		opts := resolverForPage(pw.TargetPath, baseURL, cfg, ct, pm)
		content, err := renderPage(pw.PageID, page.Body, page.Version, opts)
		if err != nil {
			fmt.Fprintf(out, "[confluencer] WARNING: %v\n", err)
			continue
		}
		if err := writeLocalFile(root, pw.TargetPath, content); err != nil {
			fmt.Fprintf(out, "[confluencer] ERROR: write %s: %v\n", pw.TargetPath, err)
			continue
		}
		// Verb depends on whether this was a create or an update.
		verb := "update"
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(pw.TargetPath))); statErr == nil && pw.Node != nil {
			// Both create and update succeed past the write; we infer create
			// when the page wasn't tracked under the same path before. We've
			// already lost that information here, so just print "write" for
			// either case to keep output unambiguous.
			verb = "write"
		}
		fmt.Fprintf(out, "[confluencer] %s: %s\n", verb, pw.TargetPath)

		downloadPageAttachments(client, root, cfg, pw.PageID, pw.TargetPath, out)
	}
	return nil
}

// applyRenames executes all rename operations using a two-phase staging
// strategy when needed, plus moving each page's per-page attachment
// subdirectory alongside its .md file.
func applyRenames(root string, cfg *cfgpkg.Config, renames []renameOp, out io.Writer) error {
	if hasCollisions(renames) {
		stagingDir := filepath.Join(cfg.LocalRoot, ".confluencer-staging")
		stagingAbs := filepath.Join(root, filepath.FromSlash(stagingDir))
		_ = os.MkdirAll(stagingAbs, 0o755)
		// Phase 1: move all sources into staging under stable names.
		stageMap := make(map[string]string, len(renames))
		for i, r := range renames {
			stagedPath := filepath.ToSlash(filepath.Join(stagingDir, fmt.Sprintf("%d.md", i)))
			if err := gitutil.Move(root, r.From, stagedPath); err != nil {
				return fmt.Errorf("stage rename %s: %w", r.From, err)
			}
			stageMap[r.PageID] = stagedPath
		}
		// Phase 2: move from staging to final destinations.
		for _, r := range renames {
			fmt.Fprintf(out, "[confluencer] rename: %s → %s\n", r.From, r.To)
			if err := gitutil.Move(root, stageMap[r.PageID], r.To); err != nil {
				return fmt.Errorf("place rename %s → %s: %w", r.From, r.To, err)
			}
			renamePageAttachments(root, cfg, r, out)
		}
		_ = os.RemoveAll(stagingAbs)
		return nil
	}
	for _, r := range renames {
		fmt.Fprintf(out, "[confluencer] rename: %s → %s\n", r.From, r.To)
		if err := gitutil.Move(root, r.From, r.To); err != nil {
			return fmt.Errorf("rename %s → %s: %w", r.From, r.To, err)
		}
		renamePageAttachments(root, cfg, r, out)
	}
	return nil
}

// hasCollisions returns true if any rename's target path equals another
// rename's source path — which would make a direct `git mv` of the second
// rename fail because the source has already been overwritten.
func hasCollisions(renames []renameOp) bool {
	sources := make(map[string]struct{}, len(renames))
	for _, r := range renames {
		sources[r.From] = struct{}{}
	}
	for _, r := range renames {
		if _, ok := sources[r.To]; ok {
			return true
		}
	}
	return false
}

// renamePageAttachments moves a page's per-page attachment subdirectory to
// match its new path. Best effort — failures are logged but don't abort the
// rename of the .md file itself.
func renamePageAttachments(root string, cfg *cfgpkg.Config, r renameOp, out io.Writer) {
	oldDir := tree.AttachmentDir(r.From, cfg.LocalRoot, cfg.AttachmentsDir)
	newDir := tree.AttachmentDir(r.To, cfg.LocalRoot, cfg.AttachmentsDir)
	if oldDir == newDir {
		return
	}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(oldDir))); err != nil || !info.IsDir() {
		return
	}
	if err := gitutil.Move(root, oldDir, newDir); err != nil {
		fmt.Fprintf(out, "[confluencer] WARNING: move attachments %s → %s: %v\n", oldDir, newDir, err)
	}
}

// writeLocalFile writes content to a repo-relative path, creating directories.
func writeLocalFile(root, repoPath, content string) error {
	absPath := filepath.Join(root, filepath.FromSlash(repoPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(absPath, []byte(content), 0o644)
}

// downloadPageAttachments downloads all attachments for a page into the
// per-page attachment subdirectory.
func downloadPageAttachments(client *api.Client, root string, cfg *cfgpkg.Config, pageID, localPath string, out io.Writer) {
	atts, err := client.GetAttachments(pageID, "")
	if err != nil {
		fmt.Fprintf(out, "[confluencer] WARNING: list attachments for %s: %v\n", pageID, err)
		return
	}
	attDir := tree.AttachmentDir(localPath, cfg.LocalRoot, cfg.AttachmentsDir)
	for _, att := range atts {
		data, err := client.DownloadAttachment(att.DownloadPath)
		if err != nil {
			fmt.Fprintf(out, "[confluencer] WARNING: download %s: %v\n", att.Filename, err)
			continue
		}
		attPath := filepath.Join(root, filepath.FromSlash(attDir), att.Filename)
		_ = os.MkdirAll(filepath.Dir(attPath), 0o755)
		_ = os.WriteFile(attPath, data, 0o644)
	}
}

// File-lock helpers — unchanged from the previous implementation. The lock
// lives under .git/ so it never appears in `git status` output.

func acquireLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

func releaseLock(f *os.File, path string) {
	_ = f.Close()
	_ = os.Remove(path)
}

func removeStaleLock(path string) {
	_ = os.Remove(path)
}
