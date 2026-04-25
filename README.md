# confluencer

Deterministic, bidirectional synchronisation between Markdown files in a Git repository and pages in an Atlassian Confluence instance.

`confluencer` operates entirely through Git hooks and the Confluence REST API. It mirrors a Confluence space's hierarchy as a directory tree of Markdown files, and uses Git's native branch and merge machinery to reconcile changes from both sides.

## How it works in one paragraph

Each managed `.md` file carries a small front-matter block recording its `confluence_page_id` and `confluence_version`. A persistent local Git branch named `confluence` always represents the last-known Confluence-side state. `confluencer pull` updates that branch from Confluence, then merges it into your working branch using `git merge`. `confluencer push` diffs your working branch against the `confluence` branch, sends the differences to Confluence, then advances the `confluence` branch to match. Conflicts are ordinary `git merge` conflicts, resolved with your normal tools.

## Features

- **Bidirectional sync** driven by Git events — commits trigger pull, pushes trigger push.
- **Page identity travels with the file**. The `confluence_page_id` is in front-matter, so renames, moves, and copies preserve the link to the Confluence page automatically.
- **Tree-aware**. Confluence's hierarchy mirrors a directory tree; pages with children become directories with `index.md`.
- **Deterministic conversion**. Purpose-built Go lexers convert between Markdown and Confluence storage XML; round-trips are byte-stable for every supported construct.
- **Unsupported constructs preserved verbatim** via a base64-encoded HTML-comment fence — Confluence macros, panels, mentions, etc. survive round-trips intact.
- **Conflicts are git conflicts**. When the two sides genuinely diverge, `git merge` produces conflict markers; you resolve them with your editor and `git merge --continue`.
- **Self-recovering on partial push failure**. Operations that fail simply re-appear in the next push's diff. There is no separate pending queue.
- **No external dependencies**. The binary is self-contained — no CI, no Pandoc, no LLMs. Consuming repositories need only the binary, a `.env` file, and a POSIX shell.

## Installation

### From source

Requires Go 1.22 or later.

```sh
go install github.com/swill/confluencer@latest
```

### From release binaries

Pre-compiled binaries are published as GitHub release artifacts for `linux/amd64`, `darwin/amd64`, `darwin/arm64`, and `windows/amd64`. Place the binary on your `PATH`. `confluencer` is installed per-developer, not bundled with consuming repositories.

## Setup

### 1. Initialise a repository from an existing Confluence tree

```sh
cd your-repo

# Create .env with Confluence credentials
cat > .env <<'EOF'
CONFLUENCE_BASE_URL=https://yourorg.atlassian.net/wiki
CONFLUENCE_USER=your.email@yourorg.com
CONFLUENCE_API_TOKEN=your_api_token
EOF

# Populate the repository from a Confluence page tree
confluencer init --page-id <root-page-id> [--local-root docs/]
```

This fetches the full page tree, converts each page to Markdown (with front-matter), downloads attachments, and writes:

- `docs/` (or your chosen `--local-root`) — the Markdown file tree, every file carrying `confluence_page_id` and `confluence_version` in its front-matter
- `.confluencer.json` — configuration (root page ID, space key, local root)
- `.gitignore` entries for `.env`
- `.confluencer/hooks/` shims, installed into `.git/hooks/`

Review the output, then `git add` and commit. Your first post-commit hook will seed the local `confluence` branch from your tree state.

### 2. Developer onboarding (existing repo)

```sh
git clone <repo>
cd <repo>

# Set up credentials
cp .env.example .env
# Edit .env with your Confluence credentials

# Install Git hooks (assumes `confluencer` is on your PATH — see Installation)
confluencer install
```

After this, ordinary Git operations stay in sync with Confluence:

- `git commit` → post-commit hook runs `confluencer pull`
- `git push` → pre-push hook runs `confluencer push`
- `git merge` / `git rebase` → post-merge / post-rewrite hooks also run pull

The pull hooks are guarded by `CONFLUENCER_HOOK_ACTIVE` so the commit that pull itself creates doesn't re-trigger pull.

## How it works (in detail)

### The `confluence` branch

