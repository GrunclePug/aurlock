package aur

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Repo is a bare mirror of one package base's AUR repository: fetching it
// never checks anything out or runs hooks.
type Repo struct {
	Dir string
}

// RepoURL is the git URL of a package base.
func RepoURL(baseURL, pkgbase string) string {
	return baseURL + "/" + pkgbase + ".git"
}

// Sync clones url into dir, or fetches it if dir already holds a clone.
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

// RemoteHead returns the commit at a remote's HEAD without fetching.
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

// Cmd returns a git command for the caller to attach to the terminal. It
// has no context: an interactive pager must not be killed mid-screen.
func (r *Repo) Cmd(args ...string) *exec.Cmd {
	return exec.Command("git", append([]string{"-C", r.Dir}, args...)...)
}

func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s: no commits", r.Dir)
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) HasCommit(ctx context.Context, commit string) bool {
	_, err := r.git(ctx, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// IsAncestor reports whether a is an ancestor of, or equal to, b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) bool {
	_, err := r.git(ctx, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Blob returns the content of a blob by its object id, unconverted.
func (r *Repo) Blob(ctx context.Context, id string) ([]byte, error) {
	return r.git(ctx, "cat-file", "blob", id)
}

// Entry is one file in a tree.
type Entry struct {
	Path string
	Mode string // git mode: 100644, 100755 or 120000 for a symlink
	ID   string // blob id
}

// Tree lists every file at a commit. Submodules are left out.
func (r *Repo) Tree(ctx context.Context, commit string) ([]Entry, error) {
	out, err := r.git(ctx, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, rec := range splitZ(out) {
		// <mode> SP <type> SP <id> TAB <path>
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("ls-tree: bad entry %q", rec)
		}
		if f[1] == "blob" {
			entries = append(entries, Entry{Path: path, Mode: f[0], ID: f[2]})
		}
	}
	return entries, nil
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

// Authors lists the distinct "name <email>" authors of the commits a
// revision range selects, such as "abc..HEAD" or "abc".
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

// Diff is the change to one file: added lines numbered in the new file,
// removed lines in the old one.
type Diff struct {
	Added   []Line
	Removed []Line
}

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
	// Only an empty range starts at 0, and it has no lines to number.
	return max(n, 1), nil
}

func (r *Repo) DiffText(ctx context.Context, from, to string) (string, error) {
	out, err := r.git(ctx, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", from, to)
	return string(out), err
}
