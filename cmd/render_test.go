package cmd

import (
	"testing"

	cfgpkg "github.com/swill/gfl/config"
	"github.com/swill/gfl/lexer"
	"github.com/swill/gfl/tree"
)

// pushAttachmentResolver is what md_to_cf calls during push conversion to
// distinguish attachment image refs (rendered as <ri:attachment>) from
// external image URLs (rendered as <ri:url>). The tests here pin down the
// path-resolution rules so a future change can't quietly regress to the
// pre-fix behaviour where every image src fell through to <ri:url>.

func TestPushAttachmentResolver_RelativeUnderAttachmentsDir(t *testing.T) {
	r := &pushAttachmentResolver{
		localPath:      "docs/architecture/database-design.md",
		localRoot:      "docs/",
		attachmentsDir: "docs/_attachments",
	}
	// `../_attachments/architecture/database-design/schema.png`, when joined
	// with the source's directory, lands under the attachments dir.
	got, ok := r.ResolveImage("../_attachments/architecture/database-design/schema.png")
	if !ok {
		t.Fatalf("expected resolution; got !ok")
	}
	if got != "schema.png" {
		t.Errorf("filename: got %q, want %q", got, "schema.png")
	}
	// Side-effect: the resolver records what it resolved so push can upload
	// the binary.
	if r.resolved["schema.png"] != "docs/_attachments/architecture/database-design/schema.png" {
		t.Errorf("resolved-map mismatch: %+v", r.resolved)
	}
}

func TestPushAttachmentResolver_AbsoluteRepoRelative(t *testing.T) {
	// Older cf_to_md output emits absolute repo-relative paths
	// (`docs/_attachments/...`) rather than `../`-relative ones, so
	// the inverse must accept both forms.
	r := &pushAttachmentResolver{
		localPath:      "docs/architecture/foo.md",
		localRoot:      "docs/",
		attachmentsDir: "docs/_attachments",
	}
	got, ok := r.ResolveImage("docs/_attachments/architecture/foo/x.png")
	if !ok {
		t.Fatalf("expected resolution; got !ok")
	}
	if got != "x.png" {
		t.Errorf("filename: got %q, want %q", got, "x.png")
	}
}

func TestPushAttachmentResolver_RootIndexPage(t *testing.T) {
	// The root index.md sits directly under localRoot; its attachments
	// live one level up from any sub-page's, so path computation must
	// handle this correctly.
	r := &pushAttachmentResolver{
		localPath:      "docs/index.md",
		localRoot:      "docs/",
		attachmentsDir: "docs/_attachments",
	}
	got, ok := r.ResolveImage("_attachments/banner.png")
	if !ok {
		t.Fatalf("expected resolution; got !ok")
	}
	if got != "banner.png" {
		t.Errorf("filename: got %q, want %q", got, "banner.png")
	}
}

func TestPushAttachmentResolver_ExternalURL(t *testing.T) {
	r := &pushAttachmentResolver{
		localPath:      "docs/page.md",
		localRoot:      "docs/",
		attachmentsDir: "docs/_attachments",
	}
	if _, ok := r.ResolveImage("https://example.com/logo.png"); ok {
		t.Errorf("external URL must not resolve as an attachment")
	}
	if len(r.resolved) != 0 {
		t.Errorf("resolved map should be empty for external URL: %+v", r.resolved)
	}
}

func TestPushAttachmentResolver_UnrelatedRelativePath(t *testing.T) {
	r := &pushAttachmentResolver{
		localPath:      "docs/page.md",
		localRoot:      "docs/",
		attachmentsDir: "docs/_attachments",
	}
	if _, ok := r.ResolveImage("./assets/foo.png"); ok {
		t.Errorf("path outside attachments dir must not resolve")
	}
}

func TestPushAttachmentResolver_TrailingSlashOnAttachmentsDir(t *testing.T) {
	// AttachmentsDir is a config field with no canonical form — accept
	// both with and without a trailing slash.
	r := &pushAttachmentResolver{
		localPath:      "docs/page.md",
		localRoot:      "docs/",
		attachmentsDir: "docs/_attachments/",
	}
	got, ok := r.ResolveImage("_attachments/page/foo.png")
	if !ok {
		t.Fatalf("expected resolution despite trailing slash; got !ok")
	}
	if got != "foo.png" {
		t.Errorf("filename: got %q", got)
	}
}

// pushPageResolver is what md_to_cf calls for ac:link page references.
// The tests pin the resolution rules: relative-path joining, fragment
// stripping, and rejection of external URLs / mailtos / pure anchors.

func newSamplePushTree(t *testing.T) (*tree.CfTree, *tree.PathMap) {
	t.Helper()
	root := &tree.CfNode{PageID: "1", Title: "Docs", SpaceKey: "DOCS"}
	arch := &tree.CfNode{PageID: "2", Title: "Architecture", SpaceKey: "DOCS", ParentPageID: "1"}
	api := &tree.CfNode{PageID: "3", Title: "API Design", SpaceKey: "DOCS", ParentPageID: "2"}
	root.Children = []*tree.CfNode{arch}
	arch.Children = []*tree.CfNode{api}
	ct := tree.NewCfTree(root)
	pm := tree.ComputePaths(ct, "docs/")
	return ct, pm
}

