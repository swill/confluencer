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

func TestCfToMd_AdmonitionMacros(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantPart string
	}{
		{"info", `<ac:structured-macro ac:name="info"><ac:rich-text-body><p>be careful</p></ac:rich-text-body></ac:structured-macro>`, "> **Info:** be careful"},
		{"note", `<ac:structured-macro ac:name="note"><ac:rich-text-body><p>note this</p></ac:rich-text-body></ac:structured-macro>`, "> **Note:** note this"},
		{"warning", `<ac:structured-macro ac:name="warning"><ac:rich-text-body><p>danger</p></ac:rich-text-body></ac:structured-macro>`, "> **Warning:** danger"},
		{"tip", `<ac:structured-macro ac:name="tip"><ac:rich-text-body><p>helpful</p></ac:rich-text-body></ac:structured-macro>`, "> **Tip:** helpful"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runCfToMd(t, c.in, CfToMdOpts{})
			if got != c.wantPart {
				t.Errorf("got %q, want %q", got, c.wantPart)
			}
		})
	}
}

func TestCfToMd_TocMacro_Omitted(t *testing.T) {
	in := `<p>Before.</p><ac:structured-macro ac:name="toc"/><p>After.</p>`
	want := "Before.\n\nAfter."
	if got := runCfToMd(t, in, CfToMdOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestCfToMd_UnknownMacro_Fenced(t *testing.T) {
	// A jira macro is unknown to our mapping; it must round-trip through the
	// Confluence-native fence so a subsequent push back to Confluence
	// preserves it intact.
	in := `<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">PROJ-1</ac:parameter></ac:structured-macro>`
	got := runCfToMd(t, in, CfToMdOpts{})
	if !strings.HasPrefix(got, "<!-- confluencer:storage:block:v1:b64") {
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
