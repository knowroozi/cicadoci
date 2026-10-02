// Package secretexpr defines the canonical syntax for referencing a stored
// secret from inside a workflow's environment, and the two operations that
// operate on it:
//
//	Refs (server side): extract the distinct list of referenced secret NAMES
//	  from an env map. The server writes this list to the job's `secret_refs`
//	  column. Secret VALUES are never stored on the job row or in the column.
//
//	Resolve (runner side): substitute `secrets.NAME` references in an env map
//	  with the actual secret VALUES that the runner fetched via the lease-scoped
//	  GetJobSecrets RPC and holds in memory for the life of the job.
//
// This package is the single source of truth for the `secrets.` syntax, wired
// by both the server controller (Refs → secret_refs, CIC-79 Part A) and the
// runner executor (Resolve, CIC-79 Part B/C).
//
// Two reference forms are accepted in an env value:
//
//   - the bare form:      DEPLOY_ROLE: secrets.PROD_DEPLOY_ROLE
//   - the template form:  DEPLOY_ROLE: ${'${{'} secrets.PROD_DEPLOY_ROLE {@}}}
//
// A reference whose name is absent from the secrets map is left as the literal
// reference text, so a child process sees a syntactically inert placeholder
// rather than a missing environment entry. Because the server is authoritative
// about which refs a job carries (secret_refs), an unresolvable ref on a
// correctly created job does not arise in practice.
//
// The package is a leaf: it imports only the standard library.
package secretexpr

import (
	"sort"
	"strings"
)

// prefix is the literal marker that marks an env value as a secret reference.
const prefix = "secrets."

// validName reports whether s is a syntactically valid secret reference name:
// non-empty and composed only of characters permitted in both a Go identifier
// and a Postgres identifier, keeping references portable and unambiguous.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !allowedNameRune(r) {
			return false
		}
	}
	return true
}

// allowedNameRune reports whether r is permitted inside a secret name.
func allowedNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' ||
		r == '_' || r == '-'
}

// Refs returns the distinct, sorted list of secret NAMES referenced by the
// values in env. Values that are not references are ignored.
//
// The extraction is intentionally strict: a value is a reference only if it is
// either exactly `secrets.NAME` or the full-line template `${{ secrets.NAME }}`
// (optional surrounding whitespace). A `secrets.FRAGMENT` embedded within a
// larger string is NOT treated as a reference, because it cannot be
// unambiguously rewritten by Resolve and would otherwise leak a partial
// reference into the child environment.
//
// Returns nil when no reference is present, matching the store's empty-array
// default so the column can stay as-is.
func Refs(env map[string]string) []string {
	seen := make(map[string]struct{})
	for _, v := range env {
		if name, ok := extract(v); ok {
			seen[name] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// extract returns the secret name referenced by value v and true when v is a
// reference; otherwise ("", false). See the package doc for the accepted forms.
func extract(v string) (string, bool) {
	// Trim once: whitespace padding may surround either form, and it must not
	// defeat the boundary checks for either.
	v = strings.TrimSpace(v)
	// Bare form: "secrets.NAME" — the whole value is the reference.
	if strings.HasPrefix(v, prefix) {
		name := strings.TrimPrefix(v, prefix)
		if validName(name) {
			return name, true
		}
		return "", false
	}
	// Template form: "${{ secrets.NAME }}".
	if strings.HasPrefix(v, "${{") && strings.HasSuffix(v, "}}") {
		inner := strings.TrimSpace(v[len("${{") : len(v)-len("}}")])
		if name, ok := cutPrefix(inner, prefix); ok && validName(name) {
			return name, true
		}
	}
	return "", false
}

// cutPrefix reports whether s begins with p and, if so, returns the remainder.
func cutPrefix(s, p string) (string, bool) {
	if strings.HasPrefix(s, p) {
		return s[len(p):], true
	}
	return "", false
}

// Resolve substitutes the secret references in env with values from secrets,
// returning a new map. It neither mutates env nor secrets. A reference whose
// name is absent from secrets is returned verbatim so the child sees a
// syntactically inert placeholder rather than a missing environment value.
//
// Returns nil when env is empty (mirroring the store's empty-map default so
// callers can pass the result straight to the child environment).
func Resolve(env map[string]string, secrets map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if name, ok := extract(v); ok {
			if val, present := secrets[name]; present {
				out[k] = val
				continue
			}
			// Unresolved: keep the literal reference text.
		}
		out[k] = v
	}
	return out
}

// IsRef reports whether v is a secret reference in either the bare
// `secrets.NAME` form or the template form `${{ secrets.NAME }}`.
// It is the single-source-of-truth predicate used by:
//
//  1. fork.go's scrubSecrets to remove secret refs from fork-PR jobs, and
//  2. controller secretRefsFor when accumulating the set of refs from
//     multiple step envs (each step's env is evaluated independently so
//     that two steps using the same key with different secret refs are
//     both captured).
func IsRef(v string) bool {
	_, ok := extract(v)
	return ok
}

// Extract returns the secret name and true when v is a reference; otherwise
// ("", false). It is the exported counterpart to the internal extract and
// is used by the runner to detect unresolved references after Resolve.
func Extract(v string) (string, bool) {
	return extract(v)
}
