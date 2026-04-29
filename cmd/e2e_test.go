package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	cfgpkg "github.com/swill/gfl/config"
	"github.com/swill/gfl/gitutil"
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
// the .gfl.json that points at the mock, and points credential env
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

// gitLog returns `git log --pretty=oneline -n N` so tests can assert the
// shape of recent history (e.g. "push produced a chore commit, not a merge").
func (r *e2eRepo) gitLog(n int) string {
	r.t.Helper()
	cmd := exec.Command("git", "log", "--pretty=%P %s", fmt.Sprintf("-n%d", n))
	cmd.Dir = r.dir
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git log: %v", err)
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

// writeBinary writes a binary file under the repo (creating directories) but
// does not commit. Useful for staging an attachment that a subsequent
// `commit` call (of a referencing .md) will sweep up.
func (r *e2eRepo) writeBinary(rel string, data []byte) {
	r.t.Helper()
	full := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		r.t.Fatal(err)
	}
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

	// Working tree should be clean: push commits its post-push merge into
	// main automatically, so there's nothing for the user to stage or fix up.
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

// TestE2E_PushProducesLinearHistory locks in that push does not introduce a
// merge commit on the working branch — the chore commit lands directly on
// main and confluence fast-forwards to match. The user reported the
// previous "chore + Merge branch 'confluence'" pair as noise; this test
// guards against regressing back to that shape.
func TestE2E_PushProducesLinearHistory(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>root body</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	r.commit("docs/new-page.md", "# New Page\n\nSome content.\n", "add new-page")
	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("push: %v", err)
	}

	logTop := r.gitLog(1)
	// "%P %s" prints parent-shas (space separated) then subject. A merge
	// commit has two parents → two SHAs before the subject.
	parents := strings.Fields(strings.SplitN(logTop, " ", 2)[0])
	if len(parents) != 1 {
		t.Errorf("expected linear history (single-parent commit at HEAD), got %d parents: %q", len(parents), logTop)
	}
	if !strings.Contains(logTop, "chore(sync): confluence-push") {
		t.Errorf("expected HEAD to be the push chore commit, got: %q", logTop)
	}

	// confluence ref should now point at HEAD.
	headSha, err := gitutil.HeadSHA(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	confluenceSha, err := gitutil.HeadSHA(r.dir) // placeholder; we'll use show-ref below
	_ = confluenceSha
	out, err := exec.Command("git", "-C", r.dir, "rev-parse", confluenceBranch).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", confluenceBranch, err)
	}
	if got := strings.TrimSpace(string(out)); got != headSha {
		t.Errorf("expected %s ref == HEAD (%s), got %s", confluenceBranch, headSha, got)
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

// TestE2E_EditCycleNoSpuriousConflict reproduces the failure mode where a
// second edit-on-both-sides cycle conflicts on a line that *only the
// confluence side* edited in cycle 2. The pre-fix bug: push advanced the
// confluence branch but didn't merge that advance into the working branch,
// so cycle 2's pull used cycle 1's *pre-push* confluence commit as the
// merge base — making cycle 1's user-side edit look like a fresh main-side
// change relative to base, even though Confluence already canonically
// holds it. Cycle 2's confluence-side edit on the same line then collides
// with that ghost.
//
// Sequence:
//
//   - Cycle 1: line one edited externally on Confluence; line two edited
//     locally → commit → pull (clean merge — disjoint changes) → push.
//   - Cycle 2: line two edited externally on Confluence (overwriting the
//     line the user just pushed); line three edited locally → commit →
//     pull. Disjoint changes from the post-push state, so this must be
//     clean. With a stale merge base it conflicts on line two.
func TestE2E_EditCycleNoSpuriousConflict(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>line one</p><p>line two</p><p>line three</p>"},
	})

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	// Cycle 1 confluence-side edit.
	r.mock.mu.Lock()
	r.mock.pages["100"].Body = "<p>line one external</p><p>line two</p><p>line three</p>"
	r.mock.pages["100"].Version = 2
	r.mock.mu.Unlock()

	// Cycle 1 local edit to a different line, then pull (which merges
	// confluence's external change in) and push (which sends the local
	// edit upstream).
	c1 := r.readFile("docs/index.md")
	c1edit := strings.Replace(c1, "line two", "line two local", 1)
	if c1edit == c1 {
		t.Fatalf("cycle 1: expected line-two substitution to apply: %q", c1)
	}
	r.commit("docs/index.md", c1edit, "edit line two")

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("cycle 1 pull: %v", err)
	}
	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("cycle 1 push: %v", err)
	}
	if status := r.gitStatus(); status != "" {
		t.Fatalf("expected clean tree after cycle 1 push, got:\n%s", status)
	}

	// Cycle 2 confluence-side edit hits line two — the line the user just
	// pushed up. Pre-fix, line two is the conflict surface in the next
	// pull because the stale merge base still has its original value.
	r.mock.mu.Lock()
	r.mock.pages["100"].Body = "<p>line one external</p><p>line two external</p><p>line three</p>"
	r.mock.pages["100"].Version = 4
	r.mock.mu.Unlock()

	// Cycle 2 local edit to line three (a line nothing on the confluence
	// side has touched this cycle).
	c2 := r.readFile("docs/index.md")
	c2edit := strings.Replace(c2, "line three", "line three local", 1)
	if c2edit == c2 {
		t.Fatalf("cycle 2: expected line-three substitution to apply: %q", c2)
	}
	r.commit("docs/index.md", c2edit, "edit line three")

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("cycle 2 pull: %v", err)
	}
	if status := r.gitStatus(); status != "" {
		t.Errorf("BUG REGRESSION: expected clean tree after cycle 2 pull, got:\n%s", status)
	}

	got := r.readFile("docs/index.md")
	for _, want := range []string{"line one external", "line two external", "line three local"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged file missing %q:\n%s", want, got)
		}
	}
}

