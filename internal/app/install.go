package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/grunclepug/aurlock/internal/aur"
	"github.com/grunclepug/aurlock/internal/build"
	"github.com/grunclepug/aurlock/internal/check"
	"github.com/grunclepug/aurlock/internal/prompt"
	"github.com/grunclepug/aurlock/internal/srcinfo"
	"github.com/grunclepug/aurlock/internal/store"
)

// options are the pacman-style flags of -S.
type options struct {
	needed     bool // skip packages installed at the approved commit
	noconfirm  bool // no prompts: approved packages only
	removemake bool // remove make dependencies installed for the build
}

// target is one package base to review, build and install.
type target struct {
	base  string
	names []string // packages of the base to install
	rpc   aur.Package

	repo    *aur.Repo
	head    string
	snap    *check.Snapshot // at head
	online  bool            // the approved build may use the network
	current bool            // upgrade only: installed at HEAD already
}

func (t *target) label() string {
	return t.names[0]
}

func (a *app) install(ctx context.Context, opts options, names []string) int {
	if len(names) == 0 {
		return a.fail(errors.New("no packages given"))
	}
	for _, n := range names {
		if !store.ValidName(n) {
			return a.fail(fmt.Errorf("%q isn't a valid package name", n))
		}
	}
	if opts.needed {
		var err error
		if names, err = a.skipCurrent(ctx, names); err != nil {
			return a.fail(err)
		}
		if len(names) == 0 {
			return exitOK
		}
	}
	info, err := a.rpc.Info(ctx, names)
	if err != nil {
		return a.fail(err)
	}
	var targets []*target
	byBase := map[string]*target{}
	for _, n := range names {
		p, ok := info[n]
		if !ok {
			return a.fail(fmt.Errorf("%s: not in the AUR", n))
		}
		if !store.ValidName(p.PackageBase) {
			return a.fail(fmt.Errorf("%s: bad package base %q", n, p.PackageBase))
		}
		if t, ok := byBase[p.PackageBase]; ok {
			if !slices.Contains(t.names, n) {
				t.names = append(t.names, n)
			}
			continue
		}
		t := &target{base: p.PackageBase, names: []string{n}, rpc: p}
		byBase[p.PackageBase] = t
		targets = append(targets, t)
	}
	return a.process(ctx, opts, targets, false)
}

