package cmd

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	cfgpkg "github.com/swill/gfl/config"
	"github.com/swill/gfl/lexer"
	"github.com/swill/gfl/tree"
)

// confluenceBranch is the name of the persistent local branch that mirrors
// the Confluence-side state. Pull writes its sync commits here; push diffs
// the working branch against this ref to compute outstanding work.
const confluenceBranch = "confluence"

// renderPage converts a Confluence storage-XML body to the canonical local
// Markdown form, with confluence_page_id and confluence_version pinned in a
// front-matter block at the top. This is the single source of truth for what
// a managed file looks like — used by both `gfl init` (initial seed)
// and `gfl pull` (subsequent updates) so the two paths produce
// byte-identical output.
func renderPage(pageID, body string, version int, opts lexer.CfToMdOpts) (string, error) {
	mdBody, err := lexer.CfToMd(body, opts)
	if err != nil {
		return "", fmt.Errorf("convert page %s: %w", pageID, err)
	}
	fm := lexer.FrontMatter{PageID: pageID, Version: version}
	return lexer.ApplyFrontMatter(fm, lexer.Normalise(mdBody)), nil
}

// localManagedFile is one .md file on disk that gfl recognises as a
// Confluence-mirror file (it has front-matter with a confluence_page_id).
type localManagedFile struct {
	Path    string // repo-relative, slash-separated
	PageID  string
	Version int
}

// scanManagedFiles walks `localRoot` and returns every .md file that has
// front-matter naming a confluence_page_id. Files without front-matter or
// with malformed front-matter are skipped silently — they may belong to the
// user, not to gfl.
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

// pushAttachmentResolver implements lexer.MdAttachmentResolver for the
// md_to_cf direction. Given a Markdown image src, it determines whether the
// referenced path resolves under the configured attachments directory; if
// so, it returns the leaf filename to embed in <ri:attachment ri:filename>.
//
// The resolver also records every resolved attachment as a side effect, so
// the push path can iterate over them after MdToCf to upload the binary
// files to Confluence (Phase 3 of the image fix). This avoids re-parsing
// the Markdown body.
type pushAttachmentResolver struct {
	localPath      string // repo-relative, slash-separated path of the source .md
	localRoot      string
	attachmentsDir string
	// resolved maps filename → repo-relative path of the on-disk binary,
	// populated as ResolveImage is called during conversion.
	resolved map[string]string
}

// ResolveImage implements lexer.MdAttachmentResolver. It accepts both
// document-relative paths (e.g. "../_attachments/foo/bar.png") and absolute
// repo-relative paths (e.g. "docs/_attachments/foo/bar.png"); the cf_to_md
// direction historically emits the latter form, so the inverse must accept
// it.
func (r *pushAttachmentResolver) ResolveImage(src string) (string, bool) {
	abs := r.canonicalAttachmentPath(src)
	if abs == "" {
		return "", false
	}
	filename := path.Base(abs)
	if r.resolved == nil {
		r.resolved = make(map[string]string)
	}
	r.resolved[filename] = abs
	return filename, true
}

// canonicalAttachmentPath returns the repo-relative cleaned path if src
// references something under r.attachmentsDir, or "" if it doesn't.
//
// Two interpretations are tried, in order:
//
//  1. src as a path relative to the source document's directory (the
//     CommonMark default for `![alt](rel/path)`).
//  2. src as already a repo-relative absolute-ish path (the form the
//     existing cf_to_md AttachmentSrc emits).
//
// Either match is accepted; non-matching srcs (external URLs, paths outside
// the attachments tree) return "".
func (r *pushAttachmentResolver) canonicalAttachmentPath(src string) string {
	if src == "" {
		return ""
	}
	attDirSlash := strings.TrimSuffix(r.attachmentsDir, "/") + "/"

	sourceDir := path.Dir(r.localPath)
	rel := path.Join(sourceDir, src)
	if strings.HasPrefix(rel, attDirSlash) {
		return rel
	}

	direct := path.Clean(src)
	if strings.HasPrefix(direct, attDirSlash) {
		return direct
	}
	return ""
}

// pushPageResolver implements lexer.MdPageResolver against a CfTree and
// PathMap. It resolves Markdown link targets relative to the source
// document's directory, looks up the resulting path in the PathMap, and
// returns the corresponding Confluence page's title and space.
type pushPageResolver struct {
	localPath string
	tree      *tree.CfTree
	paths     *tree.PathMap
}

// ResolveLink implements lexer.MdPageResolver.
func (r *pushPageResolver) ResolveLink(target string) (title, space string, ok bool) {
	if target == "" {
		return "", "", false
	}
	// External links and anchors are not page references.
	if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
		return "", "", false
	}
	// Drop any fragment (#section) before resolving.
	if i := strings.Index(target, "#"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return "", "", false
	}
	sourceDir := path.Dir(r.localPath)
	abs := path.Join(sourceDir, target)
	pageID, ok := r.paths.PageID(abs)
	if !ok {
		return "", "", false
	}
	page := r.tree.Page(pageID)
	if page == nil {
		return "", "", false
	}
	return page.Title, page.SpaceKey, true
}

// pushResolvers builds the md_to_cf resolver pair (pages + attachments) for
// a given source document path. The attachment resolver is returned
// separately so callers (push) can iterate over which attachments were
// resolved during conversion in order to upload them.
func pushResolvers(localPath string, cfg *cfgpkg.Config, ct *tree.CfTree, pm *tree.PathMap) (lexer.MdToCfOpts, *pushAttachmentResolver) {
	pages := &pushPageResolver{localPath: localPath, tree: ct, paths: pm}
	attachments := &pushAttachmentResolver{
		localPath:      localPath,
		localRoot:      cfg.LocalRoot,
		attachmentsDir: cfg.AttachmentsDir,
	}
	return lexer.MdToCfOpts{Pages: pages, Attachments: attachments}, attachments
}