`confluencer` maintains a local Git branch named `confluence` whose tip always represents the last-known Confluence-side state — every file there carries `confluence_page_id` and `confluence_version` in front-matter. Pull writes commits to it; push reads it to find page identities. Treat it as machine-managed; don't commit to it directly.

It's local-only by default. You can push it to `origin` if you want to share the canonical Confluence-mirror state across developers, but the tool doesn't require it.

### Pull (post-commit / post-merge / post-rewrite hooks)

After any local commit, merge, or rebase, `confluencer pull` runs:

1. Acquires an exclusive file lock at `<git-dir>/confluencer-pull.lock`; exits silently if another pull already holds it.
2. Fetches the full Confluence page tree (structure only — bodies are fetched on demand).
3. Switches to the `confluence` branch.
4. Reads each managed file's front-matter to determine the current page ID → path mapping for this branch.
5. Computes a plan: pages on Confluence not in the local tree → write; pages whose version changed → re-fetch and rewrite; pages whose path changed → `git mv`; pages no longer on Confluence (confirmed via direct GET → 404) → delete.
6. Applies the plan, downloads any new/changed attachments, and commits as `chore(sync): confluence @ <ts>`.
7. Switches back to the working branch and runs `git merge confluence`. Conflicts surface as standard merge conflicts — resolve with your editor and `git merge --continue`.

### Push (pre-push hook)

When you `git push`, `confluencer push` runs:

1. Diffs the working branch against the `confluence` branch with rename detection (`git diff -M`).
2. For each changed `.md` file:
   - **Added** → creates a new Confluence page (or, if HEAD's front-matter already names a real `page_id`, adopts that page and updates).
   - **Modified** → reads the page ID from the `confluence` branch's copy of the file and PUTs the new body. On 409, refetches the version and retries once.
   - **Deleted** → deletes the Confluence page (404 treated as success).
   - **Renamed** → updates the page's title (subject to the Title Stability Rule) and parent (if the directory changed) on Confluence.
3. Replays every successful operation on the `confluence` branch as a file write/move/delete with canonical front-matter, and commits as `chore(sync): confluence-push @ <ts>`.

If any operation fails, it simply re-appears in the next push's diff and is retried. There is no separate pending file.

### Status

```sh
confluencer status
```

Lists every `.md` file that differs between the working branch and the `confluence` branch — i.e. exactly what `confluencer push` would attempt to send.

## File layout

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

Key conventions:

- Pages with children become directories containing `index.md`.
- Leaf pages are flat `.md` files.
- Attachments live under `_attachments/` mirroring the page hierarchy.
- Filenames are deterministically slugified from page titles.
- Front-matter is canonicalised on every pull (sorted keys, double-quoted strings) so byte-stable round-trips are preserved.

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

### `.env` (gitignored)

```
CONFLUENCE_BASE_URL=https://yourorg.atlassian.net/wiki
CONFLUENCE_USER=your.email@yourorg.com
CONFLUENCE_API_TOKEN=your_api_token
```

Environment variables of the same names take precedence over `.env`.

## CLI reference

| Command | Description |
|---|---|
| `confluencer init --page-id <id> [--local-root <path>]` | Populate the repo from an existing Confluence tree; writes config, files (with front-matter), and hook shims, then installs hooks. |
| `confluencer install` | Copy hook shims from `.confluencer/hooks/` into `.git/hooks/` (idempotent). |
| `confluencer push` | Diff against the `confluence` branch and send changes to Confluence. |
| `confluencer pull` | Sync Confluence into the `confluence` branch and merge it. |
| `confluencer status` | Show files that differ between the working branch and `confluence`. |
| `confluencer version` | Print version, commit, and build date. |

## Development

### Build

```sh
go build -o confluencer .
```

### Test

```sh
go test ./...
```

Tests use real temporary Git repositories (`gitutil/`, `cmd/`) and `httptest.NewServer` (`api/`). No external services required.

### Project structure

```
main.go          Entry point
cmd/             CLI commands (Cobra)
lexer/           Markdown ↔ Confluence storage XML conversion + front-matter
api/             Confluence REST API client
gitutil/         Git operations (branches, diffs, merges, stash, mv/rm)
tree/            Confluence tree representation, path computation, attachment paths
config/          Configuration and credential loading
```

## License

See [LICENSE](LICENSE) for details.
