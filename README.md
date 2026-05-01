# gitfluence (`gfl`)

Project Naming: "Git + Confluence" -> "gitfluence" -> `gfl`

Deterministic, bidirectional synchronization between Markdown files in a Git repository and pages in an Atlassian Confluence space.

`gfl` operates entirely through Git hooks and the Confluence REST API. It mirrors a Confluence space's hierarchy as a directory tree of Markdown files, and uses Git's native branch and merge machinery to reconcile changes from both sides.

Each managed `.md` file carries a small front-matter block recording its `confluence_page_id` and `confluence_version`. A persistent local Git branch named `confluence` always represents the last-known Confluence-side state. `gfl pull` updates that branch from Confluence and merges it into your working branch. `gfl push` diffs your working branch against `confluence`, sends the differences to Confluence, then commits a sync chore on your working branch and fast-forwards `confluence` to match. Conflicts are ordinary `git merge` conflicts, resolved with your normal tools.

Highlights:

- **Bidirectional**, driven by Git events — commits trigger pull, pushes trigger push.
- **Page identity travels with the file**. Renames, moves, and copies preserve the link to the Confluence page automatically.
- **Tree-aware**. Confluence's hierarchy mirrors a directory tree; pages with children become directories with `index.md` as the root page.
- **Deterministic conversion**. Purpose-built Go lexers; round-trips are byte-stable for every supported construct.
- **Confluence panels stay editable**. Info / Note / Success / Warning / Error / Custom panels, plus expand sections and decision items, render as GitHub-style admonitions (`> [!INFO]\n> body`) — you edit the body as plain markdown and the macro round-trips back to Confluence intact. Panel parameters (icons, custom colours, expand titles, image sizing, link `target="_blank"`) survive via an inline `<!--gfl:meta-->` sidecar.
- **Unrepresentable constructs preserved verbatim**. Status badges, jira issue links, user mentions, layouts, decisions, custom panel parameters — anything without a clean Markdown shape is fence-preserved (base64-encoded storage XML) so push round-trips them untouched.
- **Attachments sync both ways**. Pull downloads referenced binaries; push uploads any image referenced from the markdown that lives under `_attachments/`. Confluence's "no-change" rejection is silenced.
- **Conflicts are git conflicts**. Resolve them with your editor and `git merge --continue`.
- **Self-recovering on partial push failure**. Operations that fail simply re-appear in the next push's diff.
- **No external dependencies** beyond Git and the binary itself — no CI, no Pandoc, no LLMs.

## Installation

`gfl` is installed per-developer, not bundled with consuming repositories. Each git user puts the binary in their `PATH`.

### From release binaries

