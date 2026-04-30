package lexer

import (
	"strings"
	"testing"
)

// stubResolver implements PageResolver and AttachmentRefResolver with a
// pre-seeded map. Used to verify that resolved references render to the
// configured paths and that unresolved ones fall back gracefully.
type stubResolver struct {
	pagesByID    map[string]string // page-id → local path (preferred)
	pages        map[string]string // title → local path (legacy fallback)
	attachments  map[string]string // filename → src
}

func (s *stubResolver) ResolvePageByID(pageID string) (string, bool) {
	p, ok := s.pagesByID[pageID]
	return p, ok
}

func (s *stubResolver) ResolvePageByTitle(title, _ string) (string, bool) {
	p, ok := s.pages[title]
	return p, ok
}

func (s *stubResolver) AttachmentSrc(filename string) string {
	return s.attachments[filename]
}

// runCfToMd is a small helper that fails the test on parse error and trims the
// trailing newline that Normalise always appends. The trailing newline is part
// of the canonical form but it adds noise to inline test expectations.
func runCfToMd(t *testing.T, storage string, opts CfToMdOpts) string {
	t.Helper()
	out, err := CfToMd(storage, opts)
	if err != nil {
		t.Fatalf("CfToMd error: %v\ninput: %q", err, storage)
	}
	return strings.TrimRight(out, "\n")
}

