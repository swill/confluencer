package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunStatus_NoConfluenceBranch(t *testing.T) {
	dir := initTestRepo(t)
	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	var buf bytes.Buffer
	statusCmd.SetOut(&buf)

	if err := statusCmd.RunE(statusCmd, nil); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "No \"confluence\" branch yet") {
		t.Errorf("expected unconfigured-state message, got %q", buf.String())
	}
}

func TestRunStatus_InSync(t *testing.T) {
	dir := initTestRepo(t)
	// Create the confluence branch at the same point as HEAD — nothing to push.
	run(t, dir, "git", "branch", "confluence", "HEAD")

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	var buf bytes.Buffer
	statusCmd.SetOut(&buf)

	if err := statusCmd.RunE(statusCmd, nil); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "In sync") {
		t.Errorf("expected in-sync message, got %q", buf.String())
	}
}

func TestRunStatus_OutOfSync(t *testing.T) {
	dir := initTestRepo(t)
	run(t, dir, "git", "branch", "confluence", "HEAD")

	// Add a new .md file on the working branch.
	if err := os.WriteFile(filepath.Join(dir, "new.md"), []byte("# new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "add new")

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	var buf bytes.Buffer
	statusCmd.SetOut(&buf)

	if err := statusCmd.RunE(statusCmd, nil); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "1 file(s) differ") {
		t.Errorf("expected 1 difference reported: %q", output)
	}
	if !strings.Contains(output, "add") || !strings.Contains(output, "new.md") {
		t.Errorf("expected `add new.md` line: %q", output)
	}
}
