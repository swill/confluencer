package cmd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	cfgpkg "github.com/swill/confluencer/config"
	"github.com/swill/confluencer/lexer"
	"github.com/swill/confluencer/tree"
)

// confluenceBranch is the name of the persistent local branch that mirrors
// the Confluence-side state. Pull writes its sync commits here; push diffs
// the working branch against this ref to compute outstanding work.
const confluenceBranch = "confluence"

// renderPage converts a Confluence storage-XML body to the canonical local
// Markdown form, with confluence_page_id and confluence_version pinned in a
// front-matter block at the top. This is the single source of truth for what
// a managed file looks like — used by both `confluencer init` (initial seed)
// and `confluencer pull` (subsequent updates) so the two paths produce
// byte-identical output.
func renderPage(pageID, body string, version int, opts lexer.CfToMdOpts) (string, error) {
	mdBody, err := lexer.CfToMd(body, opts)
	if err != nil {
		return "", fmt.Errorf("convert page %s: %w", pageID, err)
	}
	fm := lexer.FrontMatter{PageID: pageID, Version: version}
	return lexer.ApplyFrontMatter(fm, lexer.Normalise(mdBody)), nil
}

// localManagedFile is one .md file on disk that confluencer recognises as a
// Confluence-mirror file (it has front-matter with a confluence_page_id).
type localManagedFile struct {
	Path    string // repo-relative, slash-separated
	PageID  string
	Version int
}

// scanManagedFiles walks `localRoot` and returns every .md file that has
// front-matter naming a confluence_page_id. Files without front-matter or
// with malformed front-matter are skipped silently — they may belong to the
// user, not to confluencer.
func scanManagedFiles(repoRoot, localRoot string) ([]localManagedFile, error) {
	rootAbs := filepath.Join(repoRoot, filepath.FromSlash(localRoot))
	if _, err := os.Stat(rootAbs); os.IsNotExist(err) {
		return nil, nil
	}
	var out []localManagedFile
	err := filepath.WalkDir(rootAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip the attachments directory and any hidden directory.
			name := d.Name()
			if strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fm, _, fmErr := lexer.ExtractFrontMatter(string(data))
		if fmErr != nil || fm.PageID == "" {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return err
		}
		out = append(out, localManagedFile{
			Path:    filepath.ToSlash(rel),
			PageID:  fm.PageID,
			Version: fm.Version,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolverForPage builds the cf_to_md resolver pair for a given local path.
// Cross-page links are resolved against the supplied PathMap; image attachment
// references resolve to the per-page _attachments subdirectory. baseURL is
// the Confluence wiki base (from credentials) so cf_to_md can build a working
// URL when an ac:link points at a page outside the synced tree — without
// it the URL would be silently dropped on conversion back to Markdown.
func resolverForPage(localPath, baseURL string, cfg *cfgpkg.Config, ct *tree.CfTree, pm *tree.PathMap) lexer.CfToMdOpts {
	return lexer.CfToMdOpts{
		Pages: &treePageResolver{tree: ct, paths: pm},
		Attachments: &stubAttachmentResolver{
			localPath:      localPath,
			attachmentsDir: cfg.AttachmentsDir,
			localRoot:      cfg.LocalRoot,
		},
		BaseURL: baseURL,
	}
}

// treePageResolver implements lexer.PageResolver against an in-memory CfTree.
// Pull and init both use this; previously each had its own copy.
type treePageResolver struct {
	tree  *tree.CfTree
	paths *tree.PathMap
}

// ResolvePageByID returns the local path for pageID iff that page is part of
// the in-memory tree (i.e. inside the configured root's subtree). This is
// the authoritative "is this link to a tracked page" check — page IDs are
// unique, so a hit here is never a false positive.
func (r *treePageResolver) ResolvePageByID(pageID string) (string, bool) {
	if !r.tree.Contains(pageID) {
		return "", false
	}
	return r.paths.Path(pageID)
}

// ResolvePageByTitle is the legacy fallback used only when storage XML lacks
// ri:content-id. Title matching is loose (a page outside the tree with the
// same title will silently match), so it must never be the primary resolver.
func (r *treePageResolver) ResolvePageByTitle(title, spaceKey string) (string, bool) {
	var found *tree.CfNode
	r.tree.Walk(func(n *tree.CfNode) {
		if found != nil {
			return
		}
		if n.Title == title {
			found = n
		}
	})
	if found == nil {
		return "", false
	}
	return r.paths.Path(found.PageID)
}
