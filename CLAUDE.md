# CLAUDE.md — confluencer

## Project Overview

`confluencer` is a standalone Go CLI that implements deterministic, bidirectional synchronisation between Markdown files tracked in a Git repository and pages in an Atlassian Confluence instance. It operates through Git hooks and the Confluence REST API — no CI, no external services, no LLMs, no Pandoc.

It mirrors the hierarchical structure of a Confluence space rooted at a configured anchor page as a directory tree of Markdown files. Each managed `.md` file carries its Confluence identity in a front-matter block; a persistent local Git branch named `confluence` represents the last-known Confluence-side tree state. Pull and push are diff/merge operations between that branch and your working branch.

`confluencer` is maintained as its own repository. Developers install the binary onto their PATH (`go install`, release artifact, or package manager). Consuming repositories contain only configuration and hook shims — no Go toolchain required.

## Core Design Principles

- **Page identity lives with the file.** Every managed `.md` file has front-matter naming `confluence_page_id` (stable across renames and moves) and `confluence_version` (last seen Confluence version). There is no separate index file.
- **Git is the reconciliation engine.** A local branch called `confluence` always represents the last-known Confluence state. Pull updates that branch, then `git merge`s it into the working branch. Push diffs the working branch against `confluence`, sends the result to Confluence, then advances `confluence`. Conflicts are ordinary `git merge` conflicts.
- **Deterministic conversion.** Both directions are purpose-built Go lexers with canonical output. Given the same input, the output is always identical — primary defence against formatting drift loops.
- **Either side may be the source of truth.** Edits, renames, additions, and deletions on either Git or Confluence are reconciled on every push and pull.
- **Self-recovery on partial push failure.** Failed operations re-appear in the next push's diff and are retried. There is no pending queue.
- **Pure lexers.** No network, filesystem, or git access in the lexer package. Resolvers are injected at call sites.

## Repository Structure

```
confluencer/
  main.go
  cmd/       root, init, install, push, pull, status, version, render, helpers
  lexer/     pure text transforms: normalise, frontmatter, cf_to_md, md_to_cf, fence, slugify
  api/       Confluence REST v1 client: content, attachments
  gitutil/   diff, mv/rm, branch primitives, merge, stash, content-at-ref
  tree/      CfTree/CfNode, PathMap, AttachmentDir, slug-collision disambiguation
  config/    .confluencer.json, .env credential loading
```

## Consuming Repository

```
<repo-root>/
  .confluencer/hooks/        # tracked shims: pre-push, post-commit, post-merge, post-rewrite
  .confluencer.json          # tracked — root page ID, cached space key, local root, attachments dir
  .env                       # gitignored — Confluence credentials
  docs/                      # local root (configured in .confluencer.json)
    index.md
    _attachments/            # page-tree-mirrored assets
    architecture/
      index.md
      database-design.md
    ...
```

Plus a local-only `confluence` branch maintained by the tool.

## Configuration

### `.confluencer.json` (tracked)

```json
{
  "confluence_root_page_id": "123456789",
  "confluence_space_key": "DOCS",
  "local_root": "docs/",
  "attachments_dir": "docs/_attachments"
}
```

`confluence_space_key` is cached on `confluencer init` from the root page metadata so that new-page POST calls don't re-fetch it every run.

### `.env` (gitignored)

```
CONFLUENCE_BASE_URL=https://yourorg.atlassian.net/wiki
CONFLUENCE_USER=your.email@yourorg.com
CONFLUENCE_API_TOKEN=your_api_token
```

Credentials are read from env vars — never CLI flags, to prevent process-listing exposure. Environment variables take precedence over `.env`. Missing credentials yield an actionable error referencing `.env`.

## Front-Matter

Every managed `.md` file begins with a YAML-subset front-matter block:

```markdown
---
confluence_page_id: "5233836047"
confluence_version: 12
---

# Body content starts here
```

Properties:

