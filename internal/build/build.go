// Package build runs makepkg on an approved package inside bubblewrap: a
// download step with network, then an offline build. The system is
// read-only, $HOME isn't mounted, and only the package's own build,
// source and output directories are writable.
package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Dirs are the directories one package's build may write.
type Dirs struct {
	Work    string // clean copy of the approved commit; makepkg's startdir
	SrcDest string // downloaded sources, kept between builds
	PkgDest string // built packages, emptied before each build
	GnuPG   string // aurlock's public keyring, "" if not needed
}

// Config is one build.
type Config struct {
	Dirs
	Online bool // the build step keeps network access
	Getenv func(string) string
	Stdout io.Writer
	Stderr io.Writer
}

// home is $HOME inside the sandbox: an empty directory on its tmpfs.
const home = "/tmp/home"

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
	a = append(a, "--chdir", cfg.Work, "--clearenv")
	set := func(k, v string) { a = append(a, "--setenv", k, v) }
	set("PATH", "/usr/local/sbin:/usr/local/bin:/usr/bin")
	set("HOME", home)
	lang := cfg.Getenv("LANG")
	if lang == "" {
		lang = "C.UTF-8"
	}
	set("LANG", lang)
	for _, k := range []string{"TERM", "MAKEFLAGS"} {
		if v := cfg.Getenv(k); v != "" {
			set(k, v)
		}
	}
	set("BUILDDIR", cfg.Work)
	set("SRCDEST", cfg.SrcDest)
	set("PKGDEST", cfg.PkgDest)
	if cfg.GnuPG != "" {
		set("GNUPGHOME", cfg.GnuPG)
	}
	return append(a, "--")
}

// Fetch downloads and verifies the sources, with network.
func Fetch(ctx context.Context, cfg Config) error {
	args := append(Args(cfg, true), "makepkg", "--verifysource", "--noconfirm")
	_, err := run(ctx, cfg, args)
	if err != nil {
		return fmt.Errorf("download sources: %w", err)
	}
	return nil
}

// ErrNeedsNetwork wraps a failed offline build whose output looks like a
// network error.
var ErrNeedsNetwork = errors.New("the build may need network access")

// Build builds the packages into PkgDest, offline unless cfg.Online.
func Build(ctx context.Context, cfg Config) error {
	// --holdver: VCS sources were fetched in the download step, so don't
	// try to update them.
	args := append(Args(cfg, cfg.Online), "makepkg", "--holdver", "--noconfirm")
	out, err := run(ctx, cfg, args)
	if err != nil {
		if !cfg.Online && networkError.Match(out) {
			return fmt.Errorf("build: %w: %w", err, ErrNeedsNetwork)
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
		return os.MkdirAll(d.GnuPG, 0o700)
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