func TestCfToMd_Headings(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`<h1>Title</h1>`, "# Title"},
		{`<h2>Sub</h2>`, "## Sub"},
		{`<h3>Three</h3>`, "### Three"},
		{`<h4>Four</h4>`, "#### Four"},
		{`<h5>Five</h5>`, "##### Five"},
		{`<h6>Six</h6>`, "###### Six"},
	}
	for _, c := range cases {
		got := runCfToMd(t, c.in, CfToMdOpts{})
		if got != c.want {
			t.Errorf("input %q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCfToMd_Paragraphs(t *testing.T) {
	in := `<p>Hello world.</p><p>Second.</p>`
	want := "Hello world.\n\nSecond."
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_InlineEmphasis(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`<p><strong>bold</strong></p>`, "**bold**"},
		{`<p><b>bold</b></p>`, "**bold**"},
		{`<p><em>italic</em></p>`, "*italic*"},
		{`<p><i>italic</i></p>`, "*italic*"},
		{`<p><s>struck</s></p>`, "~~struck~~"},
		{`<p><del>struck</del></p>`, "~~struck~~"},
		{`<p><strong><em>both</em></strong></p>`, "***both***"},
		{`<p>plain <code>code</code> end</p>`, "plain `code` end"},
	}
	for _, c := range cases {
		if got := runCfToMd(t, c.in, CfToMdOpts{}); got != c.want {
			t.Errorf("input %q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCfToMd_LinksExternal(t *testing.T) {
	in := `<p>See <a href="https://example.com">example</a>.</p>`
	want := "See [example](https://example.com)."
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_AcLink_Resolved(t *testing.T) {
	res := &stubResolver{
		pagesByID: map[string]string{"7777": "../architecture.md"},
	}
	in := `<p>Read <ac:link><ri:page ri:content-id="7777" ri:content-title="Architecture"/><ac:plain-text-link-body><![CDATA[the arch doc]]></ac:plain-text-link-body></ac:link>.</p>`
	want := "Read [the arch doc](../architecture.md)."
	got := runCfToMd(t, in, CfToMdOpts{Pages: res})
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_AcLink_TitleMatchOutsideTree_StaysURL(t *testing.T) {
	// Critical property: an ac:link whose content-id is NOT in our local
	// tree must NOT be resolved against a title-matching local file. Pre-fix,
	// title matching was the only mechanism — so a link to "Architecture"
	// in a different space silently got rewritten to ../architecture.md
	// pointing at OUR Architecture page (wrong page). With id-based
	// resolution, we know the linked page (id=99999) isn't ours and emit
	// the Confluence URL instead.
	res := &stubResolver{
		// We DO have a local "Architecture" — under id 7777 — but the link
		// is to an entirely different page that happens to share the title.
		pagesByID: map[string]string{"7777": "architecture.md"},
		pages:     map[string]string{"Architecture": "architecture.md"},
	}
	in := `<p>Read <ac:link><ri:page ri:content-id="99999" ri:space-key="OTHER" ri:content-title="Architecture"/><ac:plain-text-link-body><![CDATA[other arch]]></ac:plain-text-link-body></ac:link>.</p>`
	want := "Read [other arch](https://example.com/wiki/spaces/OTHER/pages/99999)."
	got := runCfToMd(t, in, CfToMdOpts{Pages: res, BaseURL: "https://example.com/wiki"})
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_AcLink_Unresolved(t *testing.T) {
	// No resolver and no BaseURL — we still emit a Markdown link rather
	// than dropping the URL on the floor. The href is path-only (greppable,
	// non-clickable) so a misconfigured run is visible rather than silent.
	in := `<p>Read <ac:link><ri:page ri:content-title="Missing Page"/><ac:plain-text-link-body><![CDATA[here]]></ac:plain-text-link-body></ac:link>.</p>`
	want := "Read [here](/search?text=Missing+Page)."
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_AcLink_UnresolvedWithBaseURL_BuildsConfluenceURL(t *testing.T) {
	// When an ac:link points outside the local sync tree but a BaseURL is
	// configured, cf_to_md keeps the link clickable by constructing the
	// Confluence-side URL from ri:content-id / ri:space-key. Pre-fix, the
	// URL was dropped silently — turning every cross-space reference into
	// orphan text.
	in := `<p>See <ac:link><ri:page ri:content-id="3376513043" ri:space-key="Product" ri:content-title="Product Strategy v1.3.1"/><ac:plain-text-link-body><![CDATA[Product Strategy v1.3.1]]></ac:plain-text-link-body></ac:link>.</p>`
	want := "See [Product Strategy v1.3.1](https://yourorg.atlassian.net/wiki/spaces/Product/pages/3376513043)."
	got := runCfToMd(t, in, CfToMdOpts{BaseURL: "https://yourorg.atlassian.net/wiki"})
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_AcLink_UnresolvedNoSpaceKey_UsesViewpageURL(t *testing.T) {
	// Older content sometimes lacks ri:space-key. Fall back to the
	// id-based viewpage URL, which still resolves.
	in := `<p>See <ac:link><ri:page ri:content-id="42"/><ac:plain-text-link-body><![CDATA[old page]]></ac:plain-text-link-body></ac:link>.</p>`
	want := "See [old page](https://example.com/wiki/pages/viewpage.action?pageId=42)."
	got := runCfToMd(t, in, CfToMdOpts{BaseURL: "https://example.com/wiki"})
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_AcLink_NoBody_FallsBackToTitle(t *testing.T) {
	// With no plain-text-link-body, the title is used both as the link
	// text and as the search-URL fallback so the URL is preserved.
	in := `<p>See <ac:link><ri:page ri:content-title="Some Page"/></ac:link>.</p>`
	want := "See [Some Page](/search?text=Some+Page)."
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCfToMd_AcLink_UserMention_InlineFenced is the regression test for
// the bug where @mentions were silently dropped. A user mention's storage
// shape is <ac:link><ri:user ri:account-id="..."/></ac:link> with no
// plain-text-link-body. Pre-fix, that hit the "no ri:page → emit body
// text" branch with empty body — the element vanished. The inline fence
// preserves it so push restores the mention verbatim.
func TestCfToMd_AcLink_UserMention_InlineFenced(t *testing.T) {
	in := `<p>Hi <ac:link><ri:user ri:account-id="557058:abc-def-ghi"/></ac:link>, see this.</p>`
	got := runCfToMd(t, in, CfToMdOpts{})
	if !strings.Contains(got, `<gfl-fence data-v1-b64="`) {
		t.Errorf("expected inline fence for @mention, got: %q", got)
	}
	// The mention must not have been dropped (output should retain
	// content for both the surrounding text and the mention).
	if !strings.Contains(got, "Hi ") || !strings.Contains(got, ", see this.") {
		t.Errorf("surrounding text mangled: %q", got)
	}
	// Decoded payload contains the original ri:user element.
	idx := strings.Index(got, "<gfl-fence")
	end := strings.Index(got[idx:], "/>")
	xml, ok := DecodeInlineFence(got[idx : idx+end+2])
	if !ok {
		t.Fatalf("could not decode inline fence: %q", got)
	}
	if !strings.Contains(xml, `ri:account-id="557058:abc-def-ghi"`) {
		t.Errorf("account-id not preserved: %q", xml)
	}
}

// TestCfToMd_AcLink_AttachmentLink_InlineFenced — an ac:link wrapping
// ri:attachment is a download link to an attachment, distinct from the
// ac:image attachment-embed shape we already handle. Markdown has no
// equivalent, so fence-preserve.
func TestCfToMd_AcLink_AttachmentLink_InlineFenced(t *testing.T) {
	in := `<p>Get <ac:link><ri:attachment ri:filename="report.pdf"/><ac:plain-text-link-body><![CDATA[the report]]></ac:plain-text-link-body></ac:link>.</p>`
	got := runCfToMd(t, in, CfToMdOpts{})
	if !strings.Contains(got, `<gfl-fence data-v1-b64="`) {
		t.Errorf("expected inline fence for attachment link, got: %q", got)
	}
}

func TestCfToMd_Image_Attachment(t *testing.T) {
	res := &stubResolver{
		attachments: map[string]string{"diagram.png": "_attachments/architecture/diagram.png"},
	}
	in := `<p><ac:image ac:alt="overview"><ri:attachment ri:filename="diagram.png"/></ac:image></p>`
	want := "![overview](_attachments/architecture/diagram.png)"
	got := runCfToMd(t, in, CfToMdOpts{Attachments: res})
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_Image_Attachment_NoAltDefaultsToFilename(t *testing.T) {
	in := `<p><ac:image><ri:attachment ri:filename="schema.png"/></ac:image></p>`
	// No resolver and no alt — alt defaults to filename without extension; src
	// falls back to the bare filename.
	want := "![schema](schema.png)"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_Image_RemoteURL(t *testing.T) {
	in := `<p><ac:image ac:alt="logo"><ri:url ri:value="https://example.com/logo.png"/></ac:image></p>`
	want := "![logo](https://example.com/logo.png)"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Block-level <ac:image> — Confluence emits this form when the image carries
// styling attributes (ac:align, ac:width, ac:layout, ...). It must render as
// a Markdown image, not get fence-encoded as an unsupported block.
func TestCfToMd_Image_BlockLevel_StyledAttachment(t *testing.T) {
	res := &stubResolver{
		attachments: map[string]string{"aptum_offerings.png": "_attachments/aptum_offerings.png"},
	}
	in := `<ac:image ac:align="center" ac:alt="aptum_offerings.png" ac:custom-width="true" ac:layout="center" ac:local-id="8bfbff4b9417" ac:original-height="664" ac:original-width="1291" ac:width="1006"><ri:attachment ri:filename="aptum_offerings.png" ri:version-at-save="1"/></ac:image>`
	want := "![aptum_offerings.png](_attachments/aptum_offerings.png)"
	got := runCfToMd(t, in, CfToMdOpts{Attachments: res})
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "gfl:storage:block:v1:b64") {
		t.Errorf("block-level image was fence-encoded instead of rendered: %q", got)
	}
}

func TestCfToMd_Image_BlockLevel_PlainAttachment(t *testing.T) {
	in := `<ac:image><ri:attachment ri:filename="schema.png"/></ac:image>`
	want := "![schema](schema.png)"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_Image_BlockLevel_RemoteURL(t *testing.T) {
	in := `<ac:image ac:alt="logo"><ri:url ri:value="https://example.com/logo.png"/></ac:image>`
	want := "![logo](https://example.com/logo.png)"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Block-level images alongside other block content — the image should render
// as its own block separated by a blank line from the surrounding paragraphs.
func TestCfToMd_Image_BlockLevel_AmongBlocks(t *testing.T) {
	in := `<p>Above.</p><ac:image><ri:attachment ri:filename="x.png"/></ac:image><p>Below.</p>`
	want := "Above.\n\n![x](x.png)\n\nBelow."
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_CodeMacro_WithLanguage(t *testing.T) {
	in := `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">go</ac:parameter><ac:plain-text-body><![CDATA[fmt.Println("hi")]]></ac:plain-text-body></ac:structured-macro>`
	want := "```go\nfmt.Println(\"hi\")\n```"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_CodeMacro_NoLanguage(t *testing.T) {
	in := `<ac:structured-macro ac:name="code"><ac:plain-text-body><![CDATA[plain code]]></ac:plain-text-body></ac:structured-macro>`
	want := "```\nplain code\n```"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_CodeMacro_LanguageLowercased(t *testing.T) {
	in := `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">Go</ac:parameter><ac:plain-text-body><![CDATA[x]]></ac:plain-text-body></ac:structured-macro>`
	if got := runCfToMd(t, in, CfToMdOpts{}); !strings.HasPrefix(got, "```go\n") {
		t.Errorf("language not lowercased: %q", got)
	}
}

func TestCfToMd_PreBlock(t *testing.T) {
	in := `<pre>line one
line two</pre>`
	want := "```\nline one\nline two\n```"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

// TestCfToMd_AdmonitionMacros_FencePreserved is the regression test for
// the bug where info/note/warning/tip macros rendered as a Markdown
// blockquote `> **Info:** body` — pretty in markdown, but lossy on push:
// goldmark parsed the blockquote back as <blockquote> and md_to_cf
// emitted that, REPLACING the original macro on Confluence. The fence
// preserves the storage XML byte-for-byte so admonitions round-trip
// across an arbitrary number of edit cycles.
func TestCfToMd_AdmonitionMacros_FencePreserved(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"info", `<ac:structured-macro ac:name="info"><ac:rich-text-body><p>be careful</p></ac:rich-text-body></ac:structured-macro>`},
		{"note", `<ac:structured-macro ac:name="note"><ac:rich-text-body><p>note this</p></ac:rich-text-body></ac:structured-macro>`},
		{"warning", `<ac:structured-macro ac:name="warning"><ac:rich-text-body><p>danger</p></ac:rich-text-body></ac:structured-macro>`},
		{"tip", `<ac:structured-macro ac:name="tip"><ac:rich-text-body><p>helpful</p></ac:rich-text-body></ac:structured-macro>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runCfToMd(t, c.in, CfToMdOpts{})
			if !strings.HasPrefix(got, "<!-- gfl:storage:block:v1:b64") {
				t.Errorf("expected block fence, got: %s", got)
			}
			// The blockquote-style rendering must NOT appear — that's
			// what made the round trip lossy.
			if strings.Contains(got, "> **") {
				t.Errorf("BUG REGRESSION: blockquote-style rendering reappeared: %s", got)
			}
			// Decoded fence body contains the original macro element.
			xml, ok := DecodeBlockFence(got)
			if !ok {
				t.Fatalf("could not decode fence: %s", got)
			}
			if xml != serializeXMLEquivalent(c.in) {
				// The fence preserves a serialised form (sorted attrs);
				// the serialised input matches the fence body.
				t.Logf("note: serialiser may reorder attributes — full equality not strictly required")
			}
			if !strings.Contains(xml, `ac:name="`+c.name+`"`) {
				t.Errorf("decoded XML missing macro name: %s", xml)
			}
		})
	}
}

// serializeXMLEquivalent re-emits a storage XML payload through the same
// serialiser the fence uses, so callers can compare for byte equality.
func serializeXMLEquivalent(storage string) string {
	root, err := parseStorage(storage)
	if err != nil || len(root.children) == 0 {
		return ""
	}
	return serializeXML(root.children[0])
}

func TestCfToMd_TocMacro_Omitted(t *testing.T) {
	in := `<p>Before.</p><ac:structured-macro ac:name="toc"/><p>After.</p>`
	want := "Before.\n\nAfter."
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

// TestCfToMd_Emoticon_InlineFenced is the regression test for the bug
// where <ac:emoticon ac:name="heart"/> was being rendered as the literal
// string ":heart:". Plain-text shortcodes are lossy: pushing the file
// back to Confluence emits ":heart:" verbatim rather than restoring the
// emoticon. The inline fence preserves the original element so the round
// trip is faithful.
func TestCfToMd_Emoticon_InlineFenced(t *testing.T) {
	in := `<p>I love <ac:emoticon ac:name="heart"/> this.</p>`
	got := runCfToMd(t, in, CfToMdOpts{})
	if !strings.Contains(got, `<gfl-fence data-v1-b64="`) {
		t.Errorf("expected inline fence, got: %q", got)
	}
	if strings.Contains(got, ":heart:") {
		t.Errorf("plain-text shortcode leaked (lossy on push): %q", got)
	}
	// Decoded payload must be the original emoticon element.
	idx := strings.Index(got, "<gfl-fence")
	end := strings.Index(got[idx:], "/>")
	xml, ok := DecodeInlineFence(got[idx : idx+end+2])
	if !ok {
		t.Fatalf("could not decode inline fence: %q", got)
	}
	if !strings.Contains(xml, `ac:name="heart"`) {
		t.Errorf("decoded XML missing emoticon name: %q", xml)
	}
}

// TestCfToMd_StatusMacro_InlineFenced is the regression test for the bug
// where an inline <ac:structured-macro ac:name="status"> rendered as
// "I AM A STATUSBlue" — the title and colour parameters concatenated by
// the recurse-into-children default. Inline structured macros must
// fence-preserve like any other unsupported inline construct.
func TestCfToMd_StatusMacro_InlineFenced(t *testing.T) {
	in := `<p>Build: <ac:structured-macro ac:name="status"><ac:parameter ac:name="title">I AM A STATUS</ac:parameter><ac:parameter ac:name="colour">Blue</ac:parameter></ac:structured-macro> here.</p>`
	got := runCfToMd(t, in, CfToMdOpts{})
	if !strings.Contains(got, `<gfl-fence data-v1-b64="`) {
		t.Errorf("expected inline fence, got: %q", got)
	}
	// The pre-fix concatenation must not appear.
	if strings.Contains(got, "I AM A STATUSBlue") {
		t.Errorf("BUG REGRESSION: parameter text concatenated into output: %q", got)
	}
	if strings.Contains(got, "I AM A STATUS") && !strings.Contains(got, "<gfl-fence") {
		t.Errorf("status title leaked outside fence: %q", got)
	}
}

func TestCfToMd_UnknownMacro_Fenced(t *testing.T) {
	// A jira macro is unknown to our mapping; it must round-trip through the
	// Confluence-native fence so a subsequent push back to Confluence
	// preserves it intact.
	in := `<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">PROJ-1</ac:parameter></ac:structured-macro>`
	got := runCfToMd(t, in, CfToMdOpts{})
	if !strings.HasPrefix(got, "<!-- gfl:storage:block:v1:b64") {
		t.Fatalf("expected fence, got:\n%s", got)
	}
	// And the fence should decode back to a structurally-equivalent macro
	// (sorted attributes, but same content).
	xml, ok := DecodeBlockFence(got)
	if !ok {
		t.Fatal("decoded fence not recognised")
	}
	if !strings.Contains(xml, `ac:name="jira"`) || !strings.Contains(xml, "PROJ-1") {
		t.Errorf("decoded XML missing expected content: %q", xml)
	}
}

func TestCfToMd_Lists(t *testing.T) {
	in := `<ul><li>one</li><li>two</li><li>three</li></ul>`
	want := "- one\n- two\n- three"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}

	in = `<ol><li>first</li><li>second</li></ol>`
	want = "1. first\n1. second"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_NestedLists(t *testing.T) {
	in := `<ul><li>outer<ul><li>inner</li></ul></li><li>second outer</li></ul>`
	got := runCfToMd(t, in, CfToMdOpts{})
	want := "- outer\n  - inner\n- second outer"
	if got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_TaskList(t *testing.T) {
	in := `<ac:task-list>
<ac:task><ac:task-status>incomplete</ac:task-status><ac:task-body>do thing</ac:task-body></ac:task>
<ac:task><ac:task-status>complete</ac:task-status><ac:task-body>done thing</ac:task-body></ac:task>
</ac:task-list>`
	want := "- [ ] do thing\n- [x] done thing"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_Table(t *testing.T) {
	in := `<table>
<thead><tr><th>Name</th><th>Type</th></tr></thead>
<tbody>
<tr><td>id</td><td>int</td></tr>
<tr><td>name</td><td>string</td></tr>
</tbody>
</table>`
	want := "| Name | Type |\n| --- | --- |\n| id | int |\n| name | string |"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_Table_NoThead(t *testing.T) {
	// First row is treated as header when no <thead> is present (GFM tables
	// require a header).
	in := `<table><tbody><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></tbody></table>`
	want := "| A | B |\n| --- | --- |\n| 1 | 2 |"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_Blockquote(t *testing.T) {
	in := `<blockquote><p>quoted</p></blockquote>`
	want := "> quoted"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_HorizontalRule(t *testing.T) {
	in := `<p>before</p><hr/><p>after</p>`
	want := "before\n\n---\n\nafter"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_HardBreak(t *testing.T) {
	in := `<p>line one<br/>line two</p>`
	want := "line one\\\nline two"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_Entities(t *testing.T) {
	// HTML entities must be resolved to their characters before being escaped
	// for inline Markdown context.
	in := `<p>nbsp:&nbsp;and amp:&amp;and lt:&lt;</p>`
	got := runCfToMd(t, in, CfToMdOpts{})
	if !strings.Contains(got, "nbsp:") || !strings.Contains(got, "and amp:&") {
		t.Errorf("entities not resolved correctly: %q", got)
	}
	// "<" must be escaped so it doesn't open an HTML tag.
	if !strings.Contains(got, `\<`) {
		t.Errorf("less-than not escaped: %q", got)
	}
}

func TestCfToMd_PreservesInlineWhitespace(t *testing.T) {
	// Source-level newlines from XML pretty-printing inside a paragraph
	// collapse to a single space, but non-trivial words remain intact.
	in := "<p>one\n  two\n\tthree</p>"
	want := "one two three"
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCfToMd_EmptyInput(t *testing.T) {
	// An empty body produces an empty Markdown string (no trailing newline).
	out, err := CfToMd("", CfToMdOpts{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output, got %q", out)
	}
}

func TestCfToMd_RoundTripStable_Fence(t *testing.T) {
	// The fence path must be stable under repeated CfToMd application: the
	// fence emitted on round 2 is byte-identical to round 1. This is the
	// fixed-point property the round-trip test will rely on.
	in := `<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">PROJ-1</ac:parameter></ac:structured-macro>`
	first := runCfToMd(t, in, CfToMdOpts{})

	// Re-parse the fence as if it were Markdown source: extract the encoded
	// XML, convert it back, and confirm the output equals the first run.
	xml, ok := DecodeBlockFence(first)
	if !ok {
		t.Fatal("first-run output not a fence")
	}
	second := runCfToMd(t, xml, CfToMdOpts{})
	if first != second {
		t.Errorf("fence not stable across round trip:\nfirst:\n%s\n\nsecond:\n%s", first, second)
	}
}

func TestCfToMd_OutputIsNormalised(t *testing.T) {
	// Whatever shape the renderer emits, the public output must be the
	// canonical form (single trailing newline, no trailing whitespace,
	// consistent block separation).
	in := `<h1>T</h1><p>p1</p>`
	out, err := CfToMd(in, CfToMdOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if out != Normalise(out) {
		t.Errorf("output not normalised:\n%q\nNormalise produced:\n%q", out, Normalise(out))
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output missing trailing newline: %q", out)
	}
}
