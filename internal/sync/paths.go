package sync

import (
	"bytes"
	"path"
	"regexp"
	"runtime"
	"strings"
)

// PathMapper translates machine-specific paths to portable tokens on push and
// back to local paths on pull, so a session started on one machine resumes on
// another even when the home directory differs (macOS /Users/edwardcurt vs
// Windows C:\Users\edward.curt).
//
// Claude Code stores sessions under ~/.claude/projects/<encoded-cwd>/ where
// <encoded-cwd> is the working directory with every non-alphanumeric character
// replaced by "-". Two things are rewritten:
//
//   - remote keys:  projects/-Users-alice-Code-app/... -> projects/${HOME}-Code-app/...
//   - file content: the home prefix becomes ${HOME}; anything under the code
//     root (<home>/Code) becomes ${HOME}/Code/<rest> with forward slashes.
//
// The key form and the ${HOME} token are exactly what v1 wrote, so v1 and v2
// machines share one bucket. The code root is built in (rules, not config):
// <home>/Code on every OS.
//
// Windows needs care v1 lacked: inside JSON a Windows path is written with
// escaped backslashes (C:\\Users\\x), and resolving ${HOME} to a raw
// C:\Users\x inside JSON produces invalid escapes. v2 normalizes both escaped
// and forward-slash Windows forms, and resolves to properly escaped native
// paths in JSON files.
type PathMapper struct {
	home     string // absolute home dir, no trailing separator
	encHome  string // home in Claude Code's directory encoding
	windows  bool
	unixHome *regexp.Regexp // v1 home rule: literal home + boundary

	// Windows rules, per content format.
	winJSON winRules
	winText winRules
}

type winRules struct {
	sep     string         // literal separator as written in this format
	normCode *regexp.Regexp // <home><sep>Code(<sep>rest)*<boundary>
	normHome *regexp.Regexp // <home><boundary>
}

// CodeDir is the code root under the home directory, on every OS.
const CodeDir = "Code"

const (
	homeToken   = "${HOME}"
	tokenPrefix = "${"
)

// path characters allowed inside a code path segment when rewriting
// separators. Whitespace, quotes and backslashes end a path; a backslash that
// is not a separator is a JSON escape such as \n.
const segChars = `[^\\/"'\s<>|*?:\x00-\x1f]`

