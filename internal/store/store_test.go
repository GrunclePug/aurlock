package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const commitA = "3f2a000000000000000000000000000000000c91"
const commitB = "b000000000000000000000000000000000000001"

func TestApprovalsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved")
	a, err := LoadApprovals(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Set(Approval{PkgBase: "solidcraft-bin", Commit: commitA, Version: "0.25.1-1", Maintainer: "FrogSnot", Date: "2026-10-29"})
	a.Set(Approval{PkgBase: "abc", Commit: commitB, Version: "1-1", Date: "2026-10-29", Online: true})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := approvedHeader + "\n" +
		"solidcraft-bin " + commitA + " 0.25.1-1 FrogSnot 2026-10-29 offline\n" +
		"abc " + commitB + " 1-1 - 2026-10-29 online\n"
	if string(data) != want {
		t.Errorf("file =\n%s\nwant\n%s", data, want)
	}
	b, err := LoadApprovals(path)
	if err != nil {
		t.Fatal(err)
	}
	list := b.List()
	if len(list) != 2 || list[0].PkgBase != "abc" || !list[0].Online || list[0].Maintainer != "-" {
		t.Errorf("list = %+v", list)
	}
}

func TestApprovalsKeepComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved")
	in := "# mine\nfoo " + commitA + " 1-1 me 2026-01-01 online # note\n\nbar " + commitA + " 2-1 me 2026-01-01 offline\n"
	os.WriteFile(path, []byte(in), 0o644)
	a, err := LoadApprovals(path)
	if err != nil {
		t.Fatal(err)
	}
	// A new commit resets the network grant to offline.
	a.Set(Approval{PkgBase: "foo", Commit: commitB, Version: "1-2", Maintainer: "me", Date: "2026-02-01"})
	a.Delete("bar")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := "# mine\nfoo " + commitB + " 1-2 me 2026-02-01 offline\n\n"
	if string(data) != want {
		t.Errorf("file =\n%q\nwant\n%q", data, want)
	}
}

func TestApprovalsInvalid(t *testing.T) {
	for _, line := range []string{
		"foo " + commitA + " 1-1 me 2026-01-01",
		"foo abc 1-1 me 2026-01-01 offline",
		"foo " + commitA + " 1-1 me yesterday offline",
		"foo " + commitA + " 1-1 me 2026-01-01 maybe",
		"../foo " + commitA + " 1-1 me 2026-01-01 offline",
	} {
		path := filepath.Join(t.TempDir(), "approved")
		os.WriteFile(path, []byte(line+"\n"), 0o644)
		if _, err := LoadApprovals(path); err == nil {
			t.Errorf("LoadApprovals accepted %q", line)
		}
	}
}

func TestSaveThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	sync := filepath.Join(dir, "Sync", "aurlock")
	os.MkdirAll(sync, 0o755)
	real := filepath.Join(sync, "approved")
	os.WriteFile(real, []byte("# synced\n"), 0o644)
	cfg := filepath.Join(dir, "config", "aurlock")
	os.MkdirAll(cfg, 0o755)
	link := filepath.Join(cfg, "approved")
	if err := os.Symlink("../../Sync/aurlock/approved", link); err != nil {
		t.Fatal(err)
	}
	a, err := LoadApprovals(link)
	if err != nil {
		t.Fatal(err)
	}
	a.Set(Approval{PkgBase: "foo", Commit: commitA, Version: "1-1", Maintainer: "me", Date: "2026-01-01"})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", fi.Mode(), err)
	}
	data, _ := os.ReadFile(real)
	if !strings.HasPrefix(string(data), "# synced\nfoo ") {
		t.Errorf("real file = %q", data)
	}
	// No temporary files left behind in the synced folder.
	entries, _ := os.ReadDir(sync)
	if len(entries) != 1 {
		t.Errorf("synced folder has %d entries", len(entries))
	}
}

func TestSaveDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "approved")
	os.Symlink(filepath.Join(dir, "sync", "approved"), link)
	os.MkdirAll(filepath.Join(dir, "sync"), 0o755)
	a, _ := LoadApprovals(link)
	a.Set(Approval{PkgBase: "foo", Commit: commitA, Version: "1-1", Maintainer: "me", Date: "2026-01-01"})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "sync", "approved")); err != nil {
		t.Error(err)
	}
}

func TestConflictFiles(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "approved")
	os.WriteFile(real, nil, 0o644)
	link := filepath.Join(dir, "link")
	os.Symlink(real, link)
	if got, _ := ConflictFiles(link); len(got) != 0 {
		t.Errorf("conflicts = %v", got)
	}
	c := filepath.Join(dir, "approved.sync-conflict-20261029-101500-ABCDEFG")
	os.WriteFile(c, nil, 0o644)
	got, err := ConflictFiles(link)
	if err != nil || len(got) != 1 || got[0] != c {
		t.Errorf("conflicts = %v, %v", got, err)
	}
}

func TestInstalled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "installed")
	l, err := LoadInstalled(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Set(Installed{PkgName: "foo-docs", PkgBase: "foo", Commit: commitA, Version: "1-1", Date: "2026-01-01"})
	l.Set(Installed{PkgName: "foo", PkgBase: "foo", Commit: commitA, Version: "1-1", Date: "2026-01-01"})
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	l, err = LoadInstalled(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.List(); len(got) != 2 || got[0].PkgName != "foo" {
		t.Errorf("list = %+v", got)
	}
	if !l.Delete("foo") || l.Delete("foo") {
		t.Error("Delete")
	}
}
