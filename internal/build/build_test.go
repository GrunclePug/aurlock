package build

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		"--setenv FAKEROOTDONTTRYCHOWN 1",
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

func TestArgsRustup(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	other := filepath.Join(dir, "rust up")
	file := filepath.Join(dir, "file")
	for _, d := range []string{filepath.Join(home, ".rustup"), other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(file, nil, 0o644)
	for _, tc := range []struct {
		name  string
		env   map[string]string
		mount string // source mounted at /tmp/home/.rustup, "" for none
		err   bool
	}{
		{"default", map[string]string{"HOME": home}, filepath.Join(home, ".rustup"), false},
		{"explicit", map[string]string{"HOME": home, "RUSTUP_HOME": other}, other, false},
		{"relative", map[string]string{"HOME": home, "RUSTUP_HOME": "rustup"}, "", true},
		{"file", map[string]string{"HOME": home, "RUSTUP_HOME": file}, "", true},
		{"missing explicit", map[string]string{"HOME": home, "RUSTUP_HOME": filepath.Join(dir, "nope")}, "", true},
		{"no rustup", map[string]string{"HOME": dir}, "", false},
		{"relative HOME", map[string]string{"HOME": "home"}, "", false},
		{"no HOME", map[string]string{}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if _, err := rustupHome(getenv); (err != nil) != tc.err {
				t.Errorf("rustupHome error = %v, want error %v", err, tc.err)
			}
			cfg := testConfig(t)
			cfg.Getenv = getenv
			a := Args(cfg, false)
			bind := slices.Index(a, "/tmp/home/.rustup")
			env := slices.Index(a, "RUSTUP_HOME")
			if tc.mount == "" {
				if bind >= 0 || env >= 0 || slices.Contains(a, "RUSTUP_AUTO_INSTALL") {
					t.Errorf("rustup mounted: %q", a)
				}
				return
			}
			if bind < 2 || a[bind-2] != "--ro-bind" || a[bind-1] != tc.mount {
				t.Fatalf("no --ro-bind %s /tmp/home/.rustup in %q", tc.mount, a)
			}
			if tmpfs := slices.Index(a, "--tmpfs"); tmpfs < 0 || bind < tmpfs {
				t.Error("rustup bind before --tmpfs /tmp")
			}
			clear := slices.Index(a, "--clearenv")
			if env < clear || a[env-1] != "--setenv" || a[env+1] != "/tmp/home/.rustup" {
				t.Errorf("RUSTUP_HOME not set after --clearenv: %q", a)
			}
			if i := slices.Index(a, "RUSTUP_AUTO_INSTALL"); i < clear || a[i+1] != "0" {
				t.Errorf("RUSTUP_AUTO_INSTALL not 0: %q", a)
			}
		})
	}

	cfg := testConfig(t)
	cfg.Getenv = func(k string) string { return map[string]string{"RUSTUP_TOOLCHAIN": "stable"}[k] }
	if a := strings.Join(Args(cfg, false), " "); !strings.Contains(a, "--setenv RUSTUP_TOOLCHAIN stable") {
		t.Error("RUSTUP_TOOLCHAIN not passed through")
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

	for _, out := range []string{
		"npm ERR! getaddrinfo EAI_AGAIN registry.npmjs.org\n",
		"  [6] Could not resolve hostname (Could not resolve host: index.crates.io)\n",
	} {
		os.WriteFile(filepath.Join(state, "build-fails"), []byte(out), 0o644)
		if err := Build(ctx, cfg); !errors.Is(err, ErrNeedsNetwork) {
			t.Errorf("offline network failure %q: %v", out, err)
		}
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

	var stderr strings.Builder
	cfg.Stderr = &stderr
	cfg.Getenv = func(k string) string { return map[string]string{"RUSTUP_HOME": "rustup"}[k] }
	if err := Fetch(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), `aurlock: RUSTUP_HOME "rustup" isn't an absolute path to a directory: not mounted into the build`) {
		t.Errorf("no warning for a bad RUSTUP_HOME: %q", stderr.String())
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

func TestWriteTree(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"PKGBUILD": []byte("x\n"), "sub/run.sh": []byte("#!/bin/sh\n"), "link": []byte("PKGBUILD")}
	modes := map[string]string{"PKGBUILD": "100644", "sub/run.sh": "100755", "link": "120000"}
	if err := WriteTree(dir, files, modes); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "sub", "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != "PKGBUILD" {
		t.Errorf("link: %q %v", target, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "PKGBUILD")); string(data) != "x\n" {
		t.Errorf("PKGBUILD = %q", data)
	}
	for _, bad := range []string{"../escape", "/etc/passwd"} {
		if err := WriteTree(t.TempDir(), map[string][]byte{bad: nil}, nil); err == nil {
			t.Errorf("wrote %q", bad)
		}
	}
}

// In the real sandbox, package() may give files to root under fakeroot,
// with chown, install -o or cp -a, although only the user is mapped.
func TestSandboxChownUnderFakeroot(t *testing.T) {
	for _, tool := range []string{"bwrap", "fakeroot"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	cfg := testConfig(t)
	for _, d := range []string{cfg.Work, cfg.SrcDest, cfg.PkgDest} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec.Command("bwrap", append(Args(cfg, false), "true")...).Run(); err != nil {
		t.Skipf("bwrap can't create a sandbox here: %v", err)
	}
	script := "mkdir -p src/d && echo x > src/d/f && chown root:root src/d/f && install -o root -g root -m644 src/d/f g && cp -a src out && stat -c %u:%g out/d out/d/f g"
	out, err := exec.Command("bwrap", append(Args(cfg, false), "fakeroot", "--", "sh", "-c", script)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.Fields(string(out)); strings.Join(got, " ") != "0:0 0:0 0:0" {
		t.Errorf("owners under fakeroot = %q, want root for all", out)
	}
}

func TestCopyKeyring(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for name, data := range map[string]string{"pubring.kbx": "keys", "trustdb.gpg": "trust", "gpg.conf": "dirmngr-program /x", "dirmngr.conf": "x"} {
		os.WriteFile(filepath.Join(src, name), []byte(data), 0o644)
	}
	if err := CopyKeyring(src, dst); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dst)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, " ") != "pubring.kbx trustdb.gpg" {
		t.Errorf("copied %v", got)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "pubring.kbx")); string(data) != "keys" {
		t.Errorf("pubring.kbx = %q", data)
	}
	// Something already in dst, such as a planted symlink, isn't written through.
	target := filepath.Join(t.TempDir(), "approved")
	os.WriteFile(target, []byte("approvals"), 0o644)
	planted := t.TempDir()
	os.Symlink(target, filepath.Join(planted, "pubring.kbx"))
	if err := CopyKeyring(src, planted); err == nil {
		t.Error("copied through a symlink")
	}
	if data, _ := os.ReadFile(target); string(data) != "approvals" {
		t.Errorf("symlink target = %q", data)
	}
	// A keyring without a trust database yet.
	os.Remove(filepath.Join(src, "trustdb.gpg"))
	if err := CopyKeyring(src, t.TempDir()); err != nil {
		t.Error(err)
	}
}

func TestResetEmptiesKeyring(t *testing.T) {
	cfg := testConfig(t)
	cfg.GnuPG = filepath.Join(t.TempDir(), "gnupg-build", "foo")
	os.MkdirAll(cfg.GnuPG, 0o700)
	os.WriteFile(filepath.Join(cfg.GnuPG, "gpg.conf"), []byte("planted"), 0o600)
	if err := Reset(cfg.Dirs); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(cfg.GnuPG); err != nil || len(entries) != 0 {
		t.Errorf("keyring dir after Reset: %v %v", entries, err)
	}
}
