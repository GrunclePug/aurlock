package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// pacmanInstalled maps installed package names to their versions.
func pacmanInstalled(ctx context.Context) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "pacman", "-Q").Output()
	if err != nil {
		return nil, fmt.Errorf("pacman -Q: %w", err)
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

// pacmanResolvable reports whether the sync repositories can satisfy a
// dependency, by name or through provides.
func pacmanResolvable(ctx context.Context, dep string) bool {
	cmd := exec.CommandContext(ctx, "pacman", "-Sp", "--noconfirm", "--print-format", "%n", "--", dep)
	return cmd.Run() == nil
}

// sudoPacman runs pacman as root, attached to the terminal.
func sudoPacman(ctx context.Context, env Env, args ...string) error {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"pacman"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo pacman %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// depName strips the version constraint from a dependency.
func depName(dep string) string {
	if i := strings.IndexAny(dep, "<>="); i >= 0 {
		return dep[:i]
	}
	return dep
}
