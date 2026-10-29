// Package testaur is a fake AUR and fake system commands for tests. The
// AUR serves git over HTTP with git http-backend and the RPC from memory,
// both under one URL like the real one. The commands (pacman, sudo, bwrap,
// claude, gpg) record their arguments and never touch the system.
package testaur

import (
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type AUR struct {
	t    testing.TB
	Dir  string
	URL  string
	mu   sync.Mutex
	info map[string]map[string]any
}

// Isolate keeps git in tests away from the user's configuration.
func Isolate(t testing.TB) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func New(t testing.TB) *AUR {
	Isolate(t)
	a := &AUR{t: t, Dir: t.TempDir(), info: map[string]map[string]any{}}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rpc/v5/info", a.serveInfo)
	mux.Handle("/", &cgi.Handler{
		Path:       git,
		Args:       []string{"http-backend"},
		Env:        []string{"GIT_PROJECT_ROOT=" + filepath.Join(a.Dir, "repos"), "GIT_HTTP_EXPORT_ALL=1"},
		InheritEnv: []string{"PATH", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM"},
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.URL = srv.URL
	return a
}

func (a *AUR) serveInfo(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	results := []map[string]any{}
	for _, name := range r.URL.Query()["arg[]"] {
		if p, ok := a.info[name]; ok {
			results = append(results, p)
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"version": 5, "type": "multiinfo", "resultcount": len(results), "results": results})
}

type Package struct {
	Name, Base, Version string
	Maintainer          string // "" for orphaned
	FirstSubmitted      int64
}

func (a *AUR) SetInfo(p Package) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var m any
	if p.Maintainer != "" {
		m = p.Maintainer
	}
	a.info[p.Name] = map[string]any{
		"Name": p.Name, "PackageBase": p.Base, "Version": p.Version, "Maintainer": m,
		"FirstSubmitted": p.FirstSubmitted, "OutOfDate": nil,
	}
}

func (a *AUR) git(dir string, args ...string) string {
	a.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_NAME=aur", "GIT_COMMITTER_EMAIL=aur@aur.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		a.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Commit changes files of a package base (an empty content deletes the
// file), commits them as author ("Name <email>") and pushes. It returns
// the new commit.
func (a *AUR) Commit(base, author string, files map[string]string) string {
	a.t.Helper()
	work := filepath.Join(a.Dir, "work", base)
	bare := filepath.Join(a.Dir, "repos", base+".git")
	if _, err := os.Stat(bare); err != nil {
		os.MkdirAll(work, 0o755)
		a.git("", "init", "-q", "-b", "master", work)
		a.git("", "init", "-q", "--bare", "-b", "master", bare)
	}
	for name, content := range files {
		path := filepath.Join(work, name)
		if content == "" {
			os.Remove(path)
			continue
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			a.t.Fatal(err)
		}
	}
	a.git(work, "add", "-A")
	a.git(work, "commit", "-q", "--allow-empty", "--author", author, "-m", "update")
	a.git(work, "push", "-q", bare, "HEAD:master")
	return a.git(work, "rev-parse", "HEAD")
}

// Fakes writes the fake commands into a new directory and returns it, for
// the front of PATH. They log to state/log and keep pacman's database in
// state/installed ("name version" lines) and state/repo (one name per
// line). Build failures are simulated by state/fetch-fails or
// state/build-fails, whose content becomes the output.
func Fakes(t testing.TB, state string) string {
	dir := t.TempDir()
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKE_STATE", state)
	return dir
}

var scripts = map[string]string{
	"sudo": `#!/bin/sh
echo "sudo $*" >> "$FAKE_STATE/log"
exec "$@"
`,
	"pacman": `#!/bin/sh
s=$FAKE_STATE
echo "pacman $*" >> "$s/log"
touch "$s/installed" "$s/repo"
op=$1
shift
[ "$1" = -- ] && shift
remove() {
	grep -v "^$1 " "$s/installed" > "$s/installed.new"
	mv "$s/installed.new" "$s/installed"
}
case "$op" in
-Q)
	cat "$s/installed" ;;
-T)
	rc=0
	for d; do
		n=${d%%[<>=]*}
		grep -q "^$n " "$s/installed" || { echo "$d"; rc=127; }
	done
	exit $rc ;;
-Sp)
	for d; do :; done
	grep -qx "${d%%[<>=]*}" "$s/repo" || { echo "error: target not found: $d" >&2; exit 1; } ;;
-S)
	for a; do
		case "$a" in -*) ;; *) n=${a%%[<>=]*}; remove "$n"; echo "$n 1.0-1" >> "$s/installed" ;; esac
	done ;;
-U)
	for a; do
		case "$a" in -*) continue ;; esac
		f=$(basename "$a" | sed 's/\.pkg\.tar.*//')
		n=$(echo "$f" | sed -E 's/-[^-]+-[^-]+-[^-]+$//')
		v=$(echo "$f" | sed -E 's/^.*-([^-]+-[^-]+)-[^-]+$/\1/')
		remove "$n"
		echo "$n $v" >> "$s/installed"
	done ;;
-R*)
	for a; do
		case "$a" in -*) ;; *) remove "$a" ;; esac
	done ;;
-D) ;;
*)
	echo "fake pacman: unsupported $op" >&2
	exit 1 ;;
esac
`,
	"bwrap": `#!/bin/sh
s=$FAKE_STATE
echo "bwrap $*" >> "$s/log"
while [ $# -gt 0 ]; do
	case "$1" in
	--setenv) [ "$2" = PKGDEST ] && pkgdest=$3; shift 3; continue ;;
	--chdir) work=$2; shift 2; continue ;;
	--) shift; break ;;
	esac
	shift
done
echo "$*" >> "$s/makepkg"
case " $* " in
*" --verifysource "*)
	[ -f "$s/fetch-fails" ] && { cat "$s/fetch-fails" >&2; exit 1; }
	exit 0 ;;
esac
[ -f "$s/build-fails" ] && { cat "$s/build-fails" >&2; exit 4; }
cd "$work" || exit 1
[ -f PKGBUILD ] || { echo "no PKGBUILD in $work" >&2; exit 1; }
v=$(awk -F' = ' '/^\tpkgver = /{v=$2} /^\tpkgrel = /{r=$2} END{print v "-" r}' .SRCINFO)
for n in $(awk -F' = ' '/^pkgname = /{print $2}' .SRCINFO); do
	: > "$pkgdest/$n-$v-x86_64.pkg.tar.zst"
done
`,
	"claude": `#!/bin/sh
s=$FAKE_STATE
printf '%s\n' "$@" > "$s/claude-args"
pwd > "$s/claude-dir"
cat > "$s/claude-prompt"
echo "Malicious likelihood: low"
echo "Recommendation: approve"
`,
	"gpg": `#!/bin/sh
s=$FAKE_STATE
echo "gpg $*" >> "$s/log"
case " $* " in
*" --list-keys "*)
	for k; do :; done
	grep -qx "$k" "$s/keys" 2>/dev/null ;;
*" --recv-keys "*)
	seen=
	for k; do
		[ -n "$seen" ] && echo "$k" >> "$s/keys"
		[ "$k" = --recv-keys ] && seen=1
	done
	exit 0 ;;
esac
`,
	"gpgconf": `#!/bin/sh
echo "gpgconf $*" >> "$FAKE_STATE/log"
`,
}
