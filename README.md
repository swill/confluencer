# confluencer

Deterministic, bidirectional synchronisation between Markdown files in a Git repository and pages in an Atlassian Confluence instance.

`confluencer` operates entirely through Git hooks and the Confluence REST API. It understands the hierarchical structure of a Confluence space and mirrors it as a directory tree of Markdown files. The mapping between pages and files is derived automatically from the tree and maintained in a tracked index file.

## Features

- **Bidirectional sync** — edits on either side (Git or Confluence) are reconciled automatically on every push and pull.
- **Tree-aware** — the full Confluence page hierarchy is mirrored as a directory tree. Pages with children become directories with an `index.md`.
- **Rename and move tracking** — Confluence page IDs are the stable identity. Renames, moves, promotions (flat file to directory), and demotions are detected and propagated in both directions using `git mv` for clean history.
- **Deterministic conversion** — purpose-built Go lexers convert between Markdown and Confluence storage XML. Given the same input, the output is always identical, preventing formatting drift loops.
- **Unsupported construct preservation** — Confluence macros and other constructs that have no Markdown equivalent are preserved verbatim via base64-encoded HTML comment fences. They survive round-trips byte-for-byte.
- **Three-way merge on conflict** — pull applies Confluence-side changes on a temporary branch rooted at the last sync commit, then rebases (or merges) it into your working branch. Concurrent edits on both sides are reconciled by Git's native three-way merge; conflict markers appear only when automatic merge fails.
- **Non-blocking on failure** — Confluence write failures are queued to a pending file and retried on the next push. A failed write never blocks a `git push`.
- **No external dependencies** — the binary is self-contained. No CI, no Pandoc, no LLMs. Consuming repositories need only the binary, a `.env` file, and a POSIX shell.

## Installation

### From source

Requires Go 1.22 or later.

```sh
go install github.com/swill/confluencer@latest
```

### From release binaries

Pre-compiled binaries are published as GitHub release artifacts for:

- `linux/amd64`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`

Download the artifact for your platform and place it on your `PATH`. `confluencer` is installed per-developer, not bundled with consuming repositories.

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

This fetches the full page tree, converts each page to Markdown, downloads attachments, and writes:

- `docs/` (or your chosen `--local-root`) — the Markdown file tree
- `.confluencer.json` — configuration (root page ID, space key, local root)
- `.confluencer-index.json` — the page ID to file path mapping
- `.gitignore` entries for `.env` and `.confluencer-pending`

Review the output, then `git add` and commit.

### 2. Install Git hooks

```sh
confluencer install
```

This copies hook shims from `.confluencer/hooks/` into `.git/hooks/` for `pre-push`, `post-commit`, `post-merge`, and `post-rewrite`. The hooks invoke the `confluencer` binary at the appropriate points in the Git workflow. `confluencer init` runs `install` automatically, so this command is primarily used by developers cloning an existing `confluencer`-managed repository.

### 3. Developer onboarding (existing repo)

```sh
git clone <repo>
cd <repo>

# Set up credentials
cp .env.example .env
# Edit .env with your Confluence credentials