- `confluence_page_id` — the Confluence page ID (always quoted; Confluence IDs are stringly-typed and frequently exceed 32 bits).
- `confluence_version` — the Confluence version number at last sync. Used to skip body fetches for unchanged pages, and to compute the version for the next write.
- Unknown keys are preserved verbatim in the order they appeared (forward-compatibility).

Canonical serialisation:

- Known keys appear first in fixed order (`confluence_page_id`, then `confluence_version`).
- String values are always double-quoted.
- The closing `---` is followed by exactly one blank line before the body.

`Normalise` preserves the front-matter at the top in canonical form and normalises the body below. `ApplyFrontMatter(ExtractFrontMatter(x)) == Normalise(x)` for canonical inputs.

The lexer itself stays pure. The orchestrator (init, pull, push) extracts and re-applies front-matter; `cf_to_md` and `md_to_cf` only ever see body content.

## The `confluence` Branch

A persistent local Git branch named `confluence` is the canonical representation of "what Confluence looked like at last sync."

- **Seeded** on first pull from the current HEAD via `EnsureBranchFromHead`.
- **Advanced** by pull (a `chore(sync): confluence @ <ts>` commit per sync) and by push (a `chore(sync): confluence-push @ <ts>` commit replaying successful Confluence writes onto it).
- **Local-only by default.** You can push it to `origin` if you want a shared canonical view across developers, but the tool doesn't require it.
- **Don't commit to it manually.** Treat it as machine-managed. The hooks and direct invocations of `confluencer pull` / `push` are the only legitimate writers.

## Tree Structure

### Hierarchy Mirroring

