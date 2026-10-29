// Package app is aurlock's command line: the subcommands, the pacman-style
// calls arcus makes, and the install flow that ties the other packages
// together.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/grunclepug/aurlock/internal/aur"
	"github.com/grunclepug/aurlock/internal/prompt"
	"github.com/grunclepug/aurlock/internal/store"
)

// Env is everything aurlock takes from its surroundings.
type Env struct {
	Args    []string // without the program name
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	TTY     bool // stdin is a terminal
	Getenv  func(string) string
	AURURL  string
	Now     func() time.Time
	Version string
}

const (
	exitOK          = 0
	exitFail        = 1
	exitError       = 2
	exitInterrupted = 130
)

const usage = `usage: aurlock <command> [args]

commands:
  install <pkg>...   check, prompt, build and install AUR packages
  upgrade            the same, for every installed package with a newer
                     AUR commit or approval
  outdated           list installed packages with a newer AUR commit or
                     approval; exit 1 if any, 0 if none, 2 on error
  list               approvals, and what's installed here
  forget <pkg>...    drop approvals (doesn't uninstall)
  -version           print the version

pacman-style calls, for arcus:
  aurlock --removemake -S --needed [--noconfirm] <pkg>...
  aurlock -R... [--noconfirm] <pkg>...   (runs sudo pacman)
  aurlock -D --asdeps <pkg>...           (runs sudo pacman)
`

type app struct {
	Env
	in    *prompt.Input
	paths paths
	rpc   *aur.Client
	arch  string
}

type paths struct {
	approved  string
	installed string
	hints     string // packages whose offline build looked like a network failure
	cache     string
}

func newPaths(getenv func(string) string) (paths, error) {
	home := getenv("HOME")
	if home == "" {
		return paths{}, errors.New("HOME isn't set")
	}
	xdg := func(key, fallback string) string {
		if v := getenv(key); filepath.IsAbs(v) {
			return v
		}
		return filepath.Join(home, fallback)
	}
	state := filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "aurlock")
	return paths{
		approved:  filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "aurlock", "approved"),
		installed: filepath.Join(state, "installed"),
		hints:     filepath.Join(state, "needs-network"),
		cache:     filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "aurlock"),
	}, nil
}

func Run(ctx context.Context, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.AURURL == "" {
		env.AURURL = aur.DefaultURL
	}
	a := &app{Env: env, in: prompt.NewInput(env.Stdin), rpc: aur.NewClient(env.AURURL), arch: archName()}
	args := env.Args
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return exitError
	}
	switch args[0] {
	case "-version", "--version":
		fmt.Fprintln(env.Stdout, "aurlock", env.Version)
		return exitOK
	case "-h", "-help", "--help", "help":
		fmt.Fprint(env.Stdout, usage)
		return exitOK
	}
	var err error
	if a.paths, err = newPaths(env.Getenv); err != nil {
		return a.fail(err)
	}
	a.warnConflicts()
	code := a.dispatch(ctx, args)
	if ctx.Err() != nil {
		fmt.Fprintln(env.Stderr, "\naurlock: interrupted")
		return exitInterrupted
	}
	return code
}

func (a *app) dispatch(ctx context.Context, args []string) int {
	switch args[0] {
	case "install":
		return a.install(ctx, options{}, args[1:])
	case "upgrade":
		if len(args) > 1 {
			return a.fail(errors.New("upgrade takes no arguments"))
		}
		return a.upgrade(ctx)
	case "outdated":
		if len(args) > 1 {
			return a.fail(errors.New("outdated takes no arguments"))
		}
		return a.outdated(ctx)
	case "list":
		if len(args) > 1 {
			return a.fail(errors.New("list takes no arguments"))
		}
		return a.list(ctx)
	case "forget":
		return a.forget(args[1:])
	}
	if strings.HasPrefix(args[0], "-") {
		return a.pacmanStyle(ctx, args)
	}
	fmt.Fprintf(a.Stderr, "aurlock: unknown command %q\n%s", args[0], usage)
	return exitError
}

func (a *app) fail(err error) int {
	fmt.Fprintf(a.Stderr, "aurlock: %v\n", err)
	return exitError
}

