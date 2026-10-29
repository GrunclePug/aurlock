package build

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grunclepug/aurlock/internal/testaur"
)

func testConfig(t *testing.T) Config {
	dir := t.TempDir()
	env := map[string]string{"LANG": "de_DE.UTF-8", "MAKEFLAGS": "-j8", "SSH_AUTH_SOCK": "/run/user/1000/ssh"}
	return Config{
		Dirs:   Dirs{Work: filepath.Join(dir, "build", "foo"), SrcDest: filepath.Join(dir, "src", "foo"), PkgDest: filepath.Join(dir, "pkg", "foo")},
		Getenv: func(k string) string { return env[k] },
		Stdout: io.Discard, Stderr: io.Discard,
	}
}

func TestArgs(t *testing.T) {
	cfg := testConfig(t)
	offline := strings.Join(Args(cfg, false), " ")
	online := strings.Join(Args(cfg, true), " ")
	for _, want := range []string{
		"--unshare-user", "--unshare-pid", "--disable-userns", "--die-with-parent", "--new-session",
		"--ro-bind /usr /usr", "--ro-bind /etc /etc", "--ro-bind /var/lib/pacman /var/lib/pacman",
		"--dev /dev", "--proc /proc", "--tmpfs /tmp",
		"--bind " + cfg.Work + " " + cfg.Work, "--bind " + cfg.SrcDest + " " + cfg.SrcDest, "--bind " + cfg.PkgDest + " " + cfg.PkgDest,
		"--chdir " + cfg.Work, "--clearenv",
		"--setenv HOME /tmp/home", "--setenv LANG de_DE.UTF-8", "--setenv MAKEFLAGS -j8",
		"--setenv BUILDDIR " + cfg.Work, "--setenv SRCDEST " + cfg.SrcDest, "--setenv PKGDEST " + cfg.PkgDest,
	} {
		if !strings.Contains(offline, want) {
			t.Errorf("args lack %q", want)
		}
	}
	if !strings.Contains(offline, "--unshare-net") || strings.Contains(online, "--unshare-net") {
		t.Error("network")
	}
	for _, bad := range []string{"SSH_AUTH_SOCK", "GNUPGHOME", os.Getenv("HOME") + " "} {
		if strings.Contains(offline, bad) {
			t.Errorf("args contain %q", bad)
		}
	}
	if !strings.HasSuffix(offline, " --") {
		t.Error("no -- at the end")
	}
	cfg.GnuPG = "/k"
	if a := strings.Join(Args(cfg, true), " "); !strings.Contains(a, "--bind /k /k") || !strings.Contains(a, "--setenv GNUPGHOME /k") {
		t.Error("keyring not mounted")
	}
}

func TestBuild(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PATH", testaur.Fakes(t, state)+":"+os.Getenv("PATH"))
	cfg := testConfig(t)
	if err := Reset(cfg.Dirs); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfg.Work, "PKGBUILD"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(cfg.Work, ".SRCINFO"), []byte("pkgbase = foo\n\tpkgver = 1.2\n\tpkgrel = 3\n\npkgname = foo\n\npkgname = foo-docs\n"), 0o644)
	ctx := context.Background()
	if err := Fetch(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := Build(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Packages(cfg.PkgDest)
	if err != nil || len(got) != 2 || got["foo-docs"] != filepath.Join(cfg.PkgDest, "foo-docs-1.2-3-x86_64.pkg.tar.zst") {
		t.Errorf("packages = %v %v", got, err)
	}
	makepkg, _ := os.ReadFile(filepath.Join(state, "makepkg"))
	if string(makepkg) != "makepkg --verifysource --noconfirm\nmakepkg --holdver --noconfirm\n" {
		t.Errorf("makepkg runs:\n%s", makepkg)
	}

	os.WriteFile(filepath.Join(state, "build-fails"), []byte("npm ERR! getaddrinfo EAI_AGAIN registry.npmjs.org\n"), 0o644)
	if err := Build(ctx, cfg); !errors.Is(err, ErrNeedsNetwork) {
		t.Errorf("offline network failure: %v", err)
	}
	cfg.Online = true
	if err := Build(ctx, cfg); err == nil || errors.Is(err, ErrNeedsNetwork) {
		t.Errorf("online failure: %v", err)
	}
	cfg.Online = false
	os.WriteFile(filepath.Join(state, "build-fails"), []byte("error: undefined reference to foo\n"), 0o644)
	if err := Build(ctx, cfg); err == nil || errors.Is(err, ErrNeedsNetwork) {
		t.Errorf("plain failure: %v", err)
	}
}

func TestPackages(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a-b-1:2.0-1-any.pkg.tar.zst", "a-b-1:2.0-1-any.pkg.tar.zst.sig", "c-1-1-x86_64.pkg.tar.xz", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	got, err := Packages(dir)
	if err != nil || len(got) != 2 || got["a-b"] == "" || got["c"] == "" {
		t.Errorf("got %v %v", got, err)
	}
}

func TestImportKeys(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PATH", testaur.Fakes(t, state)+":"+os.Getenv("PATH"))
	have := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	need := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	os.WriteFile(filepath.Join(state, "keys"), []byte(have+"\n"), 0o644)
	ctx := context.Background()
	if err := ImportKeys(ctx, "/kr", []string{have, need}, io.Discard); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(state, "log"))
	if !strings.Contains(string(log), "gpg --homedir /kr --batch --keyserver hkps://keyserver.ubuntu.com --recv-keys "+need+"\n") ||
		strings.Contains(string(log), "--recv-keys "+have) {
		t.Errorf("log:\n%s", log)
	}
	if err := ImportKeys(ctx, "/kr", []string{"DEADBEEF"}, io.Discard); err == nil {
		t.Error("short key id accepted")
	}
}
