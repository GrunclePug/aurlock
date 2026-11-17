// Package build runs makepkg on an approved package inside bubblewrap: a
// download step with network, then an offline build. The system is
// read-only, $HOME isn't mounted apart from rustup's toolchains
// (read-only), and only the package's own build, source and output
// directories are writable.
package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Dirs are the directories one package's build may write.
type Dirs struct {
	Work    string // clean copy of the approved commit; makepkg's startdir
	SrcDest string // downloaded sources, kept between builds
	PkgDest string // built packages, emptied before each build
	// GnuPG is this build's copy of aurlock's public keyring (CopyKeyring),
	// "" if not needed. The sandbox can write it, so it's never the keyring
	// itself: gpg runs outside the sandbox on that one.
	GnuPG string
}

type Config struct {
	Dirs
	Online bool // the build step keeps network access
	Getenv func(string) string
	Stdout io.Writer
	Stderr io.Writer
}

// home is $HOME inside the sandbox: a directory on its tmpfs, empty but
// for the rustup home mounted at rustupDest.
const home = "/tmp/home"

const rustupDest = home + "/.rustup"

// rustupHome returns the rustup home to mount into the sandbox, "" if
// there's none. rustup's /usr/bin proxies (rustc, cargo) need it to find a
// toolchain. An explicit RUSTUP_HOME that isn't an absolute directory is an
// error; it never falls back to ~/.rustup.
func rustupHome(getenv func(string) string) (string, error) {
	if dir := getenv("RUSTUP_HOME"); dir != "" {
		if fi, err := os.Stat(dir); !filepath.IsAbs(dir) || err != nil || !fi.IsDir() {
			return "", fmt.Errorf("RUSTUP_HOME %q isn't an absolute path to a directory", dir)
		}
		return dir, nil
	}
	h := getenv("HOME")
	if !filepath.IsAbs(h) {
		return "", nil
	}
	dir := filepath.Join(h, ".rustup")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", nil
	}
	return dir, nil
}

// Args returns the bwrap arguments for a makepkg run, up to and including
// the "--" before the command.
func Args(cfg Config, network bool) []string {
	a := []string{
		"--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-uts", "--unshare-cgroup-try",
		"--disable-userns", "--die-with-parent", "--new-session",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/bin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib", "/lib64",
		"--ro-bind", "/etc", "/etc",
		"--ro-bind-try", "/opt", "/opt",
		// makepkg checks installed dependencies with pacman.
		"--ro-bind", "/var/lib/pacman", "/var/lib/pacman",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--dir", home,
	}
	if network {
		// /etc/resolv.conf is often a link into /run.
		if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && !strings.HasPrefix(real, "/etc/") {
			a = append(a, "--ro-bind-try", filepath.Dir(real), filepath.Dir(real))
		}
	} else {
		a = append(a, "--unshare-net")
	}
	for _, d := range []string{cfg.Work, cfg.SrcDest, cfg.PkgDest, cfg.GnuPG} {
		if d != "" {
			a = append(a, "--bind", d, d)
		}
	}
	// Read-only, and with auto-install off: a build can't add toolchains or
	// change rustup's settings, and a missing toolchain fails at once.
	rustup, _ := rustupHome(cfg.Getenv)
	if rustup != "" {
		a = append(a, "--ro-bind", rustup, rustupDest)
	}
	a = append(a, "--chdir", cfg.Work, "--clearenv")
	set := func(k, v string) { a = append(a, "--setenv", k, v) }
	set("PATH", "/usr/local/sbin:/usr/local/bin:/usr/bin")
	set("HOME", home)
	lang := cfg.Getenv("LANG")
	if lang == "" {
		lang = "C.UTF-8"
	}
	set("LANG", lang)
	for _, k := range []string{"TERM", "MAKEFLAGS", "RUSTUP_TOOLCHAIN"} {
		if v := cfg.Getenv(k); v != "" {
			set(k, v)
		}
	}
	set("BUILDDIR", cfg.Work)
	set("SRCDEST", cfg.SrcDest)
	set("PKGDEST", cfg.PkgDest)
	// Only the user's own ID is mapped, so a real chown to root fails with
	// EINVAL, which fakeroot passes on (it ignores only EPERM): cp -a or
	// chown root:root in package() would fail. fakeroot records the owner
	// either way, and the record is what goes into the package.
	set("FAKEROOTDONTTRYCHOWN", "1")
	if rustup != "" {
		set("RUSTUP_HOME", rustupDest)
		set("RUSTUP_AUTO_INSTALL", "0")
	}
	if cfg.GnuPG != "" {
		set("GNUPGHOME", cfg.GnuPG)
	}
	return append(a, "--")
}