func TestPushPageResolver_ResolvesSibling(t *testing.T) {
	ct, pm := newSamplePushTree(t)
	r := &pushPageResolver{localPath: "docs/architecture/index.md", tree: ct, paths: pm}
	title, space, ok := r.ResolveLink("api-design.md")
	if !ok {
		t.Fatalf("expected sibling page to resolve")
	}
	if title != "API Design" || space != "DOCS" {
		t.Errorf("got (%q, %q), want (\"API Design\", \"DOCS\")", title, space)
	}
}

func TestPushPageResolver_ResolvesParent(t *testing.T) {
	ct, pm := newSamplePushTree(t)
	r := &pushPageResolver{localPath: "docs/architecture/api-design.md", tree: ct, paths: pm}
	title, _, ok := r.ResolveLink("index.md")
	if !ok || title != "Architecture" {
		t.Errorf("parent resolution: got (%q, %v), want (\"Architecture\", true)", title, ok)
	}
}

func TestPushPageResolver_StripsFragment(t *testing.T) {
	ct, pm := newSamplePushTree(t)
	r := &pushPageResolver{localPath: "docs/architecture/index.md", tree: ct, paths: pm}
	title, _, ok := r.ResolveLink("api-design.md#routing")
	if !ok || title != "API Design" {
		t.Errorf("fragment-bearing link: got (%q, %v)", title, ok)
	}
}

func TestPushPageResolver_RejectsExternal(t *testing.T) {
	ct, pm := newSamplePushTree(t)
	r := &pushPageResolver{localPath: "docs/architecture/index.md", tree: ct, paths: pm}
	for _, target := range []string{
		"https://example.com",
		"mailto:foo@bar.com",
		"#section",
		"",
	} {
		if _, _, ok := r.ResolveLink(target); ok {
			t.Errorf("must not resolve %q", target)
		}
	}
}

func TestPushPageResolver_RejectsUnknownPath(t *testing.T) {
	ct, pm := newSamplePushTree(t)
	r := &pushPageResolver{localPath: "docs/architecture/index.md", tree: ct, paths: pm}
	if _, _, ok := r.ResolveLink("ghost.md"); ok {
		t.Errorf("unknown page must not resolve")
	}
}

// pushResolvers is the constructor used by every MdToCf call site in push.
// The end-to-end behaviour we care about is that an attachment image in
// Markdown round-trips into <ri:attachment> rather than <ri:url> — a check
// the existing test stub couldn't catch because push wasn't using a real
// resolver.

