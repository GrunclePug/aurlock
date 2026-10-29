package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func pacmanInstalled(ctx context.Context) (map[string]string, error) {
	return pacmanQuery(ctx, "-Q")
}

// pacmanForeign lists installed packages that aren't in the sync
// repositories: AUR packages, and locally built ones.
func pacmanForeign(ctx context.Context) (map[string]string, error) {
	return pacmanQuery(ctx, "-Qm")
}

func pacmanQuery(ctx context.Context, op string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "pacman", op).Output()
	var exit *exec.ExitError
	// A filter that matches nothing exits 1 without output.
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0) {
		return nil, fmt.Errorf("pacman %s: %w", op, err)
	}
	have := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if name, version, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			have[name] = version
		}
	}
	return have, nil
}

// pacmanMissing returns the dependencies no installed package satisfies.
func pacmanMissing(ctx context.Context, deps []string) ([]string, error) {
	if len(deps) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "pacman", append([]string{"-T", "--"}, deps...)...)
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil, nil
	case errors.As(err, &exit) && exit.ExitCode() == 127:
		return strings.Fields(string(out)), nil
	}
	return nil, fmt.Errorf("pacman -T: %w", err)
}

// pacmanResolvable reports whether the sync repositories satisfy a
// dependency, by name or through provides.
func pacmanResolvable(ctx context.Context, dep string) bool {
	cmd := exec.CommandContext(ctx, "pacman", "-Sp", "--noconfirm", "--print-format", "%n", "--", dep)
	return cmd.Run() == nil
}

// sudoPacman runs pacman as root, attached to the terminal. A cancelled
// context stops it from starting but never kills it: pacman gets Ctrl-C
// from the terminal itself and stops safely.
func (a *app) sudoPacman(ctx context.Context, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command("sudo", append([]string{"pacman"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo pacman %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func depName(dep string) string {
	if i := strings.IndexAny(dep, "<>="); i >= 0 {
		return dep[:i]
	}
	return dep
}
