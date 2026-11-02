// Package vcs wraps the git operations the engine needs: worktrees per task,
// commit trailers that carry lineage, and merging task branches back.
package vcs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a git working copy.
type Repo struct{ Dir string }

func (r Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// IsRepo reports whether Dir is inside a git work tree.
func (r Repo) IsRepo() bool {
	out, err := r.git("rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// Head returns the current commit hash.
func (r Repo) Head() (string, error) { return r.git("rev-parse", "HEAD") }

// CurrentBranch returns the checked-out branch name.
func (r Repo) CurrentBranch() (string, error) { return r.git("rev-parse", "--abbrev-ref", "HEAD") }

// AddWorktree creates a worktree at path on a new branch from base.
func (r Repo) AddWorktree(path, branch, base string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := r.git("worktree", "add", "-b", branch, path, base); err != nil {
		// Branch may exist from a previous attempt; reuse it.
		if _, err2 := r.git("worktree", "add", path, branch); err2 != nil {
			return err
		}
	}
	return nil
}

// RemoveWorktree removes a worktree (force) and leaves the branch in place.
func (r Repo) RemoveWorktree(path string) error {
	_, err := r.git("worktree", "remove", "--force", path)
	return err
}

// Commit stages everything in the worktree and commits with trailers. It
// returns the new hash, or "" when there was nothing to commit.
func (r Repo) Commit(message string, trailers map[string]string) (string, error) {
	if _, err := r.git("add", "-A"); err != nil {
		return "", err
	}
	status, err := r.git("status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status == "" {
		return "", nil
	}
	var b strings.Builder
	b.WriteString(message)
	b.WriteString("\n\n")
	keys := make([]string, 0, len(trailers))
	for k := range trailers {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, trailers[k])
	}
	if _, err := r.git("-c", "user.name=gpn", "-c", "user.email=gpn@localhost", "commit", "-q", "-m", b.String()); err != nil {
		return "", err
	}
	return r.Head()
}

// Merge merges branch into the current branch without fast-forward.
func (r Repo) Merge(branch, message string) error {
	_, err := r.git("-c", "user.name=gpn", "-c", "user.email=gpn@localhost", "merge", "--no-ff", "-m", message, branch)
	return err
}

// ChangedFiles lists files changed between two commits.
func (r Repo) ChangedFiles(from, to string) ([]string, error) {
	out, err := r.git("diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// Init creates a repository with one empty commit (used by gpn init on a
// directory that is not yet a repo, and by tests).
func Init(dir string) error {
	r := Repo{Dir: dir}
	if _, err := r.git("init", "-q", "-b", "main"); err != nil {
		if _, err2 := r.git("init", "-q"); err2 != nil {
			return err
		}
	}
	_, err := r.git("-c", "user.name=gpn", "-c", "user.email=gpn@localhost", "commit", "-q", "--allow-empty", "-m", "gpn init")
	return err
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