// TestE2E_PushBodyMatchesPullCanonicalForm reproduces the second failure
// mode the user hit: even with the post-push merge in place, push's
// confluence-branch body and the next pull's confluence-branch body for
// the same Confluence content can diverge — because CfToMd is not a
// fixed point on certain inputs (e.g., HTML whitespace tokenisation
// collapses runs of spaces).
//
// Concretely: a body line "second line.  edit from markdown" survives
// Normalise byte-for-byte but loses one space after a CfToMd round-trip.
// Pre-fix, push wrote the un-roundtripped form to the confluence branch,
// so the next pull's commit (which goes through CfToMd) appeared to have
// edited that line on the confluence side — colliding with any
// concurrent main-side edit to the same line, even when the only "real"
// change in this cycle was on a different line.
func TestE2E_PushBodyMatchesPullCanonicalForm(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>first line.</p><p>second line.</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	// Cycle 1 user edit: introduces a doubled-space run on line two.
	// (A user typing in their editor — the fact that the canonical pull
	// form produces single spaces is irrelevant to what they commit.)
	body := r.readFile("docs/index.md")
	c1 := strings.Replace(body, "second line.", "second line.  edit from markdown.", 1)
	if c1 == body {
		t.Fatalf("cycle 1: expected line-two substitution to apply: %q", body)
	}
	r.commit("docs/index.md", c1, "cycle 1 markdown edit")

	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("cycle 1 push: %v", err)
	}

	// Cycle 2: a different line is edited on Confluence (so there's a
	// real change to pull). Line two — the doubled-space line — is left
	// untouched on the Confluence side.
	r.mock.mu.Lock()
	r.mock.pages["100"].Body = "<p>first line. edit from confluence.</p><p>second line.  edit from markdown.</p>"
	r.mock.pages["100"].Version = r.mock.pages["100"].Version + 1
	r.mock.mu.Unlock()

	// Cycle 2 user edit: re-touches line two with a different wording,
	// preserving the doubled-space run. Pre-fix, the merge base's line
	// two has doubled spaces but the cycle 2 confluence commit has
	// single spaces (CfToMd collapsed them) — both sides "modified"
	// line two and git produces a conflict.
	cur := r.readFile("docs/index.md")
	c2 := strings.Replace(cur, "edit from markdown.", "second edit from markdown.", 1)
	if c2 == cur {
		t.Fatalf("cycle 2: expected line-two substitution to apply: %q", cur)
	}
	r.commit("docs/index.md", c2, "cycle 2 markdown edit")

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("cycle 2 pull: %v", err)
	}
	if status := r.gitStatus(); status != "" {
		t.Errorf("BUG REGRESSION: expected clean tree after cycle 2 pull, got:\n%s", status)
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

// TestE2E_PushNewPageWithAttachment locks in the full image-handling fix:
// a Markdown image referencing a file under _attachments must
//
//   - convert to <ri:attachment> in the storage XML sent to Confluence (not
//     a broken <ri:url> pointing at a relative filesystem path), and
//   - upload the binary file alongside the page write so the
//     <ri:attachment> reference resolves on the Confluence side.
//
// Pre-fix, both halves were missing — the image link silently shipped as an
// external URL and the binary never left the developer's machine.
func TestE2E_PushNewPageWithAttachment(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>root</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	// User stages a binary attachment plus a Markdown file that references
	// it via the relative-path form.
	imgBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'd', 'a', 't', 'a'}
	r.writeBinary("docs/_attachments/diagram-page/schema.png", imgBytes)
	mdBody := "# Diagram Page\n\nSee:\n\n![schema](_attachments/diagram-page/schema.png)\n"
	r.commit("docs/diagram-page.md", mdBody, "add diagram-page with attachment")

	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Find the page that just got created.
	var newPageID string
	for id, p := range r.mock.AllPages() {
		if p.Title == "Diagram Page" {
			newPageID = id
			break
		}
	}
	if newPageID == "" {
		t.Fatalf("page not created; pages: %+v", r.mock.AllPages())
	}

	// The storage XML must reference the attachment, not a URL.
	page := r.mock.PageByID(newPageID)
	if !strings.Contains(page.Body, `<ri:attachment ri:filename="schema.png"`) {
		t.Errorf("body should contain <ri:attachment>, got: %s", page.Body)
	}
	if strings.Contains(page.Body, `<ri:url`) {
		t.Errorf("body should not contain <ri:url> for an attachment-backed image, got: %s", page.Body)
	}

	// The binary must have been uploaded.
	atts := r.mock.AttachmentsForPage(newPageID)
	got, ok := atts["schema.png"]
	if !ok {
		t.Fatalf("schema.png not uploaded; uploaded files: %v", keysOf(atts))
	}
	if string(got) != string(imgBytes) {
		t.Errorf("uploaded bytes differ from disk:\n  want %v\n  got  %v", imgBytes, got)
	}
}