func (a *app) warnf(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "aurlock: "+format+"\n", args...)
}

// warnConflicts warns about Syncthing conflict copies of the approvals,
// which are never merged automatically.
func (a *app) warnConflicts() {
	files, err := store.ConflictFiles(a.paths.approved)
	if err != nil {
		a.warnf("warning: %v", err)
		return
	}
	for _, f := range files {
		a.warnf("warning: approvals conflict %s: merge it into the approvals file by hand and delete it", f)
	}
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	}
	return runtime.GOARCH
}

// pacmanStyle handles the yay-style calls arcus makes.
func (a *app) pacmanStyle(ctx context.Context, args []string) int {
	var op string
	for _, arg := range args {
		switch {
		case arg == "-S", strings.HasPrefix(arg, "-R") && !strings.HasPrefix(arg, "--"), strings.HasPrefix(arg, "-D") && !strings.HasPrefix(arg, "--"):
			if op != "" {
				return a.fail(fmt.Errorf("more than one operation: %s and %s", op, arg))
			}
			op = arg
		}
	}
	switch {
	case op == "":
		return a.fail(fmt.Errorf("unsupported arguments: %s", strings.Join(args, " ")))
	case op == "-S":
		var opts options
		var pkgs []string
		for _, arg := range args {
			switch arg {
			case "-S":
			case "--needed":
				opts.needed = true
			case "--noconfirm":
				opts.noconfirm = true
			case "--removemake":
				opts.removemake = true
			default:
				if strings.HasPrefix(arg, "-") {
					return a.fail(fmt.Errorf("unsupported option %s", arg))
				}
				pkgs = append(pkgs, arg)
			}
		}
		return a.install(ctx, opts, pkgs)
	case strings.HasPrefix(op, "-R"), strings.HasPrefix(op, "-D"):
		if err := a.sudoPacman(ctx, args...); err != nil {
			return a.fail(err)
		}
		if strings.HasPrefix(op, "-R") {
			if err := a.pruneInstalled(ctx); err != nil {
				return a.fail(err)
			}
		}
		return exitOK
	}
	return a.fail(fmt.Errorf("unsupported operation %s", op))
}