func TestPushResolvers_AttachmentImageBecomesRiAttachment(t *testing.T) {
	cfg := &cfgpkg.Config{LocalRoot: "docs/", AttachmentsDir: "docs/_attachments"}
	ct, pm := newSamplePushTree(t)
	opts, _ := pushResolvers("docs/architecture/api-design.md", cfg, ct, pm)

	in := "![overview](../_attachments/architecture/api-design/diagram.png)\n"
	got, err := lexer.MdToCf(in, opts)
	if err != nil {
		t.Fatalf("MdToCf: %v", err)
	}
	want := `<p><ac:image ac:alt="overview"><ri:attachment ri:filename="diagram.png"/></ac:image></p>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPushResolvers_PageLinkBecomesAcLink(t *testing.T) {
	cfg := &cfgpkg.Config{LocalRoot: "docs/", AttachmentsDir: "docs/_attachments"}
	ct, pm := newSamplePushTree(t)
	opts, _ := pushResolvers("docs/architecture/index.md", cfg, ct, pm)

	in := "See [API Design](api-design.md).\n"
	got, err := lexer.MdToCf(in, opts)
	if err != nil {
		t.Fatalf("MdToCf: %v", err)
	}
	// Must emit <ac:link><ri:page …/>… not <a href>.
	if !contains(got, `<ri:page ri:content-title="API Design"`) {
		t.Errorf("expected <ri:page> reference, got: %s", got)
	}
	if contains(got, `href="api-design.md"`) {
		t.Errorf("internal link leaked as <a href>: %s", got)
	}
}

func TestPushResolvers_RecordsResolvedAttachments(t *testing.T) {
	cfg := &cfgpkg.Config{LocalRoot: "docs/", AttachmentsDir: "docs/_attachments"}
	ct, pm := newSamplePushTree(t)
	opts, attRes := pushResolvers("docs/architecture/api-design.md", cfg, ct, pm)

	in := "![](../_attachments/architecture/api-design/a.png) ![](../_attachments/architecture/api-design/b.png)\n"
	if _, err := lexer.MdToCf(in, opts); err != nil {
		t.Fatalf("MdToCf: %v", err)
	}
	if len(attRes.resolved) != 2 {
		t.Errorf("expected 2 resolved attachments, got %v", attRes.resolved)
	}
	if attRes.resolved["a.png"] != "docs/_attachments/architecture/api-design/a.png" {
		t.Errorf("a.png path: %q", attRes.resolved["a.png"])
	}
	if attRes.resolved["b.png"] != "docs/_attachments/architecture/api-design/b.png" {
		t.Errorf("b.png path: %q", attRes.resolved["b.png"])
	}
}

// stubAttachmentResolver is the cf_to_md (pull) direction's resolver.
// The path it emits is what users see embedded in their Markdown — it
// must render correctly when viewed from the directory of the source .md
// file. Repo-rooted paths break in GitHub web, IDE previews, and any
// CommonMark renderer.

func TestStubAttachmentResolver_RootIndexPage(t *testing.T) {
	r := &stubAttachmentResolver{
		localPath:      "docs/index.md",
		attachmentsDir: "docs/_attachments",
		localRoot:      "docs/",
	}
	got := r.AttachmentSrc("banner.png")
	if got != "_attachments/banner.png" {
		t.Errorf("root index: got %q, want %q", got, "_attachments/banner.png")
	}
}

func TestStubAttachmentResolver_FlatPageAtLocalRoot(t *testing.T) {
	r := &stubAttachmentResolver{
		localPath:      "docs/diagrams.md",
		attachmentsDir: "docs/_attachments",
		localRoot:      "docs/",
	}
	got := r.AttachmentSrc("aptum_offerings.png")
	if got != "_attachments/diagrams/aptum_offerings.png" {
		t.Errorf("flat page at local root: got %q, want %q", got, "_attachments/diagrams/aptum_offerings.png")
	}
}

func TestStubAttachmentResolver_NestedFlatPage(t *testing.T) {
	r := &stubAttachmentResolver{
		localPath:      "docs/sample/diagrams.md",
		attachmentsDir: "docs/_attachments",
		localRoot:      "docs/",
	}
	got := r.AttachmentSrc("aptum_offerings.png")
	if got != "../_attachments/sample/diagrams/aptum_offerings.png" {
		t.Errorf("nested flat page: got %q, want %q", got, "../_attachments/sample/diagrams/aptum_offerings.png")
	}
}

func TestStubAttachmentResolver_NestedIndexPage(t *testing.T) {
	// docs/architecture/index.md — its attachment dir is
	// docs/_attachments/architecture, which from docs/architecture/ is
	// "../_attachments/architecture".
	r := &stubAttachmentResolver{
		localPath:      "docs/architecture/index.md",
		attachmentsDir: "docs/_attachments",
		localRoot:      "docs/",
	}
	got := r.AttachmentSrc("schema.png")
	if got != "../_attachments/architecture/schema.png" {
		t.Errorf("nested index page: got %q, want %q", got, "../_attachments/architecture/schema.png")
	}
}

func TestStubAttachmentResolver_DeeplyNested(t *testing.T) {
	r := &stubAttachmentResolver{
		localPath:      "docs/a/b/c.md",
		attachmentsDir: "docs/_attachments",
		localRoot:      "docs/",
	}
	got := r.AttachmentSrc("img.png")
	if got != "../../_attachments/a/b/c/img.png" {
		t.Errorf("deeply nested: got %q, want %q", got, "../../_attachments/a/b/c/img.png")
	}
}

// Round-trip: the path that cf_to_md emits via stubAttachmentResolver
// must be one that md_to_cf's pushAttachmentResolver can resolve back to
// the same filename. Without this, pull→edit→push would lose the
// attachment link on every cycle.
func TestStubAttachmentResolver_RoundTripsThroughPushResolver(t *testing.T) {
	cases := []struct {
		name      string
		localPath string
	}{
		{"root index", "docs/index.md"},
		{"flat at root", "docs/diagrams.md"},
		{"nested flat", "docs/sample/diagrams.md"},
		{"nested index", "docs/architecture/index.md"},
		{"deeply nested", "docs/a/b/c.md"},
	}
	cfg := &cfgpkg.Config{LocalRoot: "docs/", AttachmentsDir: "docs/_attachments"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pull := &stubAttachmentResolver{
				localPath:      c.localPath,
				attachmentsDir: cfg.AttachmentsDir,
				localRoot:      cfg.LocalRoot,
			}
			emitted := pull.AttachmentSrc("foo.png")

			push := &pushAttachmentResolver{
				localPath:      c.localPath,
				localRoot:      cfg.LocalRoot,
				attachmentsDir: cfg.AttachmentsDir,
			}
			got, ok := push.ResolveImage(emitted)
			if !ok {
				t.Fatalf("push resolver couldn't recover attachment from %q", emitted)
			}
			if got != "foo.png" {
				t.Errorf("filename round-trip lost: got %q, want %q (via path %q)", got, "foo.png", emitted)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