// TestE2E_PushUpdateUploadsAttachment confirms uploads happen on the
// modify path too, not just on adds. A second push that re-edits the
// markdown body re-uploads the attachment (Confluence treats it as a new
// version). This is the "user changed an image, push it" loop.
func TestE2E_PushUpdateUploadsAttachment(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>root</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	// Initial push with image v1.
	v1 := []byte("image-v1")
	r.writeBinary("docs/_attachments/page-with-image/img.png", v1)
	r.commit("docs/page-with-image.md",
		"# Page With Image\n\n![alt](_attachments/page-with-image/img.png)\n",
		"add page with image v1")
	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("first push: %v", err)
	}

	var pageID string
	for id, p := range r.mock.AllPages() {
		if p.Title == "Page With Image" {
			pageID = id
			break
		}
	}
	if pageID == "" {
		t.Fatal("page not found after first push")
	}
	if string(r.mock.AttachmentsForPage(pageID)["img.png"]) != "image-v1" {
		t.Errorf("v1 not uploaded")
	}

	// User replaces the image content (same filename) and tweaks the body.
	v2 := []byte("image-v2-different")
	r.writeBinary("docs/_attachments/page-with-image/img.png", v2)
	cur := r.readFile("docs/page-with-image.md")
	r.commit("docs/page-with-image.md",
		strings.Replace(cur, "Page With Image", "Page With Image\n\nUpdated.", 1),
		"update body and image")
	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("second push: %v", err)
	}

	if string(r.mock.AttachmentsForPage(pageID)["img.png"]) != "image-v2-different" {
		t.Errorf("v2 not uploaded; got: %q", r.mock.AttachmentsForPage(pageID)["img.png"])
	}
}

