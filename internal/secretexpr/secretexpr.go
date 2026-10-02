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
// Three reference forms are accepted in an env value:
//
//   - the bare form:            DEPLOY_ROLE: secrets.PROD_DEPLOY_ROLE
//   - the whole-line template:  DEPLOY_ROLE: ${'${{'} secrets.PROD_DEPLOY_ROLE {@}}}
//   - an embedded template:
//     Authorization: Bearer ${{ secrets.API_TOKEN }}
//     DATABASE_URL: postgres://u:${{ secrets.PW }}@db:5432/x
//
// The embedded form is what GitHub-Actions users write most often; before
// CIC-135 it was silently passed through as literal text (so the step saw the
// string `${{ secrets.PW }}` instead of the value and failed later with an
// opaque auth error). CIC-135 scans every `${{ secrets.NAME }}` occurrence in a
// value, extracts the names, and substitutes each occurrence in place.
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
	"regexp"
	"sort"
	"strings"
)

// prefix is the literal marker that marks an env value as a secret reference.
const prefix = "secrets."

// secretTemplateRe matches a GitHub-style template occurrence
// `${{ secrets.NAME }}` anywhere in a value, with optional inner whitespace.
// The leading dollar is part of the GitHub expression syntax. The name is
// captured; NAME follows the same permitted-character rules as a bare
// reference (validName).
var secretTemplateRe = regexp.MustCompile(`\$\{\{\s*secrets\.([A-Za-z0-9_-]+)\s*\}\}`)

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
// values in env, including any embedded `${{ secrets.NAME }}` occurrences
// (CIC-135). Values that reference no secret are ignored.
//
// A bare `secrets.FRAGMENT` that is only part of a larger string (not a
// template and not the whole value) is NOT treated as a reference; only the
// template form may be embedded. This keeps the set of names unambiguous and
// avoids treating arbitrary substring matches as secret references.
//
// Returns nil when no reference is present, matching the store's empty-array
// default so the column can stay as-is.
func Refs(env map[string]string) []string {
	return distinctSortedNames(env)
}

// distinctSortedNames collects every referenced name across the map's values,
// de-duplicated and sorted (for a stable secret_refs).
func distinctSortedNames(env map[string]string) []string {
	seen := make(map[string]struct{})
	for _, v := range env {
		for _, n := range NamesIn(v) {
			seen[n] = struct{}{}
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

// NamesIn returns every distinct secret NAME referenced by a single value v,
// in first-seen order: first any embedded `${{ secrets.NAME }}` template
// occurrences, then the bare whole-value form `secrets.NAME` when present
// (the bare form is only considered when the value is not already a template,
// mirroring extract's original semantics). It is the exported source of truth
// used by Refs and by the runner's unresolved-secret check (so an embedded
// reference with no matching secret is still flagged).
func NamesIn(v string) []string {
	var names []string
	seen := make(map[string]struct{})
	add := func(n string) {
		if n == "" {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	// Embedded template occurrences, in order of appearance.
	for _, sub := range secretTemplateRe.FindAllStringSubmatch(v, -1) {
		add(sub[1])
	}
	// Bare whole-value form, only when no template form is present, so a value
	// like "token=secrets.X; url=https://..." is not treated as a bare ref.
	if len(names) == 0 {
		if n, ok := bareName(v); ok {
			add(n)
		}
	}
	return names
}

// bareName returns the name of a bare whole-value reference `secrets.NAME`
// (optional surrounding whitespace) and true, otherwise ("", false).
func bareName(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, prefix) {
		return "", false
	}
	name := v[len(prefix):]
	if validName(name) {
		return name, true
	}
	return "", false
}

// Resolve substitutes the secret references in env with values from secrets,
// returning a new map. It neither mutates env nor secrets. For each value,
// every embedded `${{ secrets.NAME }}` occurrence is replaced with the fetched
// value; if the whole value is the bare form `secrets.NAME`, the whole value is
// replaced. A reference whose name is absent from secrets is returned verbatim
// so the child sees a syntactically inert placeholder rather than a missing
// environment value.
//
// Returns nil when env is empty (mirroring the store's empty-map default so
// callers can pass the result straight to the child environment).
func Resolve(env map[string]string, secrets map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = resolveValue(v, secrets)
	}
	return out
}

// resolveValue substitutes every secret occurrence in a single value.
func resolveValue(v string, secrets map[string]string) string {
	// Embedded templates take precedence and are substituted in place. An
	// occurrence whose name is not in secrets is left as its literal template
	// text, so the child sees an inert placeholder rather than a dangling
	// secret name.
	if secretTemplateRe.MatchString(v) {
		return secretTemplateRe.ReplaceAllStringFunc(v, func(m string) string {
			name := templateName(m)
			if val, present := secrets[name]; present {
				return val
			}
			return m // unresolved: keep the literal template.
		})
	}
	// No template: the bare whole-value form, if any, is substituted wholesale.
	if name, ok := bareName(v); ok {
		if val, present := secrets[name]; present {
			return val
		}
	}
	return v
}

// templateName extracts the secret NAME from a full template match string of
// the shape `${{ secrets.NAME }}` (secretTemplateRe guarantees the shape).
func templateName(m string) string {
	rest := strings.TrimSuffix(strings.TrimSpace(m), "}}")
	rest = strings.TrimPrefix(rest, "${{")
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, prefix)
	return strings.TrimSpace(rest)
}

// extract returns the secret name referenced by value v and true when v is a
// reference; otherwise ("", false). It returns the first name from NamesIn.
func extract(v string) (string, bool) {
	names := NamesIn(v)
	if len(names) == 0 {
		return "", false
	}
	return names[0], true
}

// IsRef reports whether v references any secret, in the bare `secrets.NAME`
// form, the whole-line template `${{ secrets.NAME }}`, or an embedded
// `${{ secrets.NAME }}` occurrence somewhere inside the value (CIC-135).
// It is the single-source-of-truth predicate used by:
//
//  1. fork.go's scrubSecrets to remove secret refs from fork-PR jobs, and
//  2. controller secretRefsFor when accumulating the set of refs from
//     multiple step envs (each step's env is evaluated independently so
//     that two steps using the same key with different secret refs are
//     both captured).
func IsRef(v string) bool {
	return len(NamesIn(v)) > 0
}

// Extract returns the secret name and true when v is a reference; otherwise
// ("", false). It is the exported counterpart to the internal extract and is
// kept for callers that only need a single name; NamesIn is preferred because
// it reports every reference in a value.
func Extract(v string) (string, bool) {
	return extract(v)
}