- A page with **no children** → a flat `.md` file named after the slugified title.
- A page with **one or more children** → a directory named after the slugified title, containing `index.md` (the page's own body) plus one child `.md` or subdirectory per child.
- The root anchor page is always `index.md` directly under `local_root`.

Empty `index.md` files are a fully supported state. A body → empty (or vice versa) is a content change handled like any other.

### Attachments

Attachments live under `_attachments/` mirroring the page hierarchy. For a page at logical path `<page-path>`, its attachments are at:

```
<attachments_dir>/<page-path-without-trailing-index.md>/<filename>
```

| Page | Attachment path |
|---|---|
| `docs/index.md` | `docs/_attachments/<file>` |
| `docs/architecture/index.md` | `docs/_attachments/architecture/<file>` |
| `docs/architecture/database-design.md` | `docs/_attachments/architecture/database-design/<file>` |

Properties:

- **No collisions.** Two pages can each reference `image.png` without interference.
- **Confluence filename preserved verbatim.** No Confluence-side attachment renames; upload and download both key on `(page_id, filename)`.
- **Page renames move attachments.** The attachment subdirectory is `git mv`'d alongside the page's `.md` file.
- **Flat and promoted forms share the same attachment dir.** Promotion does not move attachments.

Markdown images use paths relative to the `.md` file:

```markdown
![schema](../_attachments/architecture/database-design/schema.png)
```

`md_to_cf` recognises any path under `_attachments/` and emits `<ac:image><ri:attachment ri:filename="…"/></ac:image>` with just the leaf filename.

### Slugification (`lexer/slugify.go`)

Page title → slug, applied in order:

1. Lowercase.
2. Collapse whitespace runs to single hyphens.
3. Underscores → hyphens.
4. Drop all non-`[a-z0-9-]` characters.
5. Collapse consecutive hyphens; trim leading/trailing hyphens.
6. Empty result falls back to `page-<pageID>`.

**Sibling collision disambiguation** (`DisambiguateSiblings`): when two or more siblings produce the same slug, the one with the numerically lowest page ID keeps the plain slug; every other colliding sibling gets `-<last-6-digits-of-page-id>` appended. Deterministic, collision-free, and stable across renames.

**Reverse slugification** (filename → title, used on push-side creates and renames):

1. Strip `.md`.
2. Strip any trailing `-DDDDDD` collision suffix.
3. Hyphens/underscores → spaces.
4. Title case.

### Title Stability Rule

On a push-direction rename, the Confluence page title is updated **only if** `Slugify(currentTitle) != filenameSlug`. If the new filename slugifies to the same value as the current Confluence title, the title is preserved verbatim — preventing capitalisation and punctuation drift on no-op renames. Implemented as `lexer.TitleSlugsMatch`.

Developers who need specific capitalisation set it in Confluence and let pull propagate it; they should not try to encode capitalisation in filenames.

## Lexer

Pure functions — no I/O. The orchestrator injects `PageResolver` and `AttachmentResolver` for cross-page links and attachment references.

### Front-matter (`lexer/frontmatter.go`)

`ExtractFrontMatter` / `ApplyFrontMatter` / `FrontMatter` struct as described above. Strict parser (typed `PageID`, `Version`, plus an `Extra` slice for forward-compatibility).

### Normalisation (`lexer/normalise.go`)

`Normalise(md string) string` returns Markdown in canonical form. Both lexer outputs pass through it before being returned.

- UTF-8, no BOM, LF line endings, exactly one trailing newline.
- No trailing whitespace; exactly one blank line between top-level blocks.
- ATX headings only (`#` … `######`).
- Emphasis: `*text*`, `**text**`, `~~text~~` (GFM strikethrough).
- Lists: `-` unordered; `1.` for every ordered item (not incrementing); 2-space indent per nesting level.
- Fenced code: triple backticks with lowercased language tag.
- Links: inline `[text](url)` only.
- Images: inline `![alt](path)`.
- Tables: GFM pipe tables; alignment colons preserved.
- Blockquotes: `> ` prefix.
- Thematic break: `---`.
- Both hard (`\\\n`) and soft line breaks are preserved as `\\\n` — Confluence relies on significant line breaks for layout.
- A leading front-matter block is preserved at the top in canonical form.

`Normalise` is idempotent: `Normalise(Normalise(x)) == Normalise(x)`. Malformed front-matter falls through to body-only normalisation rather than erroring.

### cf_to_md and md_to_cf

The full construct mapping lives alongside the implementations in `lexer/cf_to_md.go` and `lexer/md_to_cf.go`. Notable choices:

- `cf_to_md` uses `golang.org/x/net/html` for tokenisation.
- `md_to_cf` uses `goldmark` with the GFM extension; backslash escapes in the AST are resolved to literals before XML encoding to prevent escape accumulation on round trips.
- Confluence code macros are emitted as `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">…</ac:parameter><ac:plain-text-body><![CDATA[…]]></ac:plain-text-body></ac:structured-macro>`.
- Cross-page links: `<ac:link><ri:page …/><ac:plain-text-link-body>…</ac:plain-text-link-body></ac:link>`.
- Image attachment refs: `<ac:image><ri:attachment ri:filename="…"/></ac:image>`.
- Tables extract/emit alignment via `style="text-align: …"` on `<th>`/`<td>`.
- Raw HTML blocks not matching the fence format are wrapped in `<p>` and escaped as text — we never emit `<ac:structured-macro ac:name="html">` (many Confluence Cloud instances disable it).
- `<ac:structured-macro ac:name="toc">` is dropped; unknown `<ac:structured-macro>` is fence-preserved.

The orchestrator is responsible for stripping front-matter before calling `md_to_cf` and re-applying it after `cf_to_md` (via the shared `cmd/render.go` helpers).

### Confluence-Native Fence (`lexer/fence.go`)

Constructs Markdown can't represent (Jira macros, user mentions, panels, layouts, unknown structured macros) are preserved as a single HTML-comment block with base64-encoded storage XML:

```
<!-- confluencer:storage:block:v1:b64
<base64 body wrapped at 76 cols>
-->
```

`DecodeBlockFence(EncodeBlockFence(x)) == x` for arbitrary XML (property-tested).

### Round-Trip Idempotency

The primary correctness property:

- `Normalise(cf_to_md(md_to_cf(body))) == Normalise(body)` for every construct in the supported mapping (where `body` is markdown with no front-matter — front-matter is orchestrator-managed).
- `md_to_cf(cf_to_md(xml))` reaches a fixed point after one round trip.

Fence-preserved constructs round-trip byte-for-byte in storage XML.

## Pull (`cmd/pull.go`)

Triggered by post-commit, post-merge, post-rewrite, and direct invocation. All hook shims are guarded by `CONFLUENCER_HOOK_ACTIVE` to prevent recursion (pull creates its own commits on the `confluence` branch).

Sequence:

1. Acquire an exclusive file lock at `<git-dir>/confluencer-pull.lock`. If held, exit silently — the holder will do the work. Direct invocation reclaims stale locks.
2. Refuse to operate with a dirty working tree (refuse rather than stash, to keep behaviour predictable).
3. Ensure the local `confluence` branch exists (seed from HEAD on first run via `EnsureBranchFromHead`).
4. Fetch the Confluence tree (structure only — bodies fetched on demand).
5. Switch to the `confluence` branch.
6. Walk the working tree under `local_root`, parsing front-matter to map `page_id → {path, version}` (`scanManagedFiles`).
7. Compute the plan (`planPull`):
   - Page in tree, not in local: pending write (create).
   - Page in tree, in local at same path, version differs: pending write (update).
   - Page in tree, in local at different path: rename (and a pending write if version also differs).
   - Page in local, not in tree: delete candidate.
8. Confirm delete candidates via direct `GET /content/{id}`:
   - 404 → confirmed delete.
   - 200 with out-of-scope ancestry → orphaned (warn, leave local file).
   - Network/5xx → unknown (warn, skip this run).
9. Apply the plan: renames first (using a two-phase staging protocol if any rename's destination is another rename's source); then deletes; then pending writes (fetch body, convert, render with front-matter, write file, download attachments).
10. `chore(sync): confluence @ <ts>` commit on the `confluence` branch via `CommitAllOnHead`. If nothing actually changed, the commit is a no-op and the merge step is skipped.
11. Switch back to the working branch.
12. `git merge confluence`. On conflict, surface guidance ("resolve with your editor and `git merge --continue`") and exit 0 — leaving the merge state for the user.

Two-phase rename protocol (when any rename's destination equals another rename's source):

1. Move all sources into `<local_root>/.confluencer-staging/<i>.md`.
2. Move each staged file to its final path.

The staging directory is created and removed within the same sync and never appears in a committed tree.

## Push (`cmd/push.go`)

Triggered by pre-push and direct invocation.

1. Verify the `confluence` branch exists (error otherwise — direct user to run pull first).
2. `gitutil.DiffBranches(confluenceBranch, "HEAD", "*.md")` with rename detection.
3. If empty, "no changes to push" and exit.
4. Sort the diffs: `index.md` files first (parents before children), then non-index files, then renames, then deletes.
5. For each diff, dispatch on action:
   - **Added**: read body from `HEAD`. If front-matter already names a `page_id` that genuinely exists, treat as adopt-then-update; otherwise `POST /content`. Auto-create intermediate parent pages via `ensurePushParents`, writing intermediate `index.md` files to the working tree so the user's next commit picks them up.
   - **Modified**: read `page_id` from the `confluence` branch's copy of the file (the canonical bridge). `GET /content/{id}` for current version, then `PUT` with new body. On 409, refetch and retry once.
   - **Deleted**: read `page_id` from the `confluence` branch's old-path copy. `DELETE /content/{id}`; 404 treated as success.
   - **Renamed**: read `page_id` from the `confluence` branch's old path. Apply Title Stability Rule for the new title. Update parent if the directory changed. `PUT` with new title, body, and parent.
6. Record every successful operation as a `pushOp{Action, OldPath, NewPath, PageID, Version, HeadContent}`.
7. After the API loop, advance the `confluence` branch (`advanceConfluenceBranch`):
   - Stash if working tree is dirty; checkout `confluence`.
   - For each `pushOp`: write/move/delete the corresponding file with canonical front-matter (`writeManagedFile`).
   - `chore(sync): confluence-push @ <ts>` commit.
   - Return to original branch; pop stash if stashed.

Failures don't queue. Whatever didn't succeed will simply re-appear in the next push's diff.

## Hooks

`confluencer init` writes shims to `.confluencer/hooks/` and installs them into `.git/hooks/` in the same step. `confluencer install` performs just the copy — used when cloning an existing confluencer-managed repo.

```sh
# pre-push
#!/bin/sh
set -e
confluencer push

# post-commit / post-merge / post-rewrite (same shape)
#!/bin/sh
set -e
if [ -n "$CONFLUENCER_HOOK_ACTIVE" ]; then
  exit 0
fi
export CONFLUENCER_HOOK_ACTIVE=1
confluencer pull
```

- `pre-push` has no guard — push never creates commits on the working branch, so it can't re-trigger itself.
- `post-commit` runs pull after every commit so Confluence-side edits are caught before the next push.
- `post-rewrite` re-establishes sync after `rebase` / `commit --amend`.
- The pull file lock prevents concurrent pulls; the env-var guard prevents pull's own commit from re-firing pull.

## Confluence REST API (`api/`)

Basic Auth. See `api/content.go` and `api/attachments.go` for the exact endpoints. Implemented operations:

- `GetPage(id)` with `expand=body.storage,version,ancestors,space`
- `GetChildren(parentID)` — paginated
- `FetchTree(rootID, fetchBody)` — BFS walk
- `CreatePage(space, parent, title, xml)`
- `UpdatePage(id, version, title, xml, parentID)` — empty `parentID` = unchanged
- `DeletePage(id)` — cascades to descendants server-side
- `GetAttachments(pageID, filename?)`, `DownloadAttachment(path)`, `UploadAttachment(pageID, filename, data)`

`api.IsConflict` / `api.IsNotFound` classify errors from `APIError`.

## CLI Reference

| Command | Description |
|---|---|
| `confluencer init --page-id <id> [--local-root <path>]` | Populate local repo from an existing Confluence tree. Writes config, files (with front-matter), and hook shims; installs hooks. Does not commit. |
| `confluencer install` | Copy hook shims from `.confluencer/hooks/` into `.git/hooks/`. Idempotent. |
| `confluencer push` | Diff against the `confluence` branch and write changes to Confluence; advance the `confluence` branch on success. |
| `confluencer pull` | Update the `confluence` branch from Confluence and merge it into the current working branch. |
| `confluencer status` | List files differing between the working branch and `confluence`. |
| `confluencer version` | Print version, commit, build date. |

## Implementation Invariants

1. **Round-trip idempotency.** `Normalise(cf_to_md(md_to_cf(x))) == Normalise(x)` for supported constructs (body-only); unsupported constructs round-trip byte-for-byte via the fence; front-matter round-trips through `ExtractFrontMatter` / `ApplyFrontMatter`.
2. **Page ID is the stable identity, carried in front-matter.** Rename detection, history preservation, and identity all key on `confluence_page_id`. Paths and titles are derived, mutable properties.
3. **The `confluence` branch is the only authoritative cache.** No separate index file; no separate pending file. The branch's tip *is* the last-known Confluence-mirror state.
4. **Sync commits are attributable.** Pull commits use `chore(sync): confluence @ <ts>`; push-side replays use `chore(sync): confluence-push @ <ts>`. Human commits must never use either prefix.
5. **Renames use `git mv`.** So `git log --follow` traces history. Local-side rename collisions use the two-phase stash-and-place protocol; Confluence-side does not need it (the API doesn't have name collisions per parent in the same way).
6. **Attachments are co-committed.** A sync commit that modifies a `.md` includes all its referenced attachments under `_attachments/<page-path>/`.
7. **Push never blocks permanently.** Any Confluence write failure surfaces a warning; the diff is recomputed on the next push.
8. **Credentials never appear in logs, flags, or commits.** Env vars only.
9. **Lexers are pure.** No network/filesystem/git access in `lexer/`. Resolvers are injected.
10. **Title Stability Rule.** A push-side rename updates the Confluence title only if `Slugify(currentTitle) != filenameSlug`.
