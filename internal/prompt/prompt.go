// Package prompt asks the owner to approve a package: it shows the flags,
// opens views on request and reads the answer. Anything unclear is a no.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/grunclepug/aurlock/internal/check"
)

// Review is what one prompt shows and can open.
type Review struct {
	PkgBase    string
	OldVersion string // "" on a first install
	NewVersion string
	Flags      []check.Flag
	Notes      []string
	OfferGrant bool // offer [g]rant network

	View func() error           // the PKGBUILD and install scripts in a pager
	Diff func() error           // git diff since the approval; nil on a first install
	AI   func() (string, error) // the AI second opinion; nil hides [c]
}

// Decision is the owner's answer.
type Decision struct {
	Approve bool
	Online  bool // approve with network during the build
}

// Ask shows a review and reads answers until it has a decision. EOF is no.
func Ask(in *bufio.Reader, out io.Writer, r Review) (Decision, error) {
	var d Decision
	showHeader := true
	for {
		flags := r.Flags
		if d.Online {
			flags = append(slices.Clone(flags), check.NetworkFlag())
		}
		if showHeader {
			header(out, r, flags)
			showHeader = false
		}
		fmt.Fprint(out, question(r, len(flags) > 0, d.Online))
		ans, err := readLine(in)
		if err != nil {
			fmt.Fprintln(out)
			if errors.Is(err, io.EOF) {
				fmt.Fprintf(out, "skipped %s\n", r.PkgBase)
				return Decision{}, nil
			}
			return Decision{}, err
		}

		switch {
		case len(flags) == 0 && (ans == "y" || ans == "yes"):
			d.Approve = true
			return d, nil
		case len(flags) > 0 && ans == r.PkgBase:
			d.Approve = true
			return d, nil
		case ans == "v":
			if err := r.View(); err != nil {
				fmt.Fprintf(out, "view: %v\n", err)
			}
		case ans == "d" && r.Diff != nil:
			if err := r.Diff(); err != nil {
				fmt.Fprintf(out, "diff: %v\n", err)
			}
		case ans == "c" && r.AI != nil:
			fmt.Fprintln(out, "asking for a second opinion...")
			answer, err := r.AI()
			// Back to the prompt with the flags above the answer.
			header(out, r, flags)
			if err != nil {
				fmt.Fprintf(out, "second opinion failed: %v\n", err)
			} else {
				fmt.Fprintf(out, "\nsecond opinion (advice only):\n%s\n\n", indent(strings.TrimSpace(answer)))
			}
		case ans == "g" && r.OfferGrant && !d.Online:
			d.Online = true
			showHeader = true
		default:
			if len(flags) > 0 && (ans == "y" || ans == "yes") {
				fmt.Fprintf(out, "skipped %s: a flagged package needs its name typed\n", r.PkgBase)
			} else {
				fmt.Fprintf(out, "skipped %s\n", r.PkgBase)
			}
			return Decision{}, nil
		}
	}
}

func header(out io.Writer, r Review, flags []check.Flag) {
	version := r.NewVersion + " (new)"
	if r.OldVersion != "" {
		version = r.OldVersion + " → " + r.NewVersion
	}
	status := "no flags"
	switch len(flags) {
	case 0:
	case 1:
		status = "1 flag"
	default:
		status = fmt.Sprintf("%d flags", len(flags))
	}
	fmt.Fprintf(out, "%s %s   %s\n", r.PkgBase, version, status)
	fmt.Fprint(out, FormatFlags(flags))
	for _, n := range r.Notes {
		fmt.Fprintf(out, "  · %s\n", n)
	}
}

// FormatFlags lists flags by group, one per line.
func FormatFlags(flags []check.Flag) string {
	var b strings.Builder
	for _, g := range check.Groups {
		for _, f := range flags {
			if f.Group == g {
				fmt.Fprintf(&b, "  ! %-6s %s\n", string(g)+":", f)
			}
		}
	}
	return b.String()
}

func question(r Review, flagged, online bool) string {
	var opts []string
	if r.AI != nil {
		opts = append(opts, "[c]laude")
	}
	opts = append(opts, "[v]iew")
	if r.Diff != nil {
		opts = append(opts, "[d]iff")
	}
	if r.OfferGrant && !online {
		opts = append(opts, "[g]rant network")
	}
	if flagged {
		return fmt.Sprintf("type %q to approve, or %s / [n]o: ", r.PkgBase, strings.Join(opts, " / "))
	}
	keys := []string{"y", "N"}
	for _, o := range opts {
		keys = append(keys, o[1:2])
	}
	return fmt.Sprintf("approve and install? [%s] ", strings.Join(keys, "/"))
}

// AskGrant asks whether an approved package may build with network after
// an offline build failed. Granting is a flag, so it takes the name typed.
func AskGrant(in *bufio.Reader, out io.Writer, pkgbase, version string, view func() error) (bool, error) {
	fmt.Fprintf(out, "%s %s   its last offline build failed with what looks like a network error\n", pkgbase, version)
	fmt.Fprint(out, FormatFlags([]check.Flag{check.NetworkFlag()}))
	for {
		fmt.Fprintf(out, "type %q to grant network for this commit, or [v]iew / [n]o: ", pkgbase)
		ans, err := readLine(in)
		if err != nil {
			fmt.Fprintln(out)
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		switch ans {
		case pkgbase:
			return true, nil
		case "v":
			if err := view(); err != nil {
				fmt.Fprintf(out, "view: %v\n", err)
			}
		default:
			return false, nil
		}
	}
}

func readLine(in *bufio.Reader) (string, error) {
	s, err := in.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