[Pre-compiled binaries](https://github.com/swill/gfl/releases) are published as GitHub release artifacts for `linux/amd64`, `darwin/amd64`, `darwin/arm64`, and `windows/amd64`. Download the appropriate archive, extract, and place `gfl` somewhere on your `PATH` (e.g. `/usr/local/bin`).

Verify the install:

```sh
gfl version
```

### From source

Requires Go 1.22 or later.

```sh
go install github.com/swill/gfl@latest
```

This drops the binary in `$(go env GOPATH)/bin`. Make sure that directory is on your `PATH`.

## Getting started: a new repository

Use `gfl init` to initialize a Git repository from an existing Confluence page tree.

```sh
cd your-git-repo

# Create .env with Confluence credentials
cat > .env <<'EOF'
CONFLUENCE_BASE_URL=https://yourorg.atlassian.net/wiki
CONFLUENCE_USER=your.email@yourorg.com
CONFLUENCE_API_TOKEN=your_api_token
EOF

# Populate the repository from a Confluence page tree
gfl init --page-id <root-page-id> [--local-root docs/]
```

`init` fetches the full page tree, converts each page to Markdown (with front-matter), downloads attachments, and writes:

- `docs/` (or your chosen `--local-root`) — the Markdown file tree
- `.gfl.json` — configuration (root page ID, cached space key, local root, attachments dir)
- `.gitignore` entries for `.env`
- `.gfl/hooks/` shims, installed into `.git/hooks/`

Review the output, then `git add` and commit. Your first post-commit hook will seed the local `confluence` branch from your tree state.

### What gets written

Given a Confluence tree:

```
Root Page
  ├── Architecture
  │     ├── Database Design
  │     └── API Design
  ├── Onboarding
  │     ├── For Developers
  │     └── For Managers
  └── API Reference
```

The local tree:

```
docs/
  index.md                            # Root Page
  _attachments/                       # page-tree-mirrored attachments
    architecture/
      database-design/
        schema.png
  architecture/
    index.md                          # has children → directory
    database-design.md
    api-design.md
  onboarding/
    index.md
    for-developers.md
    for-managers.md
  api-reference.md                    # leaf → flat file
```

Each `.md` file starts with front-matter:

```markdown
---
confluence_page_id: "5233836047"
confluence_version: 12
---

# Page Title

...
```

Conventions:

- Pages with children become directories containing `index.md`; leaf pages are flat `.md` files.
- Filenames are deterministically slugified from page titles.
- Attachments live under `_attachments/` mirroring the page hierarchy.
- Front-matter is canonicalized on every pull (sorted keys, double-quoted strings) so byte-stable round-trips are preserved.

### Editable Confluence constructs

Beyond plain text, lists, headings, and code blocks, several common Confluence constructs come down as editable Markdown — the body stays plain markdown, and the wrapper round-trips back to Confluence on push. The labels are aligned with what the editor shows in Confluence today, NOT with the legacy storage names (which are misnamed for historical reasons):

| Confluence panel  | Markdown                                                  |
|-------------------|-----------------------------------------------------------|
| Info (blue)       | `> [!INFO]`                                               |
| Note (purple)     | `> [!NOTE]`                                               |
| Success (green)   | `> [!SUCCESS]`                                            |
| Warning (yellow)  | `> [!WARNING]`                                            |
| Error (red)       | `> [!ERROR]`                                              |
| Custom panel      | `> [!PANEL]<!--gfl:meta bgColor="#..." panelIcon="..."-->`|
| Expand            | `> [!EXPAND]<!--gfl:meta title="..."-->`                  |
| Decision item     | `> [!DECISION]` (one per item)                            |

`[!TIP]`, `[!IMPORTANT]`, and `[!CAUTION]` are accepted on push as aliases for `[!SUCCESS]`, `[!NOTE]`, and `[!ERROR]` respectively (matching GitHub's admonition spec).

Image sizing/layout, link `target`/`rel`, panel icons and colours, expand titles, and similar attributes that have no native Markdown shape travel via an inline metadata sidecar:

```markdown
![diagram](_attachments/foo/diagram.png)<!--gfl:meta ac:width="1006" ac:layout="center"-->
[example](https://example.com)<!--gfl:meta target="_blank" rel="noopener"-->
> [!INFO]<!--gfl:meta icon="true"-->
> body
```

The sidecar is a plain HTML comment — invisible in rendered Markdown but editable as text.

### Configuration files

`.gfl.json` (tracked):

```json
{
  "confluence_root_page_id": "123456789",
  "confluence_space_key": "DOCS",
  "local_root": "docs/",
  "attachments_dir": "docs/_attachments"
}
```

`.env` (gitignored):

```
CONFLUENCE_BASE_URL=https://yourorg.atlassian.net/wiki
CONFLUENCE_USER=your.email@yourorg.com
CONFLUENCE_API_TOKEN=your_api_token
```

Environment variables of the same names take precedence over `.env`.

## Getting started: an existing gfl repository

When joining a repository someone else has already run `gfl init` on:

```sh
git clone <git-repo>
cd <git-repo>

# Set up credentials
cp .env.example .env
# Edit .env with your Confluence credentials

# Install Git hooks (assumes `gfl` is in your PATH)
gfl install
```

`install` is idempotent; it just copies hook shims from `.gfl/hooks/` into `.git/hooks/`.

After this, ordinary Git operations stay in sync with Confluence:

- `git commit` → post-commit hook runs `gfl pull`
- `git push` → pre-push hook runs `gfl push`
- `git merge` / `git rebase` → post-merge / post-rewrite hooks also run pull

The pull hooks are guarded by `GFL_HOOK_ACTIVE` so the commit that pull itself creates doesn't re-trigger pull. Push self-suppresses the same hooks while it commits its own sync chore.

### Daily commands

| Command       | What it does |
| ------------- | ------------ |
| `gfl status`  | Show files that differ between the working branch and `confluence` — exactly what `push` would attempt. |
| `gfl pull`    | Sync Confluence into the `confluence` branch and merge it into the working branch. Conflicts surface as standard merge conflicts. |
| `gfl push`    | Diff against the `confluence` branch and write changes to Confluence; commit a sync chore on the working branch and fast-forward `confluence` to match. |
| `gfl version` | Print version, commit, and build date. |

You usually don't run `pull` or `push` directly — Git hooks handle them. Use `status` to see what's pending; use the explicit commands when troubleshooting.  If you know there are changes in Confluence you want to pull in, then you can run `gfl pull` to get them so you can build from there locally.

> **NOTE:** When running `git push`, it is possible that Confluence pages will be updated.  In order for your local git repository to have the latest Confluence page versions, `gfl` will pull down the latest version numbers and commit them into the tree.  This means that it can leave your git repo one commit behind the remote.  A second `git push` will update your upstream repo with the latest Confluence page versions commit.

### How sync works

The local branch `confluence` is the canonical "last-known Confluence state" — every file there carries `confluence_page_id` and `confluence_version` in front-matter. *It's machine-managed; don't commit to it directly.*

**Pull** (post-commit / post-merge / post-rewrite):

1. Fetches the Confluence page tree.
2. On the `confluence` branch, applies any creates / updates / renames / deletes (deletes confirmed via direct `GET` → 404) and downloads attachments.
3. Commits as `chore(sync): confluence @ <ts>`.
4. Switches back to the working branch and runs `git merge confluence`.

**Push** (pre-push):

1. Diffs the working branch against `confluence` with rename detection.
2. For each changed `.md`: creates, updates (with 409 retry), deletes, or renames the corresponding Confluence page (renames apply the Title Stability Rule to avoid capitalization drift).
3. Uploads any attachments the markdown references under `_attachments/`. Byte-identical re-uploads (Confluence's HTTP 400 "same file name" response) are treated as success.
4. Round-trips each pushed body through `CfToMd` so its committed form matches what a future pull would produce.
5. Commits `chore(sync): confluence-push @ <ts>` on the working branch and fast-forwards `confluence` to that commit. After a successful push, `confluence` and the working branch tip are byte-equal.

Failed operations don't queue — they re-appear in the next push's diff and are retried then.

## Developing `gfl`

This section is for working on `gfl` itself, not for using it.

### Build

```sh
go build -o gfl .
```

Or with version metadata:

```sh
make build
```

### Test

```sh
go test ./...
```

Tests use real temporary Git repositories (`gitutil/`, `cmd/`) and `httptest.NewServer` (`api/`). The `cmd/` package also includes end-to-end scenario tests against an in-memory mock Confluence (`cmd/e2e_mock_test.go`). No external services required.

### Project structure

```
main.go      Entry point
cmd/         CLI commands (Cobra): init, install, push, pull, status, version
lexer/       Pure text transforms: normalise, frontmatter, cf_to_md, md_to_cf, fence, slugify
api/         Confluence REST v1 client (content, attachments)
gitutil/     Git primitives: branch, diff, merge, stash, mv/rm, content-at-ref
tree/        CfTree/CfNode, PathMap (slug-based path computation), AttachmentDir
config/      .gfl.json and .env credential loading
```

See `CLAUDE.md` for the design rationale, sync invariants, and the round-trip idempotency contract that the lexers must satisfy.

### Releases

Release binaries are built via the `Makefile` `release` target and published as GitHub release artifacts.

### Troubleshooting

If you are seeing issues with the content being changed during a round-trip of a `gfl pull` and `gfl push`, you can review the actual raw content of the confluence page to see why it is not being handled by the lexer correctly.

Get the code the lexer sees for a Confluence page by hitting this URL.

```
https://<your-company>.atlassian.net/wiki/rest/api/content/<page-id>?expand=body.storage
```

## License

See [LICENSE](LICENSE) for details.
