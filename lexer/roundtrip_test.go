package lexer

import (
	"strings"
	"testing"
)

// Round-trip tests assert the two correctness invariants called out in
// CLAUDE.md's Round-Trip Idempotency section:
//
//  A. Normalise(cf_to_md(md_to_cf(x))) == Normalise(x) for every supported
//     Markdown construct. This is the property that prevents formatting drift
//     loops: a file written by the developer, pushed to Confluence, and
//     pulled back must match the canonical form byte-for-byte.
//
//  B. md_to_cf(cf_to_md(storage)) reaches a fixed point after one round
//     trip. The first conversion of arbitrary Confluence storage XML may
//     canonicalise attribute ordering, whitespace, or tag shape, but every
//     subsequent round trip must produce identical bytes. Without this, every
//     pull-then-push cycle produces a diff and drives an infinite loop of
//     sync commits.
//
// Coverage targets: every row of both mapping tables in CLAUDE.md, plus a
// representative set of fence-preserved constructs (unknown ac:structured-
// macro instances, arbitrary XML inside the Confluence-native fence).

// paired is a test-only resolver that simultaneously satisfies all four
// resolver interfaces (PageResolver + AttachmentRefResolver for cf_to_md;
// MdPageResolver + MdAttachmentResolver for md_to_cf). Holding the forward
// and reverse maps in one place makes it impossible to accidentally break
// round-trip symmetry by seeding one direction and forgetting the other.
type paired struct {
	// idToPath[pageID] = local path that <ac:link><ri:page ri:content-id/>
	// maps to. The id-based discriminator cf_to_md prefers because ids are
	// unique across spaces.
	idToPath map[string]string
	// titleToPath[title] = local path used when storage XML lacks
	// ri:content-id (legacy fallback only).
	titleToPath map[string]string
	// pathToTitle[path] = title to emit when md_to_cf sees a link to this
	// path. Should be the inverse of titleToPath for round-trip tests.
	pathToTitle map[string]string
	// filenameToSrc[filename] = Markdown image src for a given attachment
	// filename.
	filenameToSrc map[string]string
	// srcToFilename[src] = filename that md_to_cf should emit for an image
	// with this src. Inverse of filenameToSrc.
	srcToFilename map[string]string
}

func (p *paired) ResolvePageByID(pageID string) (string, bool) {
	s, ok := p.idToPath[pageID]
	return s, ok
}
func (p *paired) ResolvePageByTitle(title, _ string) (string, bool) {
	s, ok := p.titleToPath[title]
	return s, ok
}
func (p *paired) AttachmentSrc(filename string) string {
	return p.filenameToSrc[filename]
}
func (p *paired) ResolveLink(target string) (string, string, bool) {
	t, ok := p.pathToTitle[target]
	return t, "", ok
}
func (p *paired) ResolveImage(src string) (string, bool) {
	f, ok := p.srcToFilename[src]
	return f, ok
}

func newPaired() *paired {
	return &paired{
		titleToPath: map[string]string{
			"Architecture": "architecture.md",
			"API Design":   "api-design.md",
		},
		pathToTitle: map[string]string{
			"architecture.md": "Architecture",
			"api-design.md":   "API Design",
		},
		filenameToSrc: map[string]string{
			"diagram.png": "_attachments/architecture/diagram.png",
		},
		srcToFilename: map[string]string{
			"_attachments/architecture/diagram.png": "diagram.png",
		},
	}
}

// --- A: Markdown round trip -------------------------------------------------

