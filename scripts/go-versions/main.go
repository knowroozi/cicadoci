// Command go-versions guards against the failure mode CIC-149 describes: go.mod's
// `go` directive and the Dockerfile's `golang:` build-base disagree, so `docker build`
// fails before the compiler even runs.  The build toolchain in the image must be >= the
// version go.mod requires, because GOTOOLCHAIN=local forbids fetching a newer toolchain
// at build time. Mirrors `make check-proto-versions` in spirit.
//
// Usage: go-versions [go.mod] [Dockerfile]
// (paths default to ./go.mod and ./Dockerfile).  Exit 0 when every `golang:` base is
// >= the go.mod `go` version; exit 1 with a specific message otherwise.
package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Ver is a parsed Go version (X.Y.Z, any field optional and defaulting to 0).
type Ver struct {
	major, minor, patch int
}

// String renders the version as "X.Y" (Go's toolchain-string convention, which omits a
// zero patch).
func (v Ver) String() string {
	if v.patch == 0 {
		return fmt.Sprintf("%d.%d", v.major, v.minor)
	}
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// parseVersion parses "1.26", "1.26.0", "1.26.0+build" (a '+' build suffix is ignored).
// A distro tag such as "bullseye" or "latest" is rejected, because a golang: image so
// tagged would silently pin an unknown toolchain.
func parseVersion(s string) (Ver, error) {
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return Ver{}, fmt.Errorf("malformed version %q", s)
	}
	var v Ver
	fields := []*int{&v.major, &v.minor, &v.patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Ver{}, fmt.Errorf("non-numeric version component %q in %q", p, s)
		}
		*fields[i] = n
	}
	return v, nil
}

// goDirective finds the first `go X.Y(.Z)` line and parses its version.
func goDirective(content string) (Ver, error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "go" {
			continue
		}
		v, err := parseVersion(fields[1])
		if err != nil {
			return Ver{}, fmt.Errorf("go.mod `go` directive is not a version: %q", fields[1])
		}
		return v, nil
	}
	return Ver{}, fmt.Errorf("go.mod has no `go` directive")
}

// fromGolangRe matches a `FROM golang:<tag>` stage line and captures the tag. It tolerates the
// documented `--platform=` and any other `--flag=value` tokens that Docker's FROM grammar allows
// between `FROM` and the image ref — e.g. `FROM --platform=$BUILDPLATFORM golang:1.24` for
// multi-arch builds. Without this, such a line silently escapes the guard: the exact bypass this
// guard exists to prevent. The version capture stops at the first non-[0-9._A-Za-z] char, so
// `golang:1.24-bookworm` still captures `1.24`.
var fromGolangRe = regexp.MustCompile(`(?i)^\s*FROM(?:\s+--[A-Za-z][A-Za-z0-9_-]*=[^\s]+)*\s+golang:([A-Za-z0-9._]+)`)

// golangBases parses the tags of every `FROM golang:<tag>` line. Non-golang FROMs
// (debian, distroless) are ignored. A non-parseable golang tag is a hard failure.
func golangBases(content string) ([]Ver, error) {
	var bases []Ver
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := fromGolangRe.FindStringSubmatch(line); m != nil {
			v, err := parseVersion(m[1])
			if err != nil {
				return nil, fmt.Errorf("golang base image for tag golang:%s has an unparseable version: %w", m[1], err)
			}
			bases = append(bases, v)
		}
	}
	return bases, nil
}

// Check compares every golang: base against the go.mod `go` directive and errors when any
// base is older, when a version can't be parsed, or when neither a directive nor a golang:
// base is present (an ambiguous state that can't be verified).
func Check(goMod, dockerfile string) error {
	goContent, err := os.ReadFile(goMod)
	if err != nil {
		return fmt.Errorf("read %s: %w", goMod, err)
	}
	dfContent, err := os.ReadFile(dockerfile)
	if err != nil {
		return fmt.Errorf("read %s: %w", dockerfile, err)
	}
	required, err := goDirective(string(goContent))
	if err != nil {
		return err
	}
	bases, err := golangBases(string(dfContent))
	if err != nil {
		return err
	}
	if len(bases) == 0 {
		return fmt.Errorf("%s has no `FROM golang:<tag>` build base to verify against %s's `go %s` requirement",
			dockerfile, goMod, required.String())
	}
	for _, base := range bases {
		if base.lessThan(required) {
			return fmt.Errorf("golang build base golang:%s is older than go %s required by %s; "+
				"(the build toolchain must be >= go.mod's required toolchain with GOTOOLCHAIN=local); "+
				"bump the image base to >= %s or lower the `go` directive",
				base.String(), required.String(), goMod, required.String())
		}
	}
	summary := make([]string, len(bases))
	for i, b := range bases {
		summary[i] = b.String()
	}
	fmt.Printf("go-versions: OK -- `go` directive is %s; golang: bases are %s (all >= required)\n",
		required.String(), strings.Join(summary, ", "))
	return nil
}

// lessThan reports whether v < other by major, then minor, then patch.
func (v Ver) lessThan(other Ver) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	return v.patch < other.patch
}

func main() {
	goMod, dockerfile := "go.mod", "Dockerfile"
	if len(os.Args) >= 2 {
		goMod = os.Args[1]
	}
	if len(os.Args) >= 3 {
		dockerfile = os.Args[2]
	}
	wd := cwd()
	if _, err := os.Stat(goMod); err != nil {
		fmt.Fprintf(os.Stderr, "go-versions: %v (cwd=%q)\n", err, wd)
		os.Exit(2)
	}
	if _, err := os.Stat(dockerfile); err != nil {
		fmt.Fprintf(os.Stderr, "go-versions: %v (cwd=%q)\n", err, wd)
		os.Exit(2)
	}
	if err := Check(goMod, dockerfile); err != nil {
		fmt.Fprintf(os.Stderr, "go-versions: FAIL -- %v\n", err)
		os.Exit(1)
	}
}

// cwd returns the current working directory, or "?" on error (error messages only).
func cwd() string {
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "?"
}
