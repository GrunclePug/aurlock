package aur_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/grunclepug/aurlock/internal/aur"
	"github.com/grunclepug/aurlock/internal/testaur"
)

func TestInfo(t *testing.T) {
	fake := testaur.New(t)
	fake.SetInfo(testaur.Package{Name: "foo", Base: "foo", Version: "1-1", Maintainer: "me", FirstSubmitted: 100})
	fake.SetInfo(testaur.Package{Name: "foo-docs", Base: "foo", Version: "1-1"})
	got, err := aur.NewClient(fake.URL).Info(context.Background(), []string{"foo", "foo-docs", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["foo"].Maintainer != "me" || got["foo"].FirstSubmitted != 100 || got["foo-docs"].Maintainer != "" || got["foo-docs"].PackageBase != "foo" {
		t.Errorf("got %+v", got)
	}
}

func TestRepo(t *testing.T) {
	ctx := context.Background()
	fake := testaur.New(t)
	c1 := fake.Commit("foo", "A <a@example.org>", map[string]string{"PKGBUILD": "one\ntwo\nthree\n", ".SRCINFO": "x\n", "run.sh": "#!/bin/sh\n"})
	dir := filepath.Join(t.TempDir(), "repos", "foo.git")
	url := aur.RepoURL(fake.URL, "foo")

	repo, err := aur.Sync(ctx, dir, url)
	if err != nil {
		t.Fatal(err)
	}
	if head, _ := repo.Head(ctx); head != c1 {
		t.Errorf("head %s, want %s", head, c1)
	}
	c2 := fake.Commit("foo", "B <b@example.org>", map[string]string{"PKGBUILD": "one\n2\nthree\nfour\n", "run.sh": "", "new.txt": "n\n"})
	if remote, err := aur.RemoteHead(ctx, url); err != nil || remote != c2 {
		t.Errorf("remote head %s %v", remote, err)
	}
	if repo, err = aur.Sync(ctx, dir, url); err != nil {
		t.Fatal(err)
	}
	if head, _ := repo.Head(ctx); head != c2 {
		t.Errorf("head after fetch %s", head)
	}
	if !repo.HasCommit(ctx, c1) || repo.HasCommit(ctx, "0123456789012345678901234567890123456789") {
		t.Error("HasCommit")
	}
	if !repo.IsAncestor(ctx, c1, c2) || repo.IsAncestor(ctx, c2, c1) {
		t.Error("IsAncestor")
	}

	data, ok, err := repo.File(ctx, c1, "PKGBUILD")
	if err != nil || !ok || string(data) != "one\ntwo\nthree\n" {
		t.Errorf("File = %q %v %v", data, ok, err)
	}
	if _, ok, _ := repo.File(ctx, c2, "run.sh"); ok {
		t.Error("deleted file found")
	}
	files, _ := repo.Files(ctx, c2)
	if !slices.Equal(files, []string{".SRCINFO", "PKGBUILD", "new.txt"}) {
		t.Errorf("Files = %v", files)
	}
	changed, _ := repo.Changed(ctx, c1, c2)
	if !slices.Equal(changed, []string{"PKGBUILD", "new.txt", "run.sh"}) {
		t.Errorf("Changed = %v", changed)
	}
	d, err := repo.FileDiff(ctx, c1, c2, "PKGBUILD")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Added, []aur.Line{{Num: 2, Text: "2"}, {Num: 4, Text: "four"}}) || !slices.Equal(d.Removed, []aur.Line{{Num: 2, Text: "two"}}) {
		t.Errorf("diff = %+v", d)
	}
	if authors, _ := repo.Authors(ctx, c1+".."+c2); !slices.Equal(authors, []string{"B <b@example.org>"}) {
		t.Errorf("authors = %v", authors)
	}

	out := filepath.Join(t.TempDir(), "work")
	if err := repo.Export(ctx, c1, out); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(out, "run.sh")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("exported run.sh: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(out, ".git")); err == nil {
		t.Error("export links back to the repository")
	}
}
