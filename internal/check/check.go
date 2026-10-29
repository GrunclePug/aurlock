// Package check computes aurlock's red flags: fixed rules over a package's
// .SRCINFO, raw PKGBUILD text, repository files, git history and AUR
// metadata. Any flag means approving takes typing the package name.
package check

import (
	"bytes"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/grunclepug/aurlock/internal/aur"
	"github.com/grunclepug/aurlock/internal/srcinfo"
)

// Group is how flags are shown: who changed, where it downloads from, and
// what runs.
type Group string

const (
	Who   Group = "who"
	Where Group = "where"
	What  Group = "what"
)

// Groups is the display order.
var Groups = []Group{Who, Where, What}

type Flag struct {
	Group Group
	Text  string
	At    string // "PKGBUILD:14", or "" if no single place
}

func (f Flag) String() string {
	if f.At == "" {
		return f.Text
	}
	return f.Text + " (" + f.At + ")"
}

// Snapshot is the package repository at one commit: what gets reviewed,
// and later exactly what gets built.
type Snapshot struct {
	SRCINFO *srcinfo.SRCINFO
	Files   map[string][]byte
	Modes   map[string]string // git modes of Files
}

type Input struct {
	Arch string
	Now  time.Time
	RPC  aur.Package

	// Update only: the approved snapshot and its recorded maintainer, the
	// changes since, and the commit authors new since the approval.
	Old        *Snapshot
	Maintainer string
	Diffs      map[string]aur.Diff // per changed file, approved → HEAD
	NewAuthors []string

	New     *Snapshot
	Network bool // approving with network during the build
}

// Update reports whether this reviews a change to an approved package.
func (in *Input) Update() bool {
	return in.Old != nil
}

type Result struct {
	Flags []Flag
	Notes []string
}

func Run(in Input) Result {
	var r Result
	pb := scan(string(in.New.Files["PKGBUILD"]))
	who(&in, &r)
	where(&in, &r, pb)
	what(&in, &r, pb)
	return r
}

func (r *Result) flag(g Group, at, format string, args ...any) {
	r.Flags = append(r.Flags, Flag{Group: g, Text: fmt.Sprintf(format, args...), At: at})
}

// NetworkFlag is the flag a network grant adds.
func NetworkFlag() Flag {
	return Flag{Group: What, Text: "the build gets network access"}
}

func who(in *Input, r *Result) {
	cur := in.RPC.Maintainer
	if cur == "" {
		cur = "-"
	}
	if in.Update() && in.Maintainer != cur {
		r.flag(Who, "", "AUR maintainer changed: %s → %s", in.Maintainer, cur)
	}
	for _, a := range in.NewAuthors {
		r.flag(Who, "", "new commit author: %s", a)
	}
	if in.RPC.Maintainer == "" {
		r.flag(Who, "", "orphaned: no AUR maintainer")
	}
	if in.RPC.FirstSubmitted > 0 {
		age := in.Now.Sub(time.Unix(in.RPC.FirstSubmitted, 0))
		if age < 30*24*time.Hour {
			r.flag(Who, "", "first submitted %d days ago", int(age.Hours()/24))
		}
	}
	if in.RPC.OutOfDate > 0 {
		r.Notes = append(r.Notes, "flagged out of date on the AUR since "+time.Unix(in.RPC.OutOfDate, 0).UTC().Format(time.DateOnly))
	}
}

