// Package srcinfo parses .SRCINFO files, the static metadata makepkg
// writes next to a PKGBUILD. Reading it never runs the PKGBUILD.
package srcinfo

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// SRCINFO is a parsed .SRCINFO: the pkgbase section and one section per
// package. A package section overrides the base fields it sets.
type SRCINFO struct {
	PkgBase  string
	Base     map[string][]string
	Packages []Package
}

// Package is one pkgname section.
type Package struct {
	Name   string
	Fields map[string][]string
}

// Parse reads a .SRCINFO.
func Parse(r io.Reader) (*SRCINFO, error) {
	s := &SRCINFO{Base: map[string][]string{}}
	cur := s.Base
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: no '='", n)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "pkgbase":
			if s.PkgBase != "" {
				return nil, fmt.Errorf("line %d: second pkgbase", n)
			}
			s.PkgBase = val
			cur = s.Base
		case "pkgname":
			if s.PkgBase == "" {
				return nil, fmt.Errorf("line %d: pkgname before pkgbase", n)
			}
			s.Packages = append(s.Packages, Package{Name: val, Fields: map[string][]string{}})
			cur = s.Packages[len(s.Packages)-1].Fields
		default:
			if s.PkgBase == "" {
				return nil, fmt.Errorf("line %d: %s before pkgbase", n, key)
			}
			// An empty value in a package section clears the base array.
			if val == "" {
				if _, ok := cur[key]; !ok {
					cur[key] = nil
				}
				continue
			}
			cur[key] = append(cur[key], val)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if s.PkgBase == "" {
		return nil, fmt.Errorf("no pkgbase")
	}
	if len(s.Packages) == 0 {
		return nil, fmt.Errorf("no pkgname")
	}
	return s, nil
}

// Get returns the first value of a base field, or "".
func (s *SRCINFO) Get(key string) string {
	if v := s.Base[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Field returns a field of one package, falling back to the base.
func (s *SRCINFO) Field(p Package, key string) []string {
	if v, ok := p.Fields[key]; ok {
		return v
	}
	return s.Base[key]
}

// Version is [epoch:]pkgver-pkgrel.
func (s *SRCINFO) Version() string {
	v := s.Get("pkgver") + "-" + s.Get("pkgrel")
	if e := s.Get("epoch"); e != "" && e != "0" {
		v = e + ":" + v
	}
	return v
}

// Names lists the package names.
func (s *SRCINFO) Names() []string {
	var names []string
	for _, p := range s.Packages {
		names = append(names, p.Name)
	}
	return names
}

// Package returns the package section with this name.
func (s *SRCINFO) Package(name string) (Package, bool) {
	for _, p := range s.Packages {
		if p.Name == name {
			return p, true
		}
	}
	return Package{}, false
}

// Depends lists everything a build of the whole package base needs on
// this architecture: makedepends, checkdepends and every package's
// depends, without duplicates. Runtime dependencies come first.
func (s *SRCINFO) Depends(arch string) (runtime, build []string) {
	seen := map[string]bool{}
	add := func(list *[]string, vals []string) {
		for _, v := range vals {
			if !seen[v] {
				seen[v] = true
				*list = append(*list, v)
			}
		}
	}
	for _, p := range s.Packages {
		add(&runtime, s.Field(p, "depends"))
		add(&runtime, s.Field(p, "depends_"+arch))
	}
	for _, k := range []string{"makedepends", "checkdepends"} {
		add(&build, s.Base[k])
		add(&build, s.Base[k+"_"+arch])
	}
	return runtime, build
}

// Install lists the install scripts named by the base or any package.
func (s *SRCINFO) Install() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range s.Packages {
		for _, v := range s.Field(p, "install") {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// SumKeys are the checksum arrays makepkg knows.
var SumKeys = []string{"cksums", "md5sums", "sha1sums", "sha224sums", "sha256sums", "sha384sums", "sha512sums", "b2sums"}

// Source is one entry of a source array with its checksums.
type Source struct {
	Array string // "source" or "source_<arch>"
	Index int
	Name  string // the "name::" prefix, if any
	URL   string // everything after it
	Sums  map[string]string
}

// Sources lists the base source arrays that apply on arch.
func (s *SRCINFO) Sources(arch string) []Source {
	var out []Source
	for _, suffix := range []string{"", "_" + arch} {
		arr := "source" + suffix
		for i, v := range s.Base[arr] {
			src := Source{Array: arr, Index: i, URL: v, Sums: map[string]string{}}
			if name, rest, ok := strings.Cut(v, "::"); ok {
				src.Name, src.URL = name, rest
			}
			for _, k := range SumKeys {
				if sums := s.Base[k+suffix]; i < len(sums) {
					src.Sums[k] = sums[i]
				}
			}
			out = append(out, src)
		}
	}
	return out
}

// Remote reports whether makepkg downloads the source.
func (src Source) Remote() bool {
	return strings.Contains(src.URL, "://")
}

// VCS reports whether the source is a version-control checkout, whose
// content isn't pinned by a checksum.
func (src Source) VCS() bool {
	scheme, _, ok := strings.Cut(src.URL, "://")
	if !ok {
		return false
	}
	switch scheme {
	case "git", "svn", "bzr":
		return true
	}
	vcs, _, _ := strings.Cut(scheme, "+")
	switch vcs {
	case "git", "hg", "svn", "bzr", "fossil":
		return strings.Contains(scheme, "+")
	}
	return false
}

// File is the file name makepkg stores a remote source under, or the
// path of a local one.
func (src Source) File() string {
	if src.Name != "" {
		return src.Name
	}
	u := src.URL
	if i := strings.IndexAny(u, "#?"); i >= 0 && src.Remote() {
		u = u[:i]
	}
	u = strings.TrimSuffix(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	if src.VCS() {
		u = strings.TrimSuffix(u, ".git")
	}
	return u
}

// Host returns the lowercase host of a remote source, or "".
func (src Source) Host() string {
	if !src.Remote() {
		return ""
	}
	return Host(src.URL)
}

// Host returns the lowercase host of a URL, ignoring a VCS prefix such
// as "git+", or "" if it has none.
func Host(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return ""
	}
	if _, after, ok := strings.Cut(scheme, "+"); ok {
		scheme = after
	}
	u, err := url.Parse(scheme + "://" + rest)
	if err != nil || u.Hostname() == "" {
		host := rest
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
		return strings.ToLower(host)
	}
	return strings.ToLower(u.Hostname())
}