func TestRoundTrip_Markdown(t *testing.T) {
	// Each case is a canonical-form Markdown snippet that must survive one
	// full round trip unchanged. Inputs are already in normal form so any
	// byte difference in the output reveals a real bug — not a cosmetic
	// normalisation nudge.
	cases := []struct {
		name string
		in   string
	}{
		{"heading-1", "# Title\n"},
		{"heading-6", "###### Six\n"},
		{"paragraph", "Hello world.\n"},
		{"two paragraphs", "One.\n\nTwo.\n"},
		{"strong", "A **bold** word.\n"},
		{"em", "An *italic* word.\n"},
		{"strike", "A ~~struck~~ word.\n"},
		{"inline-code", "Call `x()` now.\n"},
		{"bullet-list", "- one\n- two\n- three\n"},
		{"ordered-list", "1. first\n1. second\n"},
		{"nested-list", "- outer\n  - inner\n- second\n"},
		{"task-list", "- [ ] todo\n- [x] done\n"},
		{"fenced-code-no-lang", "```\nplain\n```\n"},
		{"fenced-code-with-lang", "```go\nfmt.Println(\"hi\")\n```\n"},
		{"external-link", "See [example](https://example.com).\n"},
		{"external-image", "![logo](https://example.com/l.png)\n"},
		{"blockquote", "> quoted\n"},
		{"thematic-break", "a\n\n---\n\nb\n"},
		{"hard-break", "line one\\\nline two\n"},
		{
			"table",
			"| Name | Type |\n| --- | --- |\n| id | int |\n| name | string |\n",
		},
		{
			"code-with-cdata-close",
			// Code containing ]]> must be CDATA-escaped in the XML step and
			// recovered intact on the way back.
			"```\na ]]> b\n```\n",
		},
		{
			"page-link",
			"Read [Architecture](architecture.md) for details.\n",
		},
		{
			"attachment-image",
			"![diagram](_attachments/architecture/diagram.png)\n",
		},
		{
			"fence-preserved",
			// A completely opaque storage construct must survive the Markdown
			// round trip because md_to_cf recognises the fence and splices
			// the XML back verbatim.
			EncodeBlockFence(`<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">PROJ-1</ac:parameter></ac:structured-macro>`) + "\n",
		},
		{
			"mixed-doc",
			"# Guide\n\nIntro paragraph with **bold** and *italic*.\n\n## Steps\n\n1. First\n1. Second\n\n> Note this.\n\n```go\nreturn nil\n```\n",
		},
	}

	res := newPaired()
	opts := CfToMdOpts{Pages: res, Attachments: res}
	mdOpts := MdToCfOpts{Pages: res, Attachments: res}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := Normalise(c.in)
			storage, err := MdToCf(in, mdOpts)
			if err != nil {
				t.Fatalf("MdToCf: %v", err)
			}
			roundTripped, err := CfToMd(storage, opts)
			if err != nil {
				t.Fatalf("CfToMd: %v", err)
			}
			if roundTripped != in {
				t.Errorf("round-trip mismatch.\n\nINPUT:\n%s\nSTORAGE:\n%s\nROUND-TRIPPED:\n%s",
					showWS(in), showWS(storage), showWS(roundTripped))
			}
		})
	}
}

// TestRoundTrip_Markdown_Idempotent exercises the stronger property that two
// round trips produce the same result as one. If the first round trip
// canonicalises anything, the second must be a no-op — otherwise successive
// pull/push cycles would keep rewriting the file.
func TestRoundTrip_Markdown_Idempotent(t *testing.T) {
	cases := []string{
		"# T\n\npara\n",
		"- a\n- b\n",
		"```go\nx := 1\n```\n",
		"> quoted\n\npara\n",
		"| a | b |\n| --- | --- |\n| 1 | 2 |\n",
		EncodeBlockFence(`<ac:structured-macro ac:name="jira"/>`) + "\n",
	}
	res := newPaired()
	opts := CfToMdOpts{Pages: res, Attachments: res}
	mdOpts := MdToCfOpts{Pages: res, Attachments: res}

	for _, in := range cases {
		in := Normalise(in)
		once := rt(t, in, mdOpts, opts)
		twice := rt(t, once, mdOpts, opts)
		if once != twice {
			t.Errorf("not idempotent:\nINPUT:\n%s\nONCE:\n%s\nTWICE:\n%s",
				showWS(in), showWS(once), showWS(twice))
		}
	}
}

// --- B: Storage XML fixed point ---------------------------------------------