// Fetch downloads and verifies the sources, with network. makepkg runs
// the PKGBUILD here, so only call it for an approved commit.
func Fetch(ctx context.Context, cfg Config) error {
	if _, err := rustupHome(cfg.Getenv); err != nil {
		fmt.Fprintf(cfg.Stderr, "aurlock: %v: not mounted into the build\n", err)
	}
	args := append(Args(cfg, true), "makepkg", "--verifysource", "--noconfirm")
	_, err := run(ctx, cfg, args)
	if err != nil {
		return fmt.Errorf("download sources: %w", err)
	}
	return nil
}

// ErrNeedsNetwork matches a *NeedsNetwork with errors.Is.
var ErrNeedsNetwork = errors.New("the build may need network access")

// NeedsNetwork is a failed offline build whose output looks like a network
// error. Log is the end of that output, written by the build: untrusted.
type NeedsNetwork struct{ Log string }

func (e *NeedsNetwork) Error() string        { return ErrNeedsNetwork.Error() }
func (e *NeedsNetwork) Is(target error) bool { return target == ErrNeedsNetwork }

// logLines and logBytes bound NeedsNetwork.Log.
const (
	logLines = 60
	logBytes = 8 << 10
)

// lastLines returns the last n lines of out, at most limit bytes of them.
func lastLines(out []byte, n, limit int) string {
	lines := strings.Split(strings.TrimRight(strings.ToValidUTF8(string(out), "?"), "\n"), "\n")
	s := strings.Join(lines[max(0, len(lines)-n):], "\n")
	if cut := len(s) - limit + 1; cut > 0 { // the final newline counts too
		// Drop the line the cut lands in, unless it starts there.
		if i := strings.IndexByte(s[cut-1:], '\n'); i >= 0 {
			s = s[cut+i:]
		} else {
			s = ""
		}
	}
	return s + "\n"
}

// Build builds the packages into PkgDest, offline unless cfg.Online.
func Build(ctx context.Context, cfg Config) error {
	// --holdver: VCS sources were fetched in the download step, so don't
	// try to update them.
	args := append(Args(cfg, cfg.Online), "makepkg", "--holdver", "--noconfirm")
	out, err := run(ctx, cfg, args)
	if err != nil {
		if !cfg.Online && networkError.Match(out) {
			return fmt.Errorf("build: %w: %w", err, &NeedsNetwork{Log: lastLines(out, logLines, logBytes)})
		}
		return fmt.Errorf("build: %w", err)
	}
	return nil
}

var networkError = regexp.MustCompile(`(?i)could not resolve|temporary failure in name resolution|name or service not known|network is unreachable|no route to host|failed to connect|getaddrinfo|eai_again|enotfound|failed to lookup address|dial tcp|connection refused`)

// run runs bwrap with the user seeing the output, and returns the tail of
// that output.
func run(ctx context.Context, cfg Config, args []string) ([]byte, error) {
	tail := &tailBuffer{max: 64 << 10}
	cmd := exec.CommandContext(ctx, "bwrap", args...)
	cmd.Stdout = io.MultiWriter(cfg.Stdout, tail)
	cmd.Stderr = io.MultiWriter(cfg.Stderr, tail)
	err := cmd.Run()
	return tail.bytes(), err
}

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.buf)
}