# Install Git hooks (assumes `confluencer` is already on your PATH — see Installation)
confluencer install
```

After this, `git push` (pre-push), `git commit` (post-commit), `git pull` / `git merge` (post-merge), and `git rebase` / `git commit --amend` (post-rewrite) automatically sync with Confluence. Pull hooks are guarded by `CONFLUENCER_HOOK_ACTIVE` so the commit that pull itself creates does not re-trigger pull.

## How it works

### Push (pre-push hook)

When you `git push`, `confluencer push` runs automatically:

1. Drains any previously queued failures from `.confluencer-pending`.
2. Identifies changed `.md` files in the commit range being pushed.
3. Filters out files whose most recent change was a sync commit (already in Confluence).
4. For each changed file:
   - **Deleted**: deletes the Confluence page (404 is treated as success).
   - **Renamed**: updates the page title and/or parent, applying the Title Stability Rule.
   - **Added**: creates a new Confluence page under the correct parent, auto-creating any intermediate pages that don't yet exist.
   - **Modified**: fetches the current version, converts, and updates the page. On version conflict (409), re-fetches the latest version and retries once; if that also fails, the write is queued.
5. Per-item failures are appended to `.confluencer-pending` and retried on the next push. The Git push always proceeds.

### Pull (post-commit / post-merge / post-rewrite hooks)

After any local commit, `git pull`, `git merge`, `git rebase`, or `git commit --amend`, `confluencer pull` runs automatically. Running it after local commits catches Confluence-side edits before the next push.

1. Acquires an exclusive file lock at `<git-dir>/confluencer-pull.lock`; exits silently if another pull already holds it.
2. Fetches the full Confluence page tree (structure only — bodies are fetched on demand).
3. Resolves index entries that are missing from the tree via direct `GET /content/{id}` to distinguish deletions, orphans, and transient fetch failures.
4. Computes a typed change set against the local index (renames, moves, creates, deletes, content changes, promotions, demotions).
5. Stashes any uncommitted local changes and creates a temporary `confluencer-sync` branch at the last in-sync commit (the most recent `chore(sync): confluence` commit, or the most recent commit that modified `.confluencer-index.json`).
6. Applies the change set on the sync branch: deletes, planned moves (with attachment subdirs), creates, content writes, attachment downloads.
7. Commits everything as `chore(sync): confluence` and updates `.confluencer-index.json` (including the Confluence `version` for every page).
8. Switches back to the original branch and `git rebase`s onto the sync branch; on rebase failure, falls back to `git merge`, surfacing any conflicts as standard Git merge conflicts for you to resolve.
9. Deletes the sync branch and pops the stash.

Because the sync branch is based on the last in-sync commit, Git's native three-way merge reconciles concurrent edits on both sides using that commit as the baseline. Conflict markers are written only when Git cannot merge automatically.

### Retry

```sh
confluencer push --retry
```

Drains the `.confluencer-pending` queue outside of a Git push. Useful for manually retrying after transient Confluence outages.

### Status

```sh
confluencer status
```

Reports pending writes and their last errors so you can see what hasn't made it to Confluence yet.

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

The local tree looks like:

```
docs/
  index.md                            # Root Page
  _attachments/                       # page-tree-mirrored attachments
    architecture/
      database-design/
        schema.png
  architecture/
    index.md                          # Architecture (has children → directory)
    database-design.md
    api-design.md
  onboarding/
    index.md                          # Onboarding (has children → directory)
    for-developers.md
    for-managers.md
  api-reference.md                    # API Reference (leaf → flat file)
```

Key conventions:

- Pages with children become directories containing `index.md`.
- Leaf pages are flat `.md` files.
- Attachments live under `_attachments/` mirroring the page hierarchy.
- Filenames are deterministically slugified from page titles.

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

### `.confluencer-index.json` (tracked)

Maps Confluence page IDs to local file paths. Updated automatically by every sync operation. Committed as part of sync commits so all developers share the same mapping.

### `.confluencer-pending` (gitignored)

NDJSON queue of failed Confluence writes. Drained automatically on the next push or manually via `confluencer push --retry`.

## CLI reference

| Command | Description |
|---|---|
| `confluencer init --page-id <id> [--local-root <path>]` | Populate local repo from an existing Confluence tree; writes config, index, and hook shims and installs them |
| `confluencer install` | Copy hook shims from `.confluencer/hooks/` into `.git/hooks/` (idempotent) |
| `confluencer push` | Sync local changes to Confluence (pre-push hook) |
| `confluencer push --retry` | Drain `.confluencer-pending` without a Git push |
| `confluencer pull` | Sync Confluence changes locally (post-commit / post-merge / post-rewrite hook) |
| `confluencer status` | Report pending writes and their last errors |
| `confluencer version` | Print version, commit, and build date |

## Development

### Build

```sh
go build -o confluencer .
```

### Test

```sh
go test ./...
```

Tests use real temporary Git repositories (for `gitutil/` and `cmd/` packages) and `httptest.NewServer` (for `api/` package). No external services or mocks are required.

### Project structure

```
main.go          Entry point
cmd/             CLI commands (Cobra)
lexer/           Markdown ↔ Confluence storage XML conversion
api/             Confluence REST API client
gitutil/         Git operations (diff, mv, merge, baseline)
tree/            Tree diff, path computation, rename planning
index/           Index and pending queue file I/O
config/          Configuration and credential loading
```

## License

See [LICENSE](LICENSE) for details.
