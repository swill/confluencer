package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	cfgpkg "github.com/swill/confluencer/config"
	"github.com/swill/confluencer/gitutil"
)

// e2eRepo bundles up an initialised test repository with a mock Confluence
// server and helper methods, so individual scenario tests stay readable.
type e2eRepo struct {
	t      *testing.T
	dir    string
	mock   *mockConfluence
	config *cfgpkg.Config
}

// newE2ERepo seeds a temp git repo and a mock Confluence instance, writes
// the .confluencer.json that points at the mock, and points credential env
// vars at the same place. The repo is left clean and committed.
func newE2ERepo(t *testing.T, tree [][4]string) *e2eRepo {
	t.Helper()

	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
	// An initial commit so HEAD exists for EnsureBranchFromHead to seed
	// the confluence branch from.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "initial")

	mock := newMockConfluence("DOCS", tree)
	t.Cleanup(mock.Close)

	cfg := &cfgpkg.Config{
		RootPageID:     tree[0][0],
		SpaceKey:       "DOCS",
		LocalRoot:      "docs/",
		AttachmentsDir: "docs/_attachments",
	}
	if err := cfg.Save(filepath.Join(dir, configFile)); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFLUENCE_BASE_URL", mock.URL())
	t.Setenv("CONFLUENCE_USER", "tester")
	t.Setenv("CONFLUENCE_API_TOKEN", "secret")

	return &e2eRepo{t: t, dir: dir, mock: mock, config: cfg}
}

// cd changes the working directory (with t.Cleanup restoration) so
// repoRoot()/git operations target our test repo.
func (r *e2eRepo) cd() {
	r.t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chdir(r.dir); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = os.Chdir(orig) })
}

// runPullInRepo invokes runPull with the test repo as the cwd.
func (r *e2eRepo) runPullInRepo() error {
	r.cd()
	pullCmd.SetErr(&bytes.Buffer{})
	pullCmd.SetOut(&bytes.Buffer{})
	return runPull(pullCmd, nil)
}

// runPushInRepo invokes runPush with the test repo as the cwd.
func (r *e2eRepo) runPushInRepo() error {
	r.cd()
	pushCmd.SetErr(&bytes.Buffer{})
	pushCmd.SetOut(&bytes.Buffer{})
	return runPush(pushCmd, nil)
}

// readFile returns the contents of a repo-relative file as a string, failing
// the test if the file is missing.
func (r *e2eRepo) readFile(rel string) string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
	if err != nil {
		r.t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// gitStatus returns `git status --porcelain` for assertion purposes —
// "clean" tests check that this is empty.
func (r *e2eRepo) gitStatus() string {
	r.t.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = r.dir
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git status: %v", err)
	}
	return string(out)
}

// commit writes a file (creating directories as needed) and creates a commit.
func (r *e2eRepo) commit(rel, content, msg string) {
	r.t.Helper()
	full := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	run(r.t, r.dir, "git", "add", "-A")
	run(r.t, r.dir, "git", "commit", "-m", msg)
}

// TestE2E_PullFromEmptyConfluenceIsClean verifies that an initial pull
// against a tree with just a root page commits a single root index.md
// on the confluence branch and merges it cleanly into main.
func TestE2E_PullFromEmptyConfluenceIsClean(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root Page", "<p>root body</p>"},
	})

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	// docs/index.md should now exist and have front-matter.
	got := r.readFile("docs/index.md")
	if !strings.Contains(got, "confluence_page_id: \"100\"") {
		t.Errorf("docs/index.md missing page_id: %q", got)
	}
	if !strings.Contains(got, "confluence_version: 1") {
		t.Errorf("docs/index.md missing version: %q", got)
	}

	// Working tree should be clean (the merge brought the file into main).
	if status := r.gitStatus(); status != "" {
		t.Errorf("expected clean tree after pull, got:\n%s", status)
	}

	// Confluence branch should exist.
	exists, err := gitutil.BranchExists(r.dir, confluenceBranch)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("confluence branch should exist after first pull")
	}

	// Second pull is a no-op — nothing changed on Confluence.
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if status := r.gitStatus(); status != "" {
		t.Errorf("expected clean tree after no-op pull, got:\n%s", status)
	}
}