// Reset empties the work and package directories and creates every
// directory of dirs.
func Reset(d Dirs) error {
	for _, dir := range []string{d.Work, d.PkgDest} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	for _, dir := range []string{d.Work, d.SrcDest, d.PkgDest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if d.GnuPG != "" {
		// Fresh each build: nothing an earlier build wrote here, such as a
		// gpg.conf, reaches this one.
		if err := os.RemoveAll(d.GnuPG); err != nil {
			return err
		}
		return os.MkdirAll(d.GnuPG, 0o700)
	}
	return nil
}

// keyringFiles are what gpg needs to verify signatures: the public keys and
// the trust database. Config files (gpg.conf, dirmngr.conf, ...) aren't
// copied.
var keyringFiles = []string{"pubring.kbx", "trustdb.gpg"}

// CopyKeyring copies the keyring at src into dst, a build's GnuPG
// directory. A build gets a copy because it can write to its keyring
// directory, and gpg reads options from there: a shared keyring would let
// one build plant a gpg.conf that the next ImportKeys runs outside the
// sandbox.
func CopyKeyring(src, dst string) error {
	for _, name := range keyringFiles {
		data, err := os.ReadFile(filepath.Join(src, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := writeNew(filepath.Join(dst, name), data); err != nil {
			return err
		}
	}
	return nil
}

// writeNew writes a file that mustn't exist yet: dst was just emptied, so
// anything there, such as a symlink a concurrent build of the same package
// planted, is refused rather than written through.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// WriteTree writes files into dir byte for byte, so the build sees exactly
// what was reviewed. Symlinks come last, so nothing is written through one.
func WriteTree(dir string, files map[string][]byte, modes map[string]string) error {
	var links []string
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe path %q", name)
		}
		if modes[name] == "120000" {
			links = append(links, name)
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if modes[name] == "100755" {
			perm = 0o755
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err != nil {
			return err
		}
		_, err = f.Write(files[name])
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	for _, name := range links {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(string(files[name]), path); err != nil {
			return err
		}
	}
	return nil
}

// Packages maps package names to the package files makepkg wrote to dir.
func Packages(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		i := strings.Index(name, ".pkg.tar")
		if i < 0 || strings.HasSuffix(name, ".sig") || e.IsDir() {
			continue
		}
		// name-pkgver-pkgrel-arch.pkg.tar.zst
		f := strings.Split(name[:i], "-")
		if len(f) < 4 {
			continue
		}
		out[strings.Join(f[:len(f)-3], "-")] = filepath.Join(dir, name)
	}
	return out, nil
}

var fingerprintRe = regexp.MustCompile(`^(?:[0-9A-Fa-f]{40}|[0-9A-Fa-f]{64})$`)

// ImportKeys makes sure the keyring at dir has every key in fingerprints,
// fetching missing ones from the keyserver. The fingerprints come from
// validpgpkeys of an approved package, which pins exact keys, so a key
// fetched here can't make a forged signature pass.
func ImportKeys(ctx context.Context, dir string, fingerprints []string, stderr io.Writer) error {
	var missing []string
	for _, fp := range fingerprints {
		if !fingerprintRe.MatchString(fp) {
			return fmt.Errorf("validpgpkeys: %q isn't a full fingerprint", fp)
		}
		if exec.CommandContext(ctx, "gpg", "--homedir", dir, "--batch", "--list-keys", fp).Run() != nil {
			missing = append(missing, fp)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	defer exec.Command("gpgconf", "--homedir", dir, "--kill", "all").Run()
	args := append([]string{"--homedir", dir, "--batch", "--keyserver", "hkps://keyserver.ubuntu.com", "--recv-keys"}, missing...)
	cmd := exec.CommandContext(ctx, "gpg", args...)
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fetch PGP keys %s: %w", strings.Join(missing, " "), err)
	}
	return nil
}
