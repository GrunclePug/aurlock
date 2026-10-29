package store

import (
	"cmp"
	"fmt"
	"slices"
)

// Approval is one line of the approvals file: the AUR commit of a package
// base the owner approved.
type Approval struct {
	PkgBase    string
	Commit     string
	Version    string
	Maintainer string // "-" if the package was orphaned
	Date       string // YYYY-MM-DD
	Online     bool   // the build may use the network
}

const approvedHeader = "# pkgbase  commit  version  maintainer  approved  network"

type Approvals struct {
	path string
	t    *Table
}

// LoadApprovals reads path. A missing file has no approvals.
func LoadApprovals(path string) (*Approvals, error) {
	t, err := load(path, approvedHeader, 6, func(f []string) error {
		switch {
		case !ValidName(f[0]):
			return fmt.Errorf("bad package base %q", f[0])
		case !ValidCommit(f[1]):
			return fmt.Errorf("bad commit %q", f[1])
		case !dateRe.MatchString(f[4]):
			return fmt.Errorf("bad date %q", f[4])
		case f[5] != "offline" && f[5] != "online":
			return fmt.Errorf("network is %q, want offline or online", f[5])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Approvals{path: path, t: t}, nil
}

func toApproval(f []string) Approval {
	return Approval{PkgBase: f[0], Commit: f[1], Version: f[2], Maintainer: f[3], Date: f[4], Online: f[5] == "online"}
}

func (a *Approvals) Get(pkgbase string) (Approval, bool) {
	f, ok := a.t.Get(pkgbase)
	if !ok {
		return Approval{}, false
	}
	return toApproval(f), true
}

// List is sorted by package base.
func (a *Approvals) List() []Approval {
	var out []Approval
	for _, f := range a.t.Records() {
		out = append(out, toApproval(f))
	}
	slices.SortFunc(out, func(x, y Approval) int { return cmp.Compare(x.PkgBase, y.PkgBase) })
	return out
}

// Set records an approval, replacing any earlier one for the package base.
// The network grant is part of the approval, so approving a new commit
// without it resets the package to offline.
func (a *Approvals) Set(ap Approval) {
	network := "offline"
	if ap.Online {
		network = "online"
	}
	m := ap.Maintainer
	if m == "" {
		m = "-"
	}
	a.t.Set([]string{ap.PkgBase, ap.Commit, ap.Version, m, ap.Date, network})
}

func (a *Approvals) Delete(pkgbase string) bool {
	return a.t.Delete(pkgbase)
}

func (a *Approvals) Save() error {
	return a.t.save(a.path)
}

// Installed is one line of the install log: which approved commit an
// installed package was built from.
type Installed struct {
	PkgName string
	PkgBase string
	Commit  string
	Version string
	Date    string
}

const installedHeader = "# pkgname  pkgbase  commit  version  installed"

type InstallLog struct {
	path string
	t    *Table
}

// LoadInstalled reads path. A missing file is empty.
func LoadInstalled(path string) (*InstallLog, error) {
	t, err := load(path, installedHeader, 5, func(f []string) error {
		switch {
		case !ValidName(f[0]) || !ValidName(f[1]):
			return fmt.Errorf("bad package name")
		case !ValidCommit(f[2]):
			return fmt.Errorf("bad commit %q", f[2])
		case !dateRe.MatchString(f[4]):
			return fmt.Errorf("bad date %q", f[4])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &InstallLog{path: path, t: t}, nil
}

func toInstalled(f []string) Installed {
	return Installed{PkgName: f[0], PkgBase: f[1], Commit: f[2], Version: f[3], Date: f[4]}
}

func (l *InstallLog) Get(pkgname string) (Installed, bool) {
	f, ok := l.t.Get(pkgname)
	if !ok {
		return Installed{}, false
	}
	return toInstalled(f), true
}

// List is sorted by package name.
func (l *InstallLog) List() []Installed {
	var out []Installed
	for _, f := range l.t.Records() {
		out = append(out, toInstalled(f))
	}
	slices.SortFunc(out, func(x, y Installed) int { return cmp.Compare(x.PkgName, y.PkgName) })
	return out
}

func (l *InstallLog) Set(in Installed) {
	l.t.Set([]string{in.PkgName, in.PkgBase, in.Commit, in.Version, in.Date})
}

func (l *InstallLog) Delete(pkgname string) bool {
	return l.t.Delete(pkgname)
}

func (l *InstallLog) Save() error {
	return l.t.save(l.path)
}