// TestE2E_PushExternalImageNotUploaded asserts that an external image URL
// (one that's not an attachment) does not trigger an upload attempt and
// stays as <ri:url> in storage.
func TestE2E_PushExternalImageNotUploaded(t *testing.T) {
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>root</p>"},
	})
	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("seed pull: %v", err)
	}

	r.commit("docs/external.md",
		"# External\n\n![logo](https://example.com/logo.png)\n",
		"add page with external image")
	if err := r.runPushInRepo(); err != nil {
		t.Fatalf("push: %v", err)
	}

	var pageID string
	for id, p := range r.mock.AllPages() {
		if p.Title == "External" {
			pageID = id
			break
		}
	}
	if pageID == "" {
		t.Fatal("page not found after push")
	}

	page := r.mock.PageByID(pageID)
	if !strings.Contains(page.Body, `<ri:url ri:value="https://example.com/logo.png"`) {
		t.Errorf("external URL should round-trip as <ri:url>, got: %s", page.Body)
	}
	if len(r.mock.AttachmentsForPage(pageID)) != 0 {
		t.Errorf("no attachments should be uploaded for external image; got: %v",
			keysOf(r.mock.AttachmentsForPage(pageID)))
	}
}

// TestE2E_PullStyledBlockImageRenders is the regression test for the
// original user-reported symptom: a Confluence page with a block-level
// <ac:image> carrying styling attributes was pulling down as a fenced
// base64 blob (treated as an unsupported construct) instead of a Markdown
// image. With Phase 1's fix, the same input must render cleanly.
func TestE2E_PullStyledBlockImageRenders(t *testing.T) {
	// Confluence editor's typical output for a sized/aligned image: an
	// <ac:image> at the block level with a battery of ac:* attributes.
	const styledImage = `<ac:image ac:align="center" ac:alt="aptum_offerings.png" ac:custom-width="true" ac:layout="center" ac:original-height="664" ac:original-width="1291" ac:width="1006"><ri:attachment ri:filename="aptum_offerings.png" ri:version-at-save="1"/></ac:image>`
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>intro</p>" + styledImage + "<p>outro</p>"},
	})

	// Seed the attachment so pull's attachment download has something to
	// pull (otherwise the binary side is silently empty — fine, but less
	// realistic).
	r.mock.mu.Lock()
	r.mock.attachments["100"] = map[string][]byte{"aptum_offerings.png": []byte("png-bytes")}
	r.mock.mu.Unlock()

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("pull: %v", err)
	}

	got := r.readFile("docs/index.md")
	// Must contain a Markdown image reference, not a fence-encoded blob.
	// The path is relative to the source file's directory, not repo-rooted
	// — index.md lives in docs/, so the local-root prefix doesn't appear.
	if !strings.Contains(got, "![aptum_offerings.png](_attachments/aptum_offerings.png)") {
		t.Errorf("expected source-relative image reference, got:\n%s", got)
	}
	if strings.Contains(got, "](docs/_attachments/") {
		t.Errorf("BUG REGRESSION: image src is repo-rooted (won't render in viewers):\n%s", got)
	}
	if strings.Contains(got, "gfl:storage:block:v1:b64") {
		t.Errorf("BUG REGRESSION: block-level image was fence-encoded:\n%s", got)
	}
}

// TestE2E_PullNestedPageImageHasParentRelativePath asserts the path format
// for an image on a non-root page: a leading "../" is required so the path
// resolves correctly when the file is viewed from its own directory.
func TestE2E_PullNestedPageImageHasParentRelativePath(t *testing.T) {
	const styledImage = `<ac:image><ri:attachment ri:filename="schema.png"/></ac:image>`
	r := newE2ERepo(t, [][4]string{
		{"100", "", "Root", "<p>root</p>"},
		{"200", "100", "Sample", "<p>" + styledImage + "</p>"},
	})

	r.mock.mu.Lock()
	r.mock.attachments["200"] = map[string][]byte{"schema.png": []byte("png")}
	r.mock.mu.Unlock()

	if err := r.runPullInRepo(); err != nil {
		t.Fatalf("pull: %v", err)
	}

	got := r.readFile("docs/sample.md")
	// Sample lives at docs/sample.md (flat). Its attachments live at
	// docs/_attachments/sample/. Source dir = docs. So the relative path
	// from docs to docs/_attachments/sample/schema.png is just
	// _attachments/sample/schema.png.
	if !strings.Contains(got, "![schema](_attachments/sample/schema.png)") {
		t.Errorf("expected _attachments/sample/schema.png, got:\n%s", got)
	}
}

// keysOf returns the keys of a map[string][]byte sorted, for stable error
// messages in test failures.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
