// Package store reads and writes aurlock's plain-text record files: the
// approvals, shared between machines, and the per-machine install log.
//
// Both are one whitespace-separated record per line, keyed by the first
// field, with '#' comments. Rewrites keep comments and the order of
// untouched lines, and replace the file atomically in the directory of
// its real path, so a symlink into a synced folder stays a symlink.
package store

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Table is a record file in memory.
type Table struct {
	header string
	fields int
	lines  []string
}

// load reads a table, checking that every record has n fields. A missing
// file is an empty table.
func load(path, header string, n int, valid func([]string) error) (*Table, error) {
	t := &Table{header: header, fields: n}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for i := 1; sc.Scan(); i++ {
		line := sc.Text()
		if f := record(line); f != nil {
			if len(f) != n {
				return nil, fmt.Errorf("%s:%d: %d fields, want %d", path, i, len(f), n)
			}
			if err := valid(f); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, i, err)
			}
		}
		t.lines = append(t.lines, line)
	}
	return t, sc.Err()
}

// record splits a line into fields, or returns nil for a blank line or a
// comment.
func record(line string) []string {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(line)
	if len(f) == 0 {
		return nil
	}
	return f
}

// Records returns every record, in file order.
func (t *Table) Records() [][]string {
	var out [][]string
	for _, l := range t.lines {
		if f := record(l); f != nil {
			out = append(out, f)
		}
	}
	return out
}

// Get returns the record with this key.
func (t *Table) Get(key string) ([]string, bool) {
	for _, l := range t.lines {
		if f := record(l); f != nil && f[0] == key {
			return f, true
		}
	}
	return nil, false
}

// Set replaces the record with the same key, or appends it.
func (t *Table) Set(f []string) {
	line := strings.Join(f, " ")
	for i, l := range t.lines {
		if r := record(l); r != nil && r[0] == f[0] {
			t.lines[i] = line
			return
		}
	}
	if len(t.lines) == 0 && t.header != "" {
		t.lines = append(t.lines, t.header)
	}
	t.lines = append(t.lines, line)
}

// Delete removes the record with this key and reports whether it existed.
func (t *Table) Delete(key string) bool {
	for i, l := range t.lines {
		if r := record(l); r != nil && r[0] == key {
			t.lines = append(t.lines[:i], t.lines[i+1:]...)
			return true
		}
	}
	return false
}

// save writes the table atomically to the real path behind path.
func (t *Table) save(path string) error {
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Either no file yet, or a dangling link: write where the link
		// points, so it stops dangling.
		real = path
		if target, lerr := os.Readlink(path); lerr == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			real = target
		}
	} else if err != nil {
		return err
	}
	dir := filepath.Dir(real)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(real)+".aurlock-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	var buf bytes.Buffer
	for _, l := range t.lines {
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), real)
}

// ConflictFiles lists the Syncthing conflict copies next to the real file
// behind path, such as approved.sync-conflict-20261029-101500-ABCDEFG.
func ConflictFiles(path string) ([]string, error) {
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		real = path
	} else if err != nil {
		return nil, err
	}
	base := filepath.Base(real)
	ext := filepath.Ext(base)
	pattern := strings.TrimSuffix(base, ext) + ".sync-conflict-*" + ext
	return filepath.Glob(filepath.Join(filepath.Dir(real), pattern))
}

var (
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dateRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	nameRe   = regexp.MustCompile(`^[a-z0-9@_+][a-z0-9@._+-]*$`)
)

// ValidName reports whether s is a valid AUR package or package base name.
func ValidName(s string) bool {
	return len(s) <= 255 && nameRe.MatchString(s)
}

// ValidCommit reports whether s is a full lowercase SHA-1 commit id.
func ValidCommit(s string) bool {
	return commitRe.MatchString(s)
}
