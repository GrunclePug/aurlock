package aur

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Repo is a local mirror of one package base's AUR git repository. It's
// bare, so fetching it never checks anything out or runs hooks.
type Repo struct {
	Dir string
}

// RepoURL is the git URL of a package base.
func RepoURL(baseURL, pkgbase string) string {
	return baseURL + "/" + pkgbase + ".git"
}

// Sync clones the repository at url into dir, or fetches it if dir
// already holds a clone.
func Sync(ctx context.Context, dir, url string) (*Repo, error) {
	r := &Repo{Dir: dir}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		if _, err := r.git(ctx, "remote", "set-url", "origin", url); err != nil {
			return nil, err
		}
		if _, err := r.git(ctx, "fetch", "--prune", "--quiet", "origin"); err != nil {
			return nil, err
		}
		return r, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, "", "clone", "--mirror", "--quiet", url, dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return r, nil
}

// RemoteHead returns the commit at HEAD of a remote repository without
// fetching it.
func RemoteHead(ctx context.Context, url string) (string, error) {
	out, err := runGit(ctx, "", "ls-remote", url, "HEAD")
	if err != nil {
		return "", err
	}
	commit, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if commit == "" {
		return "", fmt.Errorf("%s: no HEAD", url)
	}
	return commit, nil
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	sub := args[0]
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", sub, msg)
	}
	return out, nil
}

func (r *Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	return runGit(ctx, r.Dir, args...)
}

// Cmd returns a git command in the repository, for the caller to attach
// to the terminal.
func (r *Repo) Cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
}

// Head returns the commit at HEAD.
func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s: no commits", r.Dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// HasCommit reports whether the repository contains a commit.
func (r *Repo) HasCommit(ctx context.Context, commit string) bool {
	_, err := r.git(ctx, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) bool {
	_, err := r.git(ctx, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// File returns a file's content at a commit. ok is false if the file
// doesn't exist there.
func (r *Repo) File(ctx context.Context, commit, path string) (data []byte, ok bool, err error) {
	if _, err := r.git(ctx, "cat-file", "-e", commit+":"+path); err != nil {
		return nil, false, nil
	}
	out, err := r.git(ctx, "cat-file", "blob", commit+":"+path)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// Files lists every file at a commit.
func (r *Repo) Files(ctx context.Context, commit string) ([]string, error) {
	out, err := r.git(ctx, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return nil, err
	}
	return splitZ(out), nil
}

// Changed lists the files that differ between two commits.
func (r *Repo) Changed(ctx context.Context, from, to string) ([]string, error) {
	out, err := r.git(ctx, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", from, to)
	if err != nil {
		return nil, err
	}
	return splitZ(out), nil
}

func splitZ(b []byte) []string {
	var out []string
	for s := range strings.SplitSeq(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Authors lists the distinct "name <email>" authors of the commits a git
// log revision range selects, such as "abc..HEAD" or "abc".
func (r *Repo) Authors(ctx context.Context, rev string) ([]string, error) {
	out, err := r.git(ctx, "log", "--format=%an <%ae>", rev, "--")
	if err != nil {
		return nil, err
	}
	var authors []string
	seen := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\n")
		if line != "" && !seen[line] {
			seen[line] = true
			authors = append(authors, line)
		}
	}
	return authors, nil
}

// Line is one line of a file, numbered from 1.
type Line struct {
	Num  int
	Text string
}

// Diff is what changed in one file between two commits: added lines
// numbered in the new file, removed lines numbered in the old one.
type Diff struct {
	Added   []Line
	Removed []Line
}

// FileDiff returns the line changes to one file between two commits.
func (r *Repo) FileDiff(ctx context.Context, from, to, path string) (Diff, error) {
	out, err := r.git(ctx, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "-U0", from, to, "--", path)
	if err != nil {
		return Diff{}, err
	}
	return parseDiff(out)
}

func parseDiff(out []byte) (Diff, error) {
	var d Diff
	oldN, newN := 0, 0
	inHunk := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "@@ "):
			// @@ -a[,b] +c[,d] @@
			f := strings.Fields(line)
			if len(f) < 3 {
				return d, fmt.Errorf("bad hunk header %q", line)
			}
			var err error
			if oldN, err = hunkStart(f[1]); err != nil {
				return d, err
			}
			if newN, err = hunkStart(f[2]); err != nil {
				return d, err
			}
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(line, "+"):
			d.Added = append(d.Added, Line{newN, line[1:]})
			newN++
		case strings.HasPrefix(line, "-"):
			d.Removed = append(d.Removed, Line{oldN, line[1:]})
			oldN++
		case strings.HasPrefix(line, " "):
			oldN++
			newN++
		case strings.HasPrefix(line, "diff "):
			inHunk = false
		}
	}
	return d, sc.Err()
}

func hunkStart(s string) (int, error) {
	s = strings.TrimLeft(s, "-+")
	s, _, _ = strings.Cut(s, ",")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad hunk range %q", s)
	}
	// Only a zero-length range starts at 0, and it has no lines to number.
	return max(n, 1), nil
}

// DiffText returns the unified diff between two commits.
func (r *Repo) DiffText(ctx context.Context, from, to string) (string, error) {
	out, err := r.git(ctx, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", from, to)
	return string(out), err
}

// Export writes a clean copy of the tree at a commit into dst, which must
// not exist. It's a copy, not a checkout: nothing in dst points back at
// the repository.
func (r *Repo) Export(ctx context.Context, commit, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", r.Dir, "archive", "--format=tar", commit)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	xerr := untar(out, dst)
	io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %s", strings.TrimSpace(stderr.String()))
	}
	return xerr
}

func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive: unsafe path %q", h.Name)
		}
		path := filepath.Join(dst, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, path); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("archive: unsupported entry %q", h.Name)
		}
	}
}
