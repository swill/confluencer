package gitutil

import (
	"fmt"
	"os/exec"
	"strings"
)

// BranchExists reports whether a local branch with the given name exists.
func BranchExists(repoDir, name string) (bool, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Dir = repoDir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		// Non-zero exit from rev-parse --verify --quiet means "not found".
		return false, nil
	}
	return false, fmt.Errorf("git rev-parse refs/heads/%s: %w", name, err)
}

// SetBranchRef force-updates the named branch to point at ref. Used by push
// to fast-forward the confluence branch to the working branch's tip after
// committing the sync chore directly on main, so confluence and main stay
// in lockstep without requiring a merge commit.
//
// Refuses to operate on the currently-checked-out branch (git rejects
// `git branch -f` on the active branch).
func SetBranchRef(repoDir, name, ref string) error {
	cmd := exec.Command("git", "branch", "-f", name, ref)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -f %s %s: %s: %w", name, ref, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// EnsureBranchFromHead creates the named branch pointing at the current HEAD
// commit if it doesn't already exist. Idempotent. Does not check out the new
// branch — caller is responsible for that.
//
// The repo must have at least one commit; you can't seed a branch from an
// unborn HEAD.
func EnsureBranchFromHead(repoDir, name string) error {
	exists, err := BranchExists(repoDir, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	cmd := exec.Command("git", "branch", name, "HEAD")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git branch %s HEAD: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// IsClean reports whether the working tree has no staged or unstaged changes
// to tracked files. Untracked files do not count as "dirty" — they don't
// block branch checkout. (Use HasUncommittedChanges for the broader check.)
func IsClean(repoDir string) (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=no")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return len(strings.TrimSpace(string(out))) == 0, nil
}

// CommitAllOnHead stages every change in the working tree and creates a commit
// on the current branch with the given message. Returns the new commit's SHA.
// If there are no staged changes after `git add -A`, returns "" and no error
// (a no-op call — caller should treat as "nothing to sync").
func CommitAllOnHead(repoDir, message string) (string, error) {
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add -A: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// Check whether anything is staged.
	diffCmd := exec.Command("git", "diff", "--cached", "--quiet")
	diffCmd.Dir = repoDir
	if err := diffCmd.Run(); err == nil {
		// Empty diff — nothing staged, nothing to commit.
		return "", nil
	}
	commitCmd := exec.Command("git", "commit", "-m", message)
	commitCmd.Dir = repoDir
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return HeadSHA(repoDir)
}

// MergeFrom runs `git merge --no-edit <from>` while on the current branch.
// Returns conflict=true with no error when the merge produced conflicts and
// the repository is now in a merge state — the caller is responsible for
// guiding the user to resolve, or aborting via AbortMerge.
//
// Other failures (no such branch, etc.) return an error.
func MergeFrom(repoDir, from string) (conflict bool, err error) {
	return mergeRun(repoDir, []string{"merge", "--no-edit", from}, from)
}

func mergeRun(repoDir string, args []string, from string) (conflict bool, err error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return false, nil
	}
	if mergeInProgress(repoDir) {
		return true, nil
	}
	return false, fmt.Errorf("git merge %s: %s: %w", from, strings.TrimSpace(string(out)), runErr)
}

// AbortMerge cancels an in-progress merge, restoring the index and working
// tree to the pre-merge state.
func AbortMerge(repoDir string) error {
	cmd := exec.Command("git", "merge", "--abort")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git merge --abort: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// mergeInProgress checks for the presence of <git-dir>/MERGE_HEAD by asking
// rev-parse for it. Returns false on any error (best-effort signal).
func mergeInProgress(repoDir string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

// DiffBranches returns the file changes needed to transform baseRef into
// headRef, with rename detection enabled (-M). pathspecs (optional) restrict
// the diff to those paths or globs.
//
// Output semantics for confluencer's push flow when called as
// DiffBranches(repo, "confluence", "HEAD"):
//   - A: file exists in HEAD but not confluence → create on Confluence.
//   - D: file exists in confluence but not HEAD → delete on Confluence.
//   - M: both have it, content differs → update on Confluence.
//   - R: same file under different paths → rename on Confluence.
func DiffBranches(repoDir, baseRef, headRef string, pathspecs ...string) ([]FileDiff, error) {
	args := []string{"diff", "--name-status", "-M", baseRef, headRef}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git diff %s %s: %s: %w", baseRef, headRef, strings.TrimSpace(string(out)), err)
	}
	return parseDiffOutput(string(out))
}

// ReadFileAtRef returns the contents of path at the given ref (branch, tag,
// or commit). Returns os.ErrNotExist (wrapped) if path does not exist at ref.
func ReadFileAtRef(repoDir, ref, path string) ([]byte, error) {
	cmd := exec.Command("git", "show", ref+":"+path)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w", ref, path, err)
	}
	return out, nil
}