func TestRoundTrip_Storage_FixedPoint(t *testing.T) {
	// The storage side of the round trip reaches a fixed point after one
	// conversion: storage → md → storage is canonical, and doing it again
	// produces identical bytes.
	cases := []struct {
		name string
		xml  string
	}{
		{"heading", `<h1>Title</h1>`},
		{"paragraph", `<p>Hello.</p>`},
		{"emphasis", `<p><strong>bold</strong> and <em>italic</em></p>`},
		{"code-macro", `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">go</ac:parameter><ac:plain-text-body><![CDATA[x := 1]]></ac:plain-text-body></ac:structured-macro>`},
		{"table", `<table><tbody><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></tbody></table>`},
		{"list", `<ul><li>a</li><li>b</li></ul>`},
		{"task-list", `<ac:task-list><ac:task><ac:task-status>incomplete</ac:task-status><ac:task-body>do</ac:task-body></ac:task></ac:task-list>`},
		{"blockquote", `<blockquote><p>quoted</p></blockquote>`},
		{"hr", `<p>a</p><hr/><p>b</p>`},
		{"external-link", `<p>See <a href="https://example.com">ex</a>.</p>`},
		{"page-link", `<p>See <ac:link><ri:page ri:content-title="Architecture"/><ac:plain-text-link-body><![CDATA[arch]]></ac:plain-text-link-body></ac:link>.</p>`},
		{"attachment", `<p><ac:image ac:alt="diagram"><ri:attachment ri:filename="diagram.png"/></ac:image></p>`},
		{"external-image", `<p><ac:image ac:alt="logo"><ri:url ri:value="https://example.com/l.png"/></ac:image></p>`},
		// Block-level <ac:image> with full Confluence-editor styling — the
		// styling attributes are dropped on the first conversion (Markdown
		// can't represent them), but the result must reach a fixed point.
		{"attachment-block-styled", `<ac:image ac:align="center" ac:alt="diagram" ac:custom-width="true" ac:layout="center" ac:original-height="664" ac:original-width="1291" ac:width="1006"><ri:attachment ri:filename="diagram.png" ri:version-at-save="1"/></ac:image>`},
		{"unknown-macro", `<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">PROJ-1</ac:parameter></ac:structured-macro>`},
		{"admonition-info", `<ac:structured-macro ac:name="info"><ac:rich-text-body><p>watch out</p></ac:rich-text-body></ac:structured-macro>`},
		{"admonition-note", `<ac:structured-macro ac:name="note"><ac:rich-text-body><p>FYI</p></ac:rich-text-body></ac:structured-macro>`},
		// Inline-fence-preserved constructs: emoticons and inline
		// structured macros. Pre-fix these were lossy — emoticons became
		// `:name:` shortcode plain text, inline structured macros had
		// their parameter texts concatenated into the surrounding paragraph.
		{"emoticon-inline", `<p>love <ac:emoticon ac:name="heart"/> it</p>`},
		{"status-macro-inline", `<p>Build: <ac:structured-macro ac:name="status"><ac:parameter ac:name="title">DONE</ac:parameter><ac:parameter ac:name="colour">Green</ac:parameter></ac:structured-macro> shipped.</p>`},
		// Fence at paragraph start — the comment-form fence triggered
		// CommonMark HTML block detection (Type 2 "<!-- ..."), which
		// silently swallowed the rest of the line and any second fence.
		// The custom-element fence avoids this.
		{"status-macro-paragraph-start", `<p><ac:structured-macro ac:name="status" ac:macro-id="abc" ac:schema-version="1"><ac:parameter ac:name="title">I AM A STATUS</ac:parameter><ac:parameter ac:name="colour">Blue</ac:parameter></ac:structured-macro> at start.</p>`},
		// Two fences in one paragraph — the user's exact reported failure.
		{"two-status-macros-in-paragraph", `<p><ac:structured-macro ac:name="status" ac:macro-id="abc" ac:schema-version="1"><ac:parameter ac:name="title">First</ac:parameter></ac:structured-macro> and what about <ac:structured-macro ac:name="status" ac:macro-id="def" ac:schema-version="1"><ac:parameter ac:name="title">Second</ac:parameter></ac:structured-macro></p>`},
		// Emoji-style emoticon with all the attributes Confluence really
		// emits: fallback character, emoji-id, shortname, local-id, name.
		{"emoji-emoticon-rich", `<p>I love <ac:emoticon ac:emoji-fallback="😍" ac:emoji-id="1f60d" ac:emoji-shortname=":heart_eyes:" ac:local-id="3998095f428c" ac:name="blue-star"/> seeing it.</p>`},
		// User mentions — fence-preserved through the inline fence.
		{"user-mention", `<p>Hi <ac:link><ri:user ri:account-id="557058:abc-def-ghi"/></ac:link>, ping me.</p>`},
		// Mention at paragraph start — exercises the same line-start
		// HTML-block-detection path the status macro hit earlier.
		{"user-mention-paragraph-start", `<p><ac:link><ri:user ri:account-id="557058:abc"/></ac:link> please review.</p>`},
		// Attachment download link (distinct from <ac:image> embeds).
		{"attachment-download-link", `<p>See <ac:link><ri:attachment ri:filename="report.pdf"/><ac:plain-text-link-body><![CDATA[the report]]></ac:plain-text-link-body></ac:link>.</p>`},
		// Multi-paragraph admonition — fence-preserved so all blocks
		// inside the rich-text-body survive the round trip.
		{"info-multi-paragraph", `<ac:structured-macro ac:name="info"><ac:rich-text-body><p>first paragraph</p><p>second paragraph</p></ac:rich-text-body></ac:structured-macro>`},
		{"warning-with-formatting", `<ac:structured-macro ac:name="warning"><ac:rich-text-body><p>be <strong>very</strong> careful</p></ac:rich-text-body></ac:structured-macro>`},
		// Mixed: prose, then info panel, then more prose. Pre-fix, an
		// edit to either prose paragraph would replace the panel with a
		// generic <blockquote> on Confluence.
		{"info-between-paragraphs", `<p>Above.</p><ac:structured-macro ac:name="info"><ac:rich-text-body><p>panel body</p></ac:rich-text-body></ac:structured-macro><p>Below.</p>`},
	}
	res := newPaired()
	opts := CfToMdOpts{Pages: res, Attachments: res}
	mdOpts := MdToCfOpts{Pages: res, Attachments: res}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// First full round trip: storage → md → storage.
			once := storageRT(t, c.xml, opts, mdOpts)
			// Second: feed the result back around. It must be identical.
			twice := storageRT(t, once, opts, mdOpts)
			if once != twice {
				t.Errorf("not a fixed point:\nINPUT:\n%s\nAFTER-1:\n%s\nAFTER-2:\n%s",
					c.xml, once, twice)
			}
		})
	}
}