func where(in *Input, r *Result, pb []line) {
	srcs := in.New.SRCINFO.Sources(in.Arch)
	pkgbuildAt := func(host string) string {
		for _, l := range pb {
			if strings.Contains(strings.ToLower(l.Code), host) {
				return "PKGBUILD:" + strconv.Itoa(l.Num)
			}
		}
		return ".SRCINFO"
	}

	if !in.Update() {
		var hosts []string
		for _, s := range srcs {
			if h := s.Host(); h != "" && !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
		if len(hosts) > 0 {
			r.Notes = append(r.Notes, "downloads from "+strings.Join(hosts, ", "))
		}
	} else {
		oldSrcs := in.Old.SRCINFO.Sources(in.Arch)
		oldHosts := map[string]bool{}
		for _, s := range oldSrcs {
			if h := s.Host(); h != "" {
				oldHosts[h] = true
			}
		}
		flagged := map[string]bool{}
		for _, s := range srcs {
			h := s.Host()
			if h == "" {
				continue
			}
			if !oldHosts[h] && !flagged[h] {
				flagged[h] = true
				r.flag(Where, pkgbuildAt(h), "new source host %s", h)
				continue
			}
			for _, o := range oldSrcs {
				if o.Array == s.Array && o.Index == s.Index && o.Remote() && o.Host() != h && !flagged[h] {
					r.flag(Where, pkgbuildAt(h), "source %s moved from %s to %s", s.File(), o.Host(), h)
				}
			}
		}
		if pkgver(in.Old.SRCINFO) == pkgver(in.New.SRCINFO) {
			for _, s := range srcs {
				for _, o := range oldSrcs {
					if o.Array != s.Array || o.Index != s.Index {
						continue
					}
					for _, k := range srcinfo.SumKeys {
						if a, b := o.Sums[k], s.Sums[k]; a != "" && b != "" && a != b {
							r.flag(Where, ".SRCINFO", "checksum of %s changed, but pkgver stayed %s", s.File(), pkgver(in.New.SRCINFO))
							break
						}
					}
				}
			}
		}
	}

	var vcs []string
	for _, s := range srcs {
		if !s.Remote() {
			continue
		}
		if s.VCS() {
			vcs = append(vcs, s.File())
			continue
		}
		if unverified(s) {
			r.flag(Where, ".SRCINFO", "no checksum (SKIP) for %s", s.File())
		}
	}
	if len(vcs) > 0 {
		r.flag(Where, ".SRCINFO", "upstream code isn't pinned (VCS source %s)", strings.Join(vcs, ", "))
	}

	urlsOutsideSRCINFO(in, r, pb)
	staleSRCINFO(in, r)
}

func pkgver(s *srcinfo.SRCINFO) string {
	v := s.Get("pkgver")
	if e := s.Get("epoch"); e != "" && e != "0" {
		v = e + ":" + v
	}
	return v
}

// unverified reports whether every checksum of a source is SKIP.
func unverified(s srcinfo.Source) bool {
	for _, k := range srcinfo.SumKeys {
		if v := s.Sums[k]; v != "" && v != "SKIP" {
			return false
		}
	}
	return true
}

// considered returns the line numbers of a file the line rules look at:
// all of them on a first install, only added ones on an update. A nil map
// means all.
func considered(in *Input, file string) map[int]bool {
	if !in.Update() {
		return nil
	}
	nums := map[int]bool{}
	for _, l := range in.Diffs[file].Added {
		nums[l.Num] = true
	}
	return nums
}

// urlsOutsideSRCINFO flags URLs in the PKGBUILD whose host isn't among
// .SRCINFO's sources: a mismatched .SRCINFO can hide what makepkg will
// really fetch.
func urlsOutsideSRCINFO(in *Input, r *Result, pb []line) {
	known := map[string]bool{}
	for k, vals := range in.New.SRCINFO.Base {
		if k != "source" && !strings.HasPrefix(k, "source_") {
			continue
		}
		for _, v := range vals {
			if _, u, ok := strings.Cut(v, "::"); ok {
				v = u
			}
			if h := srcinfo.Host(v); h != "" {
				known[h] = true
			}
		}
	}
	vars := simpleVars(pb)
	for _, k := range []string{"pkgbase", "pkgver", "pkgrel", "epoch"} {
		if v := in.New.SRCINFO.Get(k); v != "" {
			vars[k] = v
		}
	}
	if len(in.New.SRCINFO.Packages) == 1 {
		vars["pkgname"] = in.New.SRCINFO.Packages[0].Name
	}
	only := considered(in, "PKGBUILD")
	flagged := map[string]bool{}
	for _, l := range pb {
		if only != nil && !only[l.Num] {
			continue
		}
		switch l.Var {
		case "url", "pkgdesc":
			continue
		}
		for _, u := range urlRe.FindAllString(l.Code, -1) {
			h := srcinfo.Host(expand(u, vars))
			if h == "" || strings.Contains(h, "$") || known[h] || flagged[h] {
				continue
			}
			flagged[h] = true
			r.flag(Where, "PKGBUILD:"+strconv.Itoa(l.Num), "URL host %s isn't among .SRCINFO's sources", h)
		}
	}
}

// sourceKey reports whether a PKGBUILD variable decides what makepkg
// fetches.
func sourceKey(name string) bool {
	base, _, _ := strings.Cut(name, "_")
	switch {
	case base == "source", base == "pkgver", base == "epoch", base == "noextract":
		return true
	case slices.Contains(srcinfo.SumKeys, base):
		return true
	}
	return false
}

// staleSRCINFO flags an update whose PKGBUILD sources or checksums
// changed while .SRCINFO didn't.
func staleSRCINFO(in *Input, r *Result) {
	if !in.Update() {
		return
	}
	if _, ok := in.Diffs[".SRCINFO"]; ok {
		return
	}
	d, ok := in.Diffs["PKGBUILD"]
	if !ok {
		return
	}
	check := func(lines []line, changed []aur.Line) (int, bool) {
		nums := map[int]bool{}
		for _, c := range changed {
			nums[c.Num] = true
		}
		for _, l := range lines {
			if !nums[l.Num] || l.Func != "" {
				continue
			}
			if sourceKey(l.Array) || sourceKey(l.Var) {
				return l.Num, true
			}
		}
		return 0, false
	}
	if n, ok := check(scan(string(in.New.Files["PKGBUILD"])), d.Added); ok {
		r.flag(Where, "PKGBUILD:"+strconv.Itoa(n), ".SRCINFO didn't change, but the PKGBUILD's sources or checksums did")
		return
	}
	if n, ok := check(scan(string(in.Old.Files["PKGBUILD"])), d.Removed); ok {
		r.flag(Where, "PKGBUILD:"+strconv.Itoa(n)+" (approved)", ".SRCINFO didn't change, but the PKGBUILD's sources or checksums did")
	}
}

func what(in *Input, r *Result, pb []line) {
	installs := in.New.SRCINFO.Install()
	installScripts(in, r, pb, installs)

	// Command patterns in the PKGBUILD, install scripts and local shell
	// scripts the PKGBUILD may run.
	files := []string{"PKGBUILD"}
	files = append(files, installs...)
	for _, s := range in.New.SRCINFO.Sources(in.Arch) {
		if s.Remote() || slices.Contains(files, s.File()) {
			continue
		}
		data := in.New.Files[s.File()]
		ext := path.Ext(s.File())
		if ext == ".sh" || ext == ".bash" || bytes.HasPrefix(data, []byte("#!")) {
			files = append(files, s.File())
		}
	}
	for _, f := range files {
		data, ok := in.New.Files[f]
		if !ok {
			continue
		}
		commandFlags(in, r, f, data)
	}

	extraFiles(in, r, installs)

	if in.Network {
		r.Flags = append(r.Flags, NetworkFlag())
	}
}

func installScripts(in *Input, r *Result, pb []line, installs []string) {
	for _, name := range installs {
		switch {
		case !in.Update():
			r.flag(What, name, "install script %s runs as root", name)
		case !slices.Contains(in.Old.SRCINFO.Install(), name):
			r.flag(What, name, "install script %s added", name)
		case !bytes.Equal(in.Old.Files[name], in.New.Files[name]):
			r.flag(What, name, "install script %s changed", name)
		}
	}
	// An install= in the PKGBUILD that .SRCINFO doesn't list.
	for _, l := range pb {
		if l.Var != "install" {
			continue
		}
		_, v, _ := strings.Cut(strings.TrimSpace(l.Code), "=")
		v = strings.Trim(strings.TrimSpace(v), `'"`)
		if v != "" && !strings.Contains(v, "$") && !slices.Contains(installs, v) {
			r.flag(What, "PKGBUILD:"+strconv.Itoa(l.Num), "install=%s isn't in .SRCINFO", v)
		}
	}
}

func commandFlags(in *Input, r *Result, file string, data []byte) {
	type key struct{ cmd, where string }
	var order []key
	at := map[key][]string{}
	only := considered(in, file)
	if in.Update() {
		if _, existed := in.Old.Files[file]; !existed {
			only = nil
		}
	}
	for _, l := range scan(string(data)) {
		if only != nil && !only[l.Num] {
			continue
		}
		for _, c := range l.matches() {
			k := key{c, l.where()}
			if _, ok := at[k]; !ok {
				order = append(order, k)
			}
			at[k] = append(at[k], strconv.Itoa(l.Num))
		}
	}
	for _, k := range order {
		r.flag(What, file+":"+strings.Join(at[k], ","), "%s in %s", k.cmd, k.where)
	}
}

// extraFiles flags repository files makepkg doesn't need.
func extraFiles(in *Input, r *Result, installs []string) {
	allowed := map[string]bool{
		"PKGBUILD": true, ".SRCINFO": true, ".gitignore": true, ".nvchecker.toml": true,
		"LICENSE": true, "REUSE.toml": true,
	}
	for _, name := range installs {
		allowed[name] = true
	}
	for _, s := range in.New.SRCINFO.Sources(in.Arch) {
		if !s.Remote() {
			allowed[s.File()] = true
		}
	}
	// Local sources for other architectures are fine too.
	for k, vals := range in.New.SRCINFO.Base {
		if !strings.HasPrefix(k, "source_") {
			continue
		}
		for _, v := range vals {
			if !strings.Contains(v, "://") {
				_, file, ok := strings.Cut(v, "::")
				if !ok {
					file = v
				}
				allowed[file] = true
			}
		}
	}
	for _, f := range slices.Sorted(maps.Keys(in.New.Files)) {
		if allowed[f] || strings.HasPrefix(f, "LICENSES/") {
			continue
		}
		if in.Update() {
			if _, changed := in.Diffs[f]; !changed {
				continue
			}
		}
		r.flag(What, f, "extra file %s", f)
	}
}