// skipCurrent drops the packages installed here at the approved commit.
func (a *app) skipCurrent(ctx context.Context, names []string) ([]string, error) {
	approvals, err := store.LoadApprovals(a.paths.approved)
	if err != nil {
		return nil, err
	}
	installed, err := a.installedHere(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		i := slices.IndexFunc(installed, func(in store.Installed) bool { return in.PkgName == n })
		if i >= 0 {
			ap, ok := approvals.Get(installed[i].PkgBase)
			if ok && ap.Commit == installed[i].Commit {
				continue
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func (a *app) upgrade(ctx context.Context) int {
	installed, err := a.installedHere(ctx)
	if err != nil {
		return a.fail(err)
	}
	var names []string
	for _, in := range installed {
		names = append(names, in.PkgName)
	}
	info, err := a.rpc.Info(ctx, names)
	if err != nil {
		return a.fail(err)
	}
	var targets []*target
	byBase := map[string]*target{}
	for _, in := range installed {
		p, ok := info[in.PkgName]
		if !ok {
			a.warnf("%s: no longer in the AUR, skipping", in.PkgName)
			continue
		}
		if p.PackageBase != in.PkgBase {
			a.warnf("%s: moved from package base %s to %s, skipping: reinstall it with aurlock install", in.PkgName, in.PkgBase, p.PackageBase)
			continue
		}
		if t, ok := byBase[in.PkgBase]; ok {
			t.names = append(t.names, in.PkgName)
			continue
		}
		t := &target{base: in.PkgBase, names: []string{in.PkgName}, rpc: p}
		byBase[in.PkgBase] = t
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		fmt.Fprintln(a.Stdout, "nothing to upgrade")
		return exitOK
	}
	return a.process(ctx, options{}, targets, true)
}

// process reviews every target first, then builds and installs the
// approved ones.
func (a *app) process(ctx context.Context, opts options, targets []*target, upgrade bool) int {
	failed := false
	var ready []*target
	for _, t := range targets {
		ok, err := a.review(ctx, opts, t, upgrade)
		if err != nil {
			a.warnf("%s: %v", t.label(), err)
			failed = true
			continue
		}
		if ok {
			ready = append(ready, t)
		}
	}
	if upgrade && !slices.ContainsFunc(targets, func(t *target) bool { return !t.current }) {
		fmt.Fprintln(a.Stdout, "nothing to upgrade")
	}
	for _, t := range ready {
		if ctx.Err() != nil {
			return exitFail
		}
		if err := a.build(ctx, opts, t); err != nil {
			a.warnf("%s: %v", t.label(), err)
			failed = true
		}
	}
	if failed {
		return exitFail
	}
	return exitOK
}

// errNeedsReview is a review that --noconfirm can't do.
var errNeedsReview = errors.New("needs review")

// review fetches a target and decides whether it may be built: approved
// at HEAD already, or approved now by the owner.
func (a *app) review(ctx context.Context, opts options, t *target, upgrade bool) (bool, error) {
	var err error
	t.repo, err = aur.Sync(ctx, filepath.Join(a.paths.cache, "repos", t.base+".git"), aur.RepoURL(a.AURURL, t.base))
	if err != nil {
		return false, err
	}
	if t.head, err = t.repo.Head(ctx); err != nil {
		return false, err
	}
	if upgrade {
		current, err := a.allInstalledAt(t)
		if err != nil {
			return false, err
		}
		if current {
			t.current = true
			return false, nil
		}
	}
	if t.snap, err = snapshot(ctx, t.repo, t.head); err != nil {
		return false, err
	}
	for _, n := range t.names {
		if _, ok := t.snap.SRCINFO.Package(n); !ok {
			return false, fmt.Errorf("package base %s doesn't build %s", t.base, n)
		}
	}
	if err := a.checkDeps(ctx, t.snap.SRCINFO); err != nil {
		return false, err
	}

	approvals, err := store.LoadApprovals(a.paths.approved)
	if err != nil {
		return false, err
	}
	ap, approved := approvals.Get(t.base)
	version := t.snap.SRCINFO.Version()
	hint := a.readHint(t.base)

	if approved && ap.Commit == t.head {
		t.online = ap.Online
		fmt.Fprintf(a.Stdout, ":: %s %s is approved\n", t.base, version)
		if !ap.Online && hint == t.head && a.TTY && !opts.noconfirm {
			grant, err := prompt.AskGrant(a.in, a.Stdout, t.base, version, a.viewer(ctx, t))
			if err != nil {
				return false, err
			}
			if grant {
				ap.Online = true
				if err := a.saveApproval(ap); err != nil {
					return false, err
				}
				t.online = true
			}
		}
		return true, nil
	}

	in := check.Input{Arch: a.arch, Now: a.Now(), RPC: t.rpc, New: t.snap}
	var extra []check.Flag
	update := approved && t.repo.HasCommit(ctx, ap.Commit)
	if approved && !update {
		extra = append(extra, check.Flag{Group: check.Who, Text: "the approved commit " + short(ap.Commit) + " isn't in the AUR history: reviewing the whole package"})
	}
	if update {
		if err := a.updateInput(ctx, t, ap, &in); err != nil {
			return false, err
		}
		if !t.repo.IsAncestor(ctx, ap.Commit, t.head) {
			extra = append(extra, check.Flag{Group: check.Who, Text: "history rewritten: the approved commit isn't an ancestor of HEAD"})
		}
	}
	result := check.Run(in)
	flags := append(extra, result.Flags...)

	if opts.noconfirm {
		return false, fmt.Errorf("%w: run aurlock install %s", errNeedsReview, t.label())
	}
	if !a.TTY {
		return false, fmt.Errorf("%w, but stdin isn't a terminal: run aurlock install %s in one", errNeedsReview, t.label())
	}

	r := prompt.Review{
		PkgBase:    t.base,
		NewVersion: version,
		Flags:      flags,
		Notes:      result.Notes,
		OfferGrant: (approved && ap.Online) || hint != "",
		View:       a.viewer(ctx, t),
	}
	ai := prompt.AIInput{
		PkgName: t.label(), PkgBase: t.base, NewVersion: version,
		Maintainer: t.rpc.Maintainer, Flags: flags,
	}
	if t.rpc.FirstSubmitted > 0 {
		ai.FirstSubmitted = time.Unix(t.rpc.FirstSubmitted, 0)
	}
	if update {
		r.OldVersion = ap.Version
		r.Diff = func() error {
			cmd := t.repo.Cmd(ctx, "diff", ap.Commit+".."+t.head)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
			return cmd.Run()
		}
		ai.OldVersion, ai.OldMaintainer = ap.Version, ap.Maintainer
		if ai.Content, err = t.repo.DiffText(ctx, ap.Commit, t.head); err != nil {
			return false, err
		}
	} else {
		ai.Content = reviewText(t.snap)
	}
	r.AI = func() (string, error) {
		ai.Flags = flags
		return prompt.Advise(ctx, a.Getenv("AURLOCK_AI"), prompt.AIPrompt(ai, a.Now()))
	}

	d, err := prompt.Ask(a.in, a.Stdout, r)
	if err != nil || !d.Approve {
		return false, err
	}
	maintainer := t.rpc.Maintainer
	if maintainer == "" {
		maintainer = "-"
	}
	err = a.saveApproval(store.Approval{
		PkgBase: t.base, Commit: t.head, Version: version, Maintainer: maintainer,
		Date: a.Now().Format(time.DateOnly), Online: d.Online,
	})
	if err != nil {
		return false, err
	}
	t.online = d.Online
	return true, nil
}

// allInstalledAt reports whether every package of a target is installed
// here at its AUR HEAD.
func (a *app) allInstalledAt(t *target) (bool, error) {
	log, err := store.LoadInstalled(a.paths.installed)
	if err != nil {
		return false, err
	}
	for _, n := range t.names {
		if in, ok := log.Get(n); !ok || in.Commit != t.head {
			return false, nil
		}
	}
	return true, nil
}

// updateInput fills in what an update review compares against.
func (a *app) updateInput(ctx context.Context, t *target, ap store.Approval, in *check.Input) error {
	old, err := snapshot(ctx, t.repo, ap.Commit)
	if err != nil {
		return fmt.Errorf("approved commit: %w", err)
	}
	in.Old, in.Maintainer = old, ap.Maintainer
	changed, err := t.repo.Changed(ctx, ap.Commit, t.head)
	if err != nil {
		return err
	}
	in.Diffs = map[string]aur.Diff{}
	for _, f := range changed {
		if in.Diffs[f], err = t.repo.FileDiff(ctx, ap.Commit, t.head, f); err != nil {
			return err
		}
	}
	before, err := t.repo.Authors(ctx, ap.Commit)
	if err != nil {
		return err
	}
	since, err := t.repo.Authors(ctx, ap.Commit+".."+t.head)
	if err != nil {
		return err
	}
	for _, author := range since {
		if !slices.Contains(before, author) && !knownPart(before, author) {
			in.NewAuthors = append(in.NewAuthors, author)
		}
	}
	return nil
}

// knownPart reports whether an author's name or email appears in the
// earlier history: only an author new in both counts as new.
func knownPart(before []string, author string) bool {
	name, email := splitAuthor(author)
	for _, b := range before {
		n, e := splitAuthor(b)
		if n == name || e == email {
			return true
		}
	}
	return false
}

func splitAuthor(s string) (name, email string) {
	i := strings.LastIndex(s, " <")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSuffix(s[i+2:], ">")
}

// snapshot reads a package repository at a commit, without running any
// of it.
func snapshot(ctx context.Context, repo *aur.Repo, commit string) (*check.Snapshot, error) {
	files, err := repo.Files(ctx, commit)
	if err != nil {
		return nil, err
	}
	s := &check.Snapshot{Files: map[string][]byte{}}
	for _, f := range files {
		data, ok, err := repo.File(ctx, commit, f)
		if err != nil {
			return nil, err
		}
		if ok {
			s.Files[f] = data
		}
	}
	data, ok := s.Files[".SRCINFO"]
	if !ok {
		return nil, fmt.Errorf("commit %s has no .SRCINFO", short(commit))
	}
	if _, ok := s.Files["PKGBUILD"]; !ok {
		return nil, fmt.Errorf("commit %s has no PKGBUILD", short(commit))
	}
	if s.SRCINFO, err = srcinfo.Parse(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf(".SRCINFO: %w", err)
	}
	return s, nil
}

// reviewText is the PKGBUILD and the install scripts, for the pager and a
// first-install second opinion.
func reviewText(s *check.Snapshot) string {
	var b strings.Builder
	for _, f := range append([]string{"PKGBUILD"}, s.SRCINFO.Install()...) {
		data, ok := s.Files[f]
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "==> %s <==\n%s", f, data)
		if !bytes.HasSuffix(data, []byte("\n")) {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (a *app) viewer(ctx context.Context, t *target) func() error {
	return func() error {
		return prompt.Page(ctx, a.Getenv("PAGER"), reviewText(t.snap), a.Stdout, a.Stderr)
	}
}

// saveApproval writes one approval, rereading the file first so a change
// synced in meanwhile isn't lost.
func (a *app) saveApproval(ap store.Approval) error {
	approvals, err := store.LoadApprovals(a.paths.approved)
	if err != nil {
		return err
	}
	approvals.Set(ap)
	return approvals.Save()
}

// ownProvides is what the packages of a base provide themselves.
func ownProvides(s *srcinfo.SRCINFO) map[string]bool {
	own := map[string]bool{}
	for _, p := range s.Packages {
		own[p.Name] = true
		for _, v := range s.Field(p, "provides") {
			own[depName(v)] = true
		}
	}
	return own
}

// buildDepends splits a base's dependencies into runtime and build ones,
// leaving out those the base satisfies itself.
func (a *app) buildDepends(s *srcinfo.SRCINFO) (runtime, makedeps []string) {
	own := ownProvides(s)
	keep := func(deps []string) []string {
		var out []string
		for _, d := range deps {
			if !own[depName(d)] {
				out = append(out, d)
			}
		}
		return out
	}
	r, m := s.Depends(a.arch)
	return keep(r), keep(m)
}

// checkDeps refuses a package that depends on AUR packages that aren't
// installed: aurlock doesn't resolve AUR dependencies.
func (a *app) checkDeps(ctx context.Context, s *srcinfo.SRCINFO) error {
	r, m := a.buildDepends(s)
	missing, err := pacmanMissing(ctx, append(r, m...))
	if err != nil {
		return err
	}
	var fromAUR []string
	for _, d := range missing {
		if !pacmanResolvable(ctx, d) {
			fromAUR = append(fromAUR, depName(d))
		}
	}
	if len(fromAUR) > 0 {
		return fmt.Errorf("needs AUR package %s: install it with aurlock first", strings.Join(fromAUR, ", "))
	}
	return nil
}

func (a *app) readHint(base string) string {
	data, err := os.ReadFile(filepath.Join(a.paths.hints, base))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (a *app) writeHint(base, commit string) error {
	if err := os.MkdirAll(a.paths.hints, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.paths.hints, base), []byte(commit+"\n"), 0o644)
}

// build builds an approved target in the sandbox and installs it.
func (a *app) build(ctx context.Context, opts options, t *target) (err error) {
	info := t.snap.SRCINFO
	version := info.Version()
	fmt.Fprintf(a.Stdout, ":: building %s %s\n", t.base, version)
	dirs := build.Dirs{
		Work:    filepath.Join(a.paths.cache, "build", t.base),
		SrcDest: filepath.Join(a.paths.cache, "src", t.base),
		PkgDest: filepath.Join(a.paths.cache, "pkg", t.base),
	}
	keys := info.Base["validpgpkeys"]
	if len(keys) > 0 {
		dirs.GnuPG = filepath.Join(a.paths.cache, "gnupg")
	}
	if err := build.Reset(dirs); err != nil {
		return err
	}
	if err := t.repo.Export(ctx, t.head, dirs.Work); err != nil {
		return err
	}
	if len(keys) > 0 {
		if err := build.ImportKeys(ctx, dirs.GnuPG, keys, a.Stderr); err != nil {
			return err
		}
	}

	pacmanFlags := []string{}
	if opts.noconfirm {
		pacmanFlags = append(pacmanFlags, "--noconfirm")
	}
	runDeps, makeDeps := a.buildDepends(info)
	missing, err := pacmanMissing(ctx, runDeps)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		args := append([]string{"-S", "--needed", "--asdeps"}, pacmanFlags...)
		if err := sudoPacman(ctx, a.Env, append(append(args, "--"), missing...)...); err != nil {
			return err
		}
	}
	if missing, err = pacmanMissing(ctx, makeDeps); err != nil {
		return err
	}
	if len(missing) > 0 {
		before, err := pacmanInstalled(ctx)
		if err != nil {
			return err
		}
		args := append([]string{"-S", "--needed", "--asdeps"}, pacmanFlags...)
		if err := sudoPacman(ctx, a.Env, append(append(args, "--"), missing...)...); err != nil {
			return err
		}
		after, err := pacmanInstalled(ctx)
		if err != nil {
			return err
		}
		var added []string
		for _, name := range sortedKeys(after) {
			if _, ok := before[name]; !ok {
				added = append(added, name)
			}
		}
		if opts.removemake && len(added) > 0 {
			defer func() {
				args := append([]string{"-Rns"}, pacmanFlags...)
				if rerr := sudoPacman(context.WithoutCancel(ctx), a.Env, append(append(args, "--"), added...)...); rerr != nil && err == nil {
					err = rerr
				}
			}()
		}
	}

	cfg := build.Config{Dirs: dirs, Online: t.online, Getenv: a.Getenv, Stdout: a.Stdout, Stderr: a.Stderr}
	if err := build.Fetch(ctx, cfg); err != nil {
		return err
	}
	if err := build.Build(ctx, cfg); err != nil {
		if errors.Is(err, build.ErrNeedsNetwork) {
			if herr := a.writeHint(t.base, t.head); herr != nil {
				a.warnf("%v", herr)
			}
			return fmt.Errorf("%w\n  if it needs network to build, run aurlock install %s to grant it", err, t.label())
		}
		return err
	}

	built, err := build.Packages(dirs.PkgDest)
	if err != nil {
		return err
	}
	var files []string
	for _, n := range t.names {
		f, ok := built[n]
		if !ok {
			return fmt.Errorf("makepkg didn't build %s", n)
		}
		files = append(files, f)
	}
	args := append([]string{"-U"}, pacmanFlags...)
	if err := sudoPacman(ctx, a.Env, append(append(args, "--"), files...)...); err != nil {
		return err
	}

	log, err := store.LoadInstalled(a.paths.installed)
	if err != nil {
		return err
	}
	today := a.Now().Format(time.DateOnly)
	for _, n := range t.names {
		log.Set(store.Installed{PkgName: n, PkgBase: t.base, Commit: t.head, Version: version, Date: today})
	}
	if err := log.Save(); err != nil {
		return err
	}
	os.Remove(filepath.Join(a.paths.hints, t.base))
	return nil
}