// --- Property-style tests ---------------------------------------------------

// TestRoundTrip_AdmonitionSurvivesEdits is the regression test for the
// user-reported bug: an info panel in Confluence pulls down to markdown,
// and the very first user edit (to an unrelated paragraph in the same
// page) replaces the panel with a generic <blockquote> on push. The fix:
// fence-preserve the macro so it survives an arbitrary number of
// pull→edit→push cycles.
//
// Concretely: storage → md → simulate a user edit on an unrelated line
// → md → storage. The admonition macro on the second storage must match
// the first.
func TestRoundTrip_AdmonitionSurvivesEdits(t *testing.T) {
	original := `<p>intro paragraph</p><ac:structured-macro ac:name="info"><ac:rich-text-body><p>important info</p></ac:rich-text-body></ac:structured-macro><p>outro paragraph</p>`

	// Cycle 1: pull → edit unrelated line → push.
	md1, err := CfToMd(original, CfToMdOpts{})
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(md1, "intro paragraph", "intro edited", 1)
	if edited == md1 {
		t.Fatalf("intro substitution didn't apply to: %q", md1)
	}
	pushed1, err := MdToCf(edited, MdToCfOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pushed1, `ac:name="info"`) {
		t.Fatalf("info macro lost on cycle 1 push:\n%s", pushed1)
	}
	if strings.Contains(pushed1, "<blockquote>") {
		t.Errorf("BUG REGRESSION: admonition replaced by <blockquote>:\n%s", pushed1)
	}
	if !strings.Contains(pushed1, "important info") {
		t.Errorf("admonition body content lost: %s", pushed1)
	}

	// Cycle 2: re-pull and edit again — the macro must STILL survive.
	md2, err := CfToMd(pushed1, CfToMdOpts{})
	if err != nil {
		t.Fatal(err)
	}
	edited2 := strings.Replace(md2, "outro paragraph", "outro edited", 1)
	pushed2, err := MdToCf(edited2, MdToCfOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pushed2, `ac:name="info"`) {
		t.Errorf("info macro lost on cycle 2 push:\n%s", pushed2)
	}
	if !strings.Contains(pushed2, "important info") {
		t.Errorf("admonition body content lost on cycle 2: %s", pushed2)
	}
}