// TestE2E_PushNewFileNoConflict is the regression test for the original
// bug. The user reported: commit several new files → push → next commit
// triggers post-commit pull → conflict. With the new architecture this
// should be clean.
func TestE2E_PushNewFileNoConflict(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>root</p>"},
	})

	// Seed: pull pulls down the root page so we're in sync.
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	// User creates a new file under docs/ with no front-matter (the natural
	// thing — they wrote the file themselves).
	r.commit("docs/new-page.md", "# New Page\n\nSome content.\n", "add new-page")

	// Push: should create a Confluence page and advance the confluence branch.
	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Confirm the page exists on Confluence.
	pages := r.mock.AllPages()
	if len(pages) != 2 {
		t.Errorf("expected 2 pages on Confluence (root + new), got %d", len(pages))
	}

	// Working tree should be clean: push doesn't write to main.
	if status := r.gitStatus(); status != "" {
		t.Errorf("expected clean tree after push, got:\n%s", status)
	}

	// Now the bug scenario: another commit triggers post-commit pull.
	// With the old design, pull would see the new page on Confluence,
	// classify it as Created (because no index entry), and try to recreate
	// the file — colliding with the local copy.
	r.commit("docs/another.md", "# Another\n\nUnrelated.\n", "another commit")

	// Pull should be clean: confluence branch already has new-page.md from
	// the push's advance step, so pull sees no diff to apply.
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("post-commit pull (the bug scenario): %v", err)
	}

	// Working tree must still be clean — no merge conflict, no leftover state.
	if status := r.gitStatus(); status != "" {
		t.Errorf("BUG REGRESSION: expected clean tree after pull, got:\n%s", status)
	}
}

// TestE2E_PushModifyExisting walks through an edit-and-push cycle, verifying
// the version-increment + page-id-from-confluence-branch logic works end to
// end.
func TestE2E_PushModifyExisting(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>original</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	// User edits the root page locally — append a paragraph after the
	// existing body. Whatever the body normalises to, appending a fresh
	// line is guaranteed to produce a real diff.
	edited := r.readFile("docs/index.md") + "\nEdited locally.\n"
	r.commit("docs/index.md", edited, "edit root")

	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Confluence-side page should now be at version 2 with the new body.
	root := r.mock.PageByID("100")
	if root == nil {
		t.Fatal("root page disappeared")
	}
	if root.Version != 2 {
		t.Errorf("expected version 2, got %d", root.Version)
	}
	if !strings.Contains(root.Body, "Edited locally") {
		t.Errorf("body not updated: %q", root.Body)
	}
}

// TestE2E_PullPicksUpRemoteChange simulates someone editing a page directly
// in Confluence; pull should bring the change down and update front-matter.
func TestE2E_PullPicksUpRemoteChange(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>original</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("noop pull: %v", err)
	}

	// Mutate the page on Confluence directly (as if someone edited it via
	// the UI).
	r.mock.mu.Lock()
	r.mock.pages["100"].Body = "<p>edited remotely</p>"
	r.mock.pages["100"].Version = 5
	r.mock.mu.Unlock()

	// Need a local commit to fire the post-commit hook in the real flow,
	// but here we just call runPull directly.
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("pull: %v", err)
	}

	got := r.readFile("docs/index.md")
	if !strings.Contains(got, "edited remotely") {
		t.Errorf("body not synced down: %q", got)
	}
	if !strings.Contains(got, "confluence_version: 5") {
		t.Errorf("version not updated: %q", got)
	}
}

// TestE2E_DeleteOnConfluencePropagates verifies the pull side correctly
// removes a local file when the corresponding Confluence page is gone.
func TestE2E_DeleteOnConfluencePropagates(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>root</p>"},
		{"200", "100", "Child", "<p>child</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	// Confirm child was created locally.
	if _, err := os.Stat(filepath.Join(r.dir, "docs/child.md")); err != nil {
		t.Fatalf("child.md should exist: %v", err)
	}

	// Delete on Confluence side.
	r.mock.mu.Lock()
	delete(r.mock.pages, "200")
	r.mock.mu.Unlock()

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("pull after remote delete: %v", err)
	}

	if _, err := os.Stat(filepath.Join(r.dir, "docs/child.md")); !os.IsNotExist(err) {
		t.Errorf("child.md should be gone after pull: %v", err)
	}
	if status := r.gitStatus(); status != "" {
		t.Errorf("expected clean tree, got:\n%s", status)
	}
}
