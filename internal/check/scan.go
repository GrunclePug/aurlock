package check

import (
	"regexp"
	"strings"
)

// line is one line of a shell file (a PKGBUILD or an install script) with
// a rough idea of where it sits. This is a heuristic reader for naming
// lines in flags, not a shell parser: it tracks quotes, top-level arrays
// and function bodies, which is enough for the files makepkg reads.
type line struct {
	Num   int
	Text  string
	Code  string // Text without a trailing comment
	Exec  string // Code with assigned data blanked out, see scan
	Func  string // enclosing function, "" at global scope
	Array string // top-level array the line belongs to
	Var   string // top-level scalar assigned on the line
}

var (
	funcRe  = regexp.MustCompile(`^(?:function\s+)?([A-Za-z_][\w-]*)\s*\(\s*\)`)
	arrayRe = regexp.MustCompile(`^([A-Za-z_]\w*)\+?=\(`)
	varRe   = regexp.MustCompile(`^([A-Za-z_]\w*)\+?=`)
)

// scan reads a shell file into lines. Comment-only lines are left out.
//
// Values assigned at the top level are data, not commands: depends=(curl)
// downloads nothing and pkgdesc="uses npm" runs nothing. Exec blanks them
// out but keeps command substitutions inside them and anything after the
// assignment on the same line, which do run.
func scan(text string) []line {
	var out []line
	var (
		single, double bool // inside quotes, possibly across lines
		tick           bool // inside a backtick substitution
		subst          int  // depth of $( substitutions
		paren, brace   int
		fn, array      string
		fnOpen         bool // fn's body brace has been seen
	)
	for i, raw := range strings.Split(text, "\n") {
		l := line{Num: i + 1, Text: raw}
		trimmed := strings.TrimSpace(raw)
		quoted := single || double || tick || subst > 0
		if !quoted && fn == "" && array == "" {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if m := funcRe.FindStringSubmatch(trimmed); m != nil {
				fn, fnOpen, brace = m[1], false, 0
			} else if m := arrayRe.FindStringSubmatch(trimmed); m != nil {
				array, paren = m[1], 0
			} else if m := varRe.FindStringSubmatch(trimmed); m != nil {
				l.Var = m[1]
			}
		} else if !quoted && strings.HasPrefix(trimmed, "#") {
			continue
		}
		l.Func, l.Array = fn, array

		code := raw
		exec := []byte(raw)
		eq := -1
		if l.Var != "" {
			eq = strings.IndexByte(raw, '=')
		}
		valueDone := l.Var == ""
		escaped := false
	walk:
		for j := 0; j < len(raw); j++ {
			c := raw[j]
			data := fn == "" && subst == 0 && !tick &&
				((array != "" && paren > 0) || (!valueDone && j > eq))
			switch {
			case escaped:
				escaped = false
			case single:
				if c == '\'' {
					single = false
				}
			case c == '\\':
				escaped = true
			case tick && c == '`':
				tick = false
			case subst > 0 && c == '(':
				subst++
			case subst > 0 && c == ')':
				subst--
			case subst > 0 || tick:
			case c == '$' && j+1 < len(raw) && raw[j+1] == '(':
				subst++
				j++
				data = false
			case c == '`':
				tick = true
				data = false
			case double:
				if c == '"' {
					double = false
				}
			case c == '\'':
				single = true
			case c == '"':
				double = true
			case c == '#' && (j == 0 || raw[j-1] == ' ' || raw[j-1] == '\t'):
				code = raw[:j]
				exec = exec[:j]
				break walk
			case c == '(':
				paren++
			case c == ')':
				paren--
			case c == '{':
				brace++
				if fn != "" {
					fnOpen = true
				}
			case c == '}':
				brace--
			case !valueDone && j > eq && strings.IndexByte(" \t;&|", c) >= 0:
				valueDone = true
				data = false
			}
			if data {
				exec[j] = ' '
			}
		}
		l.Code, l.Exec = code, string(exec)
		out = append(out, l)

		if array != "" && paren <= 0 && !single && !double && subst == 0 && !tick {
			array = ""
		}
		if fn != "" && fnOpen && brace <= 0 {
			fn = ""
		}
	}
	return out
}

// where describes a line's place for a flag: "build()" or "global scope".
func (l line) where() string {
	if l.Func != "" {
		return l.Func + "()"
	}
	return "global scope"
}

// command is a pattern for something that fetches or runs code the
// checksums don't cover.
type command struct {
	name string
	re   *regexp.Regexp
}

// Something in command position: after the start, whitespace, a shell
// operator or an opening substitution, optionally with a /usr/bin path.
const (
	pre  = "(?:^|[\\s;&|(`{!\"'])(?:(?:/usr)?/s?bin/)?"
	post = "(?:$|[\\s;&|)`}\"'])"
)

var commands = func() []command {
	var cs []command
	for _, w := range []string{"curl", "wget", "npm", "npx", "bun", "bunx", "pnpm", "yarn", "pip", "pip3", "base64", "eval", "nc", "ncat", "socat"} {
		cs = append(cs, command{w, regexp.MustCompile(pre + regexp.QuoteMeta(w) + post)})
	}
	cs = append(cs,
		command{"python -m pip", regexp.MustCompile(`\s-m\s+pip` + post)},
		command{"cargo install", regexp.MustCompile(pre + `cargo\s+install` + post)},
		command{"go install", regexp.MustCompile(pre + `go\s+install` + post)},
		command{"git clone", regexp.MustCompile(pre + `git\s+(?:[^;&|\s]+\s+)*?clone` + post)},
		command{"pipe into a shell", regexp.MustCompile(`\|\s*(?:sudo\s+)?(?:(?:/usr)?/bin/)?(?:ba|z|da|k)?sh` + post)},
		command{"/dev/tcp", regexp.MustCompile(`/dev/(?:tcp|udp)/`)},
	)
	return cs
}()

// matches returns the commands a line runs.
func (l line) matches() []string {
	var out []string
	for _, c := range commands {
		if c.re.MatchString(l.Exec) {
			out = append(out, c.name)
		}
	}
	return out
}

var (
	urlRe    = regexp.MustCompile("[A-Za-z][A-Za-z0-9+.-]*://[^\\s'\"()<>`]+")
	simpleRe = regexp.MustCompile(`^([A-Za-z_]\w*)=(['"]?)([^'"\s$()` + "`" + `;&|]*)(['"]?)\s*(?:#.*)?$`)
	varRefRe = regexp.MustCompile(`\$\{?([A-Za-z_]\w*)\}?`)
)

// simpleVars collects top-level assignments of literal values, such as
// _tag=v1.2, so URLs built from them can be resolved.
func simpleVars(lines []line) map[string]string {
	vars := map[string]string{}
	for _, l := range lines {
		if l.Func != "" || l.Array != "" {
			continue
		}
		if m := simpleRe.FindStringSubmatch(strings.TrimSpace(l.Text)); m != nil && m[2] == m[4] {
			vars[m[1]] = m[3]
		}
	}
	return vars
}

// expand substitutes $name and ${name} for known variables; unknown ones
// stay as they are.
func expand(s string, vars map[string]string) string {
	return varRefRe.ReplaceAllStringFunc(s, func(ref string) string {
		m := varRefRe.FindStringSubmatch(ref)
		// ${name...} with an operator isn't a plain reference.
		if strings.HasPrefix(ref, "${") && !strings.HasSuffix(ref, "}") {
			return ref
		}
		if v, ok := vars[m[1]]; ok {
			return v
		}
		return ref
	})
}