// TestRoundTrip_FencePreservesArbitraryXML verifies that an opaque XML
// payload inside a v1/b64 fence survives the full md_to_cf(cf_to_md(...))
// loop byte-for-byte. This is the load-bearing guarantee for unsupported
// constructs: whatever Confluence sends us, we preserve.
func TestRoundTrip_FencePreservesArbitraryXML(t *testing.T) {
	opaque := []string{
		`<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">PROJ-1</ac:parameter></ac:structured-macro>`,
		`<ac:structured-macro ac:name="layout"><ac:layout-section><ac:layout-cell><p>x</p></ac:layout-cell></ac:layout-section></ac:structured-macro>`,
		// Payload containing "]]>", "<", ">", "&".
		`<custom><![CDATA[if a < b && c > d then ]]> stop]]></custom>`,
		// Binary-ish content (base64 handles any bytes).
		"<weird>\x00\x01\x02 non-print</weird>",
		// Unicode — we don't want UTF-8 mangled.
		`<say>café — résumé 日本語 🎉</say>`,
	}
	for _, xml := range opaque {
		fenced := EncodeBlockFence(xml) + "\n"
		md := Normalise(fenced)
		storage, err := MdToCf(md, MdToCfOpts{})
		if err != nil {
			t.Fatalf("MdToCf: %v", err)
		}
		if !strings.Contains(storage, xml) {
			t.Errorf("opaque XML not preserved verbatim:\n  want embedded: %q\n  got storage:   %q", xml, storage)
		}
	}
}

// TestRoundTrip_CfToMd_Idempotent confirms that applying cf_to_md twice to
// the storage produced by md_to_cf is a no-op — another statement of the
// fixed-point property, framed from the Markdown side.
func TestRoundTrip_CfToMd_Idempotent(t *testing.T) {
	inputs := []string{
		"# Doc\n\npara\n",
		"- a\n- b\n  - nested\n",
		"| a | b |\n| --- | --- |\n| 1 | 2 |\n",
	}
	res := newPaired()
	opts := CfToMdOpts{Pages: res, Attachments: res}
	mdOpts := MdToCfOpts{Pages: res, Attachments: res}
	for _, in := range inputs {
		in := Normalise(in)
		storage, _ := MdToCf(in, mdOpts)
		md1, _ := CfToMd(storage, opts)
		// CfToMd output is canonical; another pass through MdToCf+CfToMd
		// should equal md1.
		storage2, _ := MdToCf(md1, mdOpts)
		md2, _ := CfToMd(storage2, opts)
		if md1 != md2 {
			t.Errorf("not idempotent:\nINPUT:\n%s\nMD1:\n%s\nMD2:\n%s", in, md1, md2)
		}
	}
}

// --- Helpers ----------------------------------------------------------------

func rt(t *testing.T, md string, mdOpts MdToCfOpts, opts CfToMdOpts) string {
	t.Helper()
	storage, err := MdToCf(md, mdOpts)
	if err != nil {
		t.Fatalf("MdToCf: %v", err)
	}
	out, err := CfToMd(storage, opts)
	if err != nil {
		t.Fatalf("CfToMd: %v", err)
	}
	return out
}

func storageRT(t *testing.T, xml string, opts CfToMdOpts, mdOpts MdToCfOpts) string {
	t.Helper()
	md, err := CfToMd(xml, opts)
	if err != nil {
		t.Fatalf("CfToMd: %v", err)
	}
	out, err := MdToCf(md, mdOpts)
	if err != nil {
		t.Fatalf("MdToCf: %v", err)
	}
	return out
}

// showWS renders whitespace visibly so round-trip failures are easy to read
// in test output — the usual suspects are tab-vs-space and trailing newlines.
func showWS(s string) string {
	r := strings.NewReplacer("\t", "→", " ", "·", "\n", "↵\n")
	return r.Replace(s)
}