var (
	resolveCodeRe = regexp.MustCompile(regexp.QuoteMeta(homeToken+"/"+CodeDir) + `((?:/` + segChars + `*)*)`)
	nameCharRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]`)
)

// NewPathMapper builds the mapper for this machine.
func NewPathMapper(homeDir string) *PathMapper {
	return NewPathMapperFor(homeDir, runtime.GOOS == "windows")
}

// NewPathMapperFor builds a mapper for an explicit OS (tests simulate Windows).
func NewPathMapperFor(homeDir string, windows bool) *PathMapper {
	homeDir = strings.TrimRight(homeDir, `/\`)
	if homeDir == "" {
		return nil
	}
	m := &PathMapper{
		home:    homeDir,
		encHome: EncodeClaudePath(homeDir),
		windows: windows,
		unixHome: regexp.MustCompile(regexp.QuoteMeta(homeDir) + `([^A-Za-z0-9_.-]|$)`),
	}
	if windows {
		m.winJSON = newWinRules(homeDir, `\\`)
		m.winText = newWinRules(homeDir, `\`)
	}
	return m
}

func newWinRules(home, sep string) winRules {
	sepRe := `(?:` + regexp.QuoteMeta(sep) + `|/)`
	parts := strings.FieldsFunc(home, func(r rune) bool { return r == '\\' || r == '/' })
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = regexp.QuoteMeta(p)
	}
	homeRe := strings.Join(quoted, sepRe)
	return winRules{
		sep:      sep,
		normCode: regexp.MustCompile(`(?i)` + homeRe + sepRe + CodeDir + `((?:` + sepRe + segChars + `*)*)`),
		normHome: regexp.MustCompile(`(?i)` + homeRe + `([^A-Za-z0-9_.-]|$)`),
	}
}

// EncodeClaudePath applies Claude Code's project directory encoding: every
// character outside [A-Za-z0-9] becomes "-".
func EncodeClaudePath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// splitProjectsPath splits "projects/<seg>/rest" into seg and "/rest".
func splitProjectsPath(relPath string) (seg, rest string, ok bool) {
	const prefix = "projects/"
	if !strings.HasPrefix(relPath, prefix) {
		return "", "", false
	}
	remainder := relPath[len(prefix):]
	if i := strings.IndexByte(remainder, '/'); i >= 0 {
		return remainder[:i], remainder[i:], true
	}
	return remainder, "", true
}

// NormalizeRelPath rewrites a local relative path to its portable remote form.
// Only the projects/<encoded-cwd> segment is affected.
func (m *PathMapper) NormalizeRelPath(relPath string) string {
	if m == nil {
		return relPath
	}
	seg, rest, ok := splitProjectsPath(relPath)
	if !ok || strings.HasPrefix(seg, tokenPrefix) {
		return relPath
	}
	if m.segHasHome(seg) {
		return "projects/" + homeToken + seg[len(m.encHome):] + rest
	}
	return relPath
}

func (m *PathMapper) segHasHome(seg string) bool {
	n := len(m.encHome)
	if len(seg) < n {
		return false
	}
	head := seg[:n]
	match := head == m.encHome || (m.windows && strings.EqualFold(head, m.encHome))
	return match && (len(seg) == n || seg[n] == '-')
}

// ResolveRelPath rewrites a portable remote path back to a local relative
// path. ok is false for a token this machine does not know (v1 path_map
// tokens); the caller skips those.
func (m *PathMapper) ResolveRelPath(relPath string) (string, bool) {
	if m == nil {
		return relPath, true
	}
	seg, rest, isProject := splitProjectsPath(relPath)
	if !isProject || !strings.HasPrefix(seg, tokenPrefix) {
		return relPath, true
	}
	if strings.HasPrefix(seg, homeToken) {
		return "projects/" + m.encHome + seg[len(homeToken):] + rest, true
	}
	return relPath, false
}

// IsPortableContentPath reports whether content translation applies: text
// formats under projects/ plus the prompt history.
func IsPortableContentPath(relPath string) bool {
	if relPath == "history.jsonl" {
		return true
	}
	if !strings.HasPrefix(relPath, "projects/") {
		return false
	}
	switch path.Ext(relPath) {
	case ".jsonl", ".json", ".md", ".txt":
		return true
	}
	return false
}

func isJSONPath(relPath string) bool {
	ext := path.Ext(relPath)
	return ext == ".jsonl" || ext == ".json"
}

// NormalizeContent converts this machine's paths to portable tokens.
func (m *PathMapper) NormalizeContent(relPath string, data []byte) []byte {
	if m == nil || !IsPortableContentPath(relPath) {
		return data
	}
	if !m.windows {
		return m.unixHome.ReplaceAll(data, []byte("$${HOME}${1}"))
	}
	r := m.winText
	if isJSONPath(relPath) {
		r = m.winJSON
	}
	data = replaceCode(r.normCode, data, func(rest []byte) []byte {
		rest = bytes.ReplaceAll(rest, []byte(r.sep), []byte("/"))
		return append([]byte(homeToken+"/"+CodeDir), rest...)
	})
	return r.normHome.ReplaceAll(data, []byte("$${HOME}${1}"))
}

// ResolveContent converts portable tokens to this machine's paths.
func (m *PathMapper) ResolveContent(relPath string, data []byte) []byte {
	if m == nil || !IsPortableContentPath(relPath) {
		return data
	}
	if !m.windows {
		return bytes.ReplaceAll(data, []byte(homeToken), []byte(m.home))
	}
	sep := `\`
	if isJSONPath(relPath) {
		sep = `\\`
	}
	nativeHome := []byte(strings.ReplaceAll(m.home, `\`, sep))
	data = replaceCode(resolveCodeRe, data, func(rest []byte) []byte {
		rest = bytes.ReplaceAll(rest, []byte("/"), []byte(sep))
		out := append([]byte{}, nativeHome...)
		out = append(out, sep...)
		out = append(out, CodeDir...)
		return append(out, rest...)
	})
	return bytes.ReplaceAll(data, []byte(homeToken), nativeHome)
}

// replaceCode replaces each match of re (whose group 1 is the path remainder)
// with build(remainder), skipping matches where the code dir name continues
// (e.g. "Codex"): the char after "Code" must not be a name character.
func replaceCode(re *regexp.Regexp, data []byte, build func(rest []byte) []byte) []byte {
	locs := re.FindAllSubmatchIndex(data, -1)
	if len(locs) == 0 {
		return data
	}
	var out bytes.Buffer
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if loc[3] == loc[2] && end < len(data) && nameCharRe.Match(data[end:end+1]) {
			continue
		}
		out.Write(data[last:start])
		out.Write(build(data[loc[2]:loc[3]]))
		last = end
	}
	out.Write(data[last:])
	return out.Bytes()
}

// RepairWindowsJSON fixes JSON lines that v1 broke on Windows by writing the
// raw home path (C:\Users\x, single backslashes) into JSON strings. Only lines
// that are invalid JSON and become valid after the fix are changed. Returns
// the repaired data and the number of lines fixed.
func (m *PathMapper) RepairWindowsJSON(relPath string, data []byte, valid func([]byte) bool) ([]byte, int) {
	if m == nil || !m.windows || !isJSONPath(relPath) || !IsPortableContentPath(relPath) {
		return data, 0
	}
	raw := []byte(m.home)
	if !bytes.Contains(data, raw) {
		return data, 0
	}
	escaped := []byte(strings.ReplaceAll(m.home, `\`, `\\`))
	lines := bytes.Split(data, []byte("\n"))
	fixed := 0
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 || !bytes.Contains(line, raw) || valid(line) {
			continue
		}
		candidate := bytes.ReplaceAll(line, raw, escaped)
		candidate = m.ResolveContent(relPath, m.NormalizeContent(relPath, candidate))
		if valid(candidate) {
			lines[i] = candidate
			fixed++
		}
	}
	if fixed == 0 {
		return data, 0
	}
	return bytes.Join(lines, []byte("\n")), fixed
}
