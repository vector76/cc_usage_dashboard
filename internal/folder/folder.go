// Package folder holds the path rules project accounting is built on: how two
// spellings of a working directory are recognized as the same folder, how a
// folder inherits the project of its nearest assigned ancestor, and how
// Claude Code's encoded project-directory names relate to real paths. It is a
// leaf package (no store, no SQL) so the store, the report and the startup
// repair can all share one definition. See docs/projects.md.
package folder

import "strings"

// Key returns the comparison form of a folder path. Two paths with the same
// key are the same folder.
//
//   - Windows paths (a drive letter or a UNC prefix) are case-insensitive and
//     accept either separator, so they are lowercased with '/' turned into
//     '\'. Both C:\WebApps and C:\webapps occur in real transcripts.
//   - POSIX paths stay case-sensitive.
//   - A trailing separator is dropped, except on the POSIX root "/". A drive
//     root "C:\" keys as "c:".
//   - Anything else (an encoded directory name) is returned trimmed but
//     otherwise verbatim.
func Key(p string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return ""
	case isWindows(p):
		p = strings.ToLower(strings.ReplaceAll(p, "/", `\`))
		return strings.TrimRight(p, `\`)
	case strings.HasPrefix(p, "/"):
		if t := strings.TrimRight(p, "/"); t != "" {
			return t
		}
		return "/"
	default:
		return p
	}
}

func isWindows(p string) bool {
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	if len(p) >= 2 && p[1] == ':' {
		c := p[0]
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	return false
}

// parent returns the key one segment up from k, or false at a root. Keys
// without a separator (encoded names, a bare drive "c:") have no parent, so
// an encoded name only ever matches an assignment of exactly that name.
func parent(k string) (string, bool) {
	if isWindows(k) {
		i := strings.LastIndex(k, `\`)
		if i < 2 { // "c:" or the "\\" of a UNC prefix
			return "", false
		}
		return k[:i], true
	}
	i := strings.LastIndex(k, "/")
	switch {
	case i < 0 || k == "/":
		return "", false
	case i == 0:
		return "/", true
	default:
		return k[:i], true
	}
}

// Encode applies Claude Code's rule for naming a project's directory under
// ~/.claude/projects: every character that is not an ASCII letter or digit
// becomes '-'. It is lossy — "a_b", "a.b" and "a\b" all encode alike — which
// is why the hook now sends the real cwd and an encoded name is only mapped
// back when exactly one known path produces it.
func Encode(p string) string {
	b := []byte(p)
	for i, c := range b {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			b[i] = '-'
		}
	}
	return string(b)
}

// IsEncoded reports whether a stored project_path is an encoded directory name
// rather than a path. Real working directories are absolute and so always
// contain a separator; encoded names never do.
func IsEncoded(p string) bool {
	return p != "" && !strings.ContainsAny(p, `/\`)
}

// Assignment links a folder (as the user picked it) to a project.
type Assignment struct {
	Folder    string `json:"folder"`
	ProjectID int64  `json:"project_id"`
}

// Resolver attributes folders to projects by longest assigned prefix.
type Resolver struct {
	byKey map[string]Assignment
}

// NewResolver indexes assignments by key. If two assignments share a key the
// later one wins; the store prevents that from happening.
func NewResolver(as []Assignment) *Resolver {
	r := &Resolver{byKey: make(map[string]Assignment, len(as))}
	for _, a := range as {
		if k := Key(a.Folder); k != "" {
			r.byKey[k] = a
		}
	}
	return r
}

// Resolve returns the assignment covering p: the one on p itself or on its
// nearest ancestor, matched on whole path segments (C:\Foo does not cover
// C:\Foobar). The returned Assignment's Folder is the ancestor it came from.
// False means p is unassigned.
func (r *Resolver) Resolve(p string) (Assignment, bool) {
	k := Key(p)
	for k != "" {
		if a, ok := r.byKey[k]; ok {
			return a, true
		}
		var more bool
		if k, more = parent(k); !more {
			break
		}
	}
	return Assignment{}, false
}