// pruneInstalled drops install records of packages no longer installed.
func (a *app) pruneInstalled(ctx context.Context) error {
	log, err := store.LoadInstalled(a.paths.installed)
	if err != nil {
		return err
	}
	have, err := pacmanInstalled(ctx)
	if err != nil {
		return err
	}
	changed := false
	for _, in := range log.List() {
		if _, ok := have[in.PkgName]; !ok {
			log.Delete(in.PkgName)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return log.Save()
}

// installedHere returns the install records of packages pacman still has.
func (a *app) installedHere(ctx context.Context) ([]store.Installed, error) {
	log, err := store.LoadInstalled(a.paths.installed)
	if err != nil {
		return nil, err
	}
	have, err := pacmanInstalled(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.Installed
	for _, in := range log.List() {
		if _, ok := have[in.PkgName]; ok {
			out = append(out, in)
		}
	}
	return out, nil
}

func short(commit string) string {
	return commit[:min(len(commit), 12)]
}

func (a *app) list(ctx context.Context) int {
	approvals, err := store.LoadApprovals(a.paths.approved)
	if err != nil {
		return a.fail(err)
	}
	installed, err := a.installedHere(ctx)
	if err != nil {
		return a.fail(err)
	}
	w := a.Stdout
	fmt.Fprintln(w, "approved:")
	list := approvals.List()
	if len(list) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, ap := range list {
		network := ""
		if ap.Online {
			network = "  network"
		}
		fmt.Fprintf(w, "  %s %s  %s  %s %s%s\n", ap.PkgBase, ap.Version, short(ap.Commit), ap.Maintainer, ap.Date, network)
	}
	fmt.Fprintln(w, "installed here:")
	if len(installed) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, in := range installed {
		note := ""
		if ap, ok := approvals.Get(in.PkgBase); !ok {
			note = "  (no approval)"
		} else if ap.Commit != in.Commit {
			note = "  (approved " + ap.Version + ")"
		}
		fmt.Fprintf(w, "  %s %s  %s  %s%s\n", in.PkgName, in.Version, short(in.Commit), in.Date, note)
	}

	untracked, err := a.untracked(ctx)
	if err != nil {
		return a.fail(err)
	}
	if len(untracked) == 0 {
		return exitOK
	}
	fmt.Fprintln(w, "untracked foreign packages (aurlock didn't install these):")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	var inAUR []string
	for _, u := range untracked {
		where := "not in the AUR"
		switch {
		case u.aur == unknown:
			where = "AUR unreachable"
		case u.aur != "":
			where = "AUR has " + u.aur
			inAUR = append(inAUR, u.name)
		}
		fmt.Fprintf(tw, "  %s %s\t%s\n", u.name, u.version, where)
	}
	tw.Flush()
	if len(inAUR) > 0 {
		fmt.Fprintln(w, "adopt them with: aurlock install <pkg>...")
	}
	return exitOK
}

type untrackedPkg struct {
	name, version string
	aur           string // AUR version, "" if not in the AUR
}

// unknown is the AUR version when the AUR can't be reached.
const unknown = "?"

// untracked lists foreign packages aurlock didn't install: ones from
// before aurlock, and locally built ones. They're shown, never adopted
// automatically, since a local package can share an AUR package's name.
func (a *app) untracked(ctx context.Context) ([]untrackedPkg, error) {
	foreign, err := pacmanForeign(ctx)
	if err != nil {
		return nil, err
	}
	log, err := store.LoadInstalled(a.paths.installed)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range foreign {
		if _, ok := log.Get(name); !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil, nil
	}
	info, err := a.rpc.Info(ctx, names)
	if err != nil {
		a.warnf("%v", err)
	}
	var out []untrackedPkg
	for _, n := range names {
		u := untrackedPkg{name: n, version: foreign[n]}
		if err != nil {
			u.aur = unknown
		} else if p, ok := info[n]; ok {
			u.aur = p.Version
		}
		out = append(out, u)
	}
	return out, nil
}

func (a *app) forget(names []string) int {
	if len(names) == 0 {
		return a.fail(errors.New("forget: no packages given"))
	}
	approvals, err := store.LoadApprovals(a.paths.approved)
	if err != nil {
		return a.fail(err)
	}
	log, err := store.LoadInstalled(a.paths.installed)
	if err != nil {
		return a.fail(err)
	}
	code := exitOK
	for _, name := range names {
		base := name
		if _, ok := approvals.Get(base); !ok {
			if in, ok := log.Get(name); ok {
				base = in.PkgBase
			}
		}
		if !approvals.Delete(base) {
			a.warnf("%s: no approval", name)
			code = exitFail
			continue
		}
		fmt.Fprintf(a.Stdout, "forgot %s\n", base)
	}
	if err := approvals.Save(); err != nil {
		return a.fail(err)
	}
	return code
}

func (a *app) outdated(ctx context.Context) int {
	approvals, err := store.LoadApprovals(a.paths.approved)
	if err != nil {
		return a.fail(err)
	}
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
	heads := map[string]string{}
	found := false
	for _, in := range installed {
		head, ok := heads[in.PkgBase]
		if !ok {
			head, err = aur.RemoteHead(ctx, aur.RepoURL(a.AURURL, in.PkgBase))
			if err != nil {
				return a.fail(err)
			}
			heads[in.PkgBase] = head
		}
		if head == in.Commit {
			continue
		}
		found = true
		state := "needs review"
		if ap, ok := approvals.Get(in.PkgBase); ok && ap.Commit == head {
			state = "approved"
		}
		version := "?"
		if p, ok := info[in.PkgName]; ok {
			version = p.Version
		}
		fmt.Fprintf(a.Stdout, "%s %s -> %s (%s)\n", in.PkgName, in.Version, version, state)
	}
	// Untracked packages don't count: outdated means upgrade has work.
	if untracked, err := a.untracked(ctx); err == nil {
		n := 0
		for _, u := range untracked {
			if u.aur != "" && u.aur != unknown {
				n++
			}
		}
		switch {
		case n == 1:
			a.warnf("1 untracked AUR package, see aurlock list")
		case n > 1:
			a.warnf("%d untracked AUR packages, see aurlock list", n)
		}
	}
	if found {
		return exitFail
	}
	return exitOK
}
