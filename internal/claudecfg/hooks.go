package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// HookEntry is one PreToolUse entry in settings.json: a tool matcher and the
// commands to run for it.
type HookEntry struct {
	Matcher string    `json:"matcher"`
	Hooks   []HookCmd `json:"hooks"`
}

// HookCmd is one command inside a HookEntry.
type HookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// WhyfailHooks is the pair of PreToolUse entries the plugin's hooks.json
// declares. `whyfail init --hooks-only` writes exactly these, so the two
// install paths behave identically.
func WhyfailHooks() []HookEntry {
	return []HookEntry{
		{Matcher: "Bash", Hooks: []HookCmd{{Type: "command", Command: HookPrefix + "bash"}}},
		{Matcher: "WebFetch", Hooks: []HookCmd{{Type: "command", Command: HookPrefix + "webfetch"}}},
	}
}

// MergeHooks adds any of add that settings does not already contain. The file
// is edited in place: only the byte range of the affected JSON value changes,
// so the hand-maintained ordering of the user's other keys survives, and a
// second call reports changed == false.
func MergeHooks(settings []byte, add []HookEntry) ([]byte, bool, error) {
	hsp, hasHooks, err := findMember(settings, "hooks")
	if err != nil {
		return nil, false, err
	}
	hooksRaw := []byte("{}")
	// base is the indentation the hooks key will end up at, so a newly created
	// hooks object is laid out for its final position rather than column zero.
	base := memberIndent(settings)
	if hasHooks {
		hooksRaw = append([]byte(nil), settings[hsp.start:hsp.end]...)
		base = lineIndent(settings, hsp.start)
	}

	newHooks, changed, err := mergePreToolUse(hooksRaw, base, add)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return settings, false, nil
	}
	if hasHooks {
		return splice(settings, hsp, newHooks), true, nil
	}
	out, err := insertMember(settings, "hooks", newHooks, memberIndent(settings))
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func mergePreToolUse(hooksRaw []byte, base string, add []HookEntry) ([]byte, bool, error) {
	psp, hasPT, err := findMember(hooksRaw, "PreToolUse")
	if err != nil {
		return nil, false, err
	}
	var entries []HookEntry
	if hasPT {
		if err := json.Unmarshal(hooksRaw[psp.start:psp.end], &entries); err != nil {
			return nil, false, fmt.Errorf("hooks.PreToolUse: %w", err)
		}
	}

	changed := false
	for _, a := range add {
		if hasEntry(entries, a) {
			continue
		}
		entries = append(entries, a)
		changed = true
	}
	if !changed {
		return hooksRaw, false, nil
	}

	prefix := base + indentUnit(base)
	if hasPT {
		prefix = lineIndent(hooksRaw, psp.start)
	}
	val, err := marshalIndent(entries, prefix, indentUnit(prefix))
	if err != nil {
		return nil, false, err
	}
	if hasPT {
		return splice(hooksRaw, psp, val), true, nil
	}
	out, err := insertMember(hooksRaw, "PreToolUse", val, base+indentUnit(base))
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// hasEntry reports whether entries already covers want.
func hasEntry(entries []HookEntry, want HookEntry) bool {
	for _, e := range entries {
		if e.Matcher != want.Matcher {
			continue
		}
		for _, h := range e.Hooks {
			for _, w := range want.Hooks {
				if sameHookCommand(h.Command, w.Command) {
					return true
				}
			}
		}
	}
	return false
}

// sameHookCommand compares two hook command lines, accepting any path to the
// binary: "/usr/local/bin/whyfail hook bash" already covers
// "whyfail hook bash", and adding a second entry would run the hook twice.
func sameHookCommand(have, want string) bool {
	if have == want {
		return true
	}
	hf, wf := strings.Fields(have), strings.Fields(want)
	if len(hf) == 0 || len(hf) != len(wf) {
		return false
	}
	for i := range wf {
		a := strings.Trim(hf[i], `"'`)
		if i == 0 {
			if a != wf[i] && filepath.Base(a) != wf[i] {
				return false
			}
			continue
		}
		if a != wf[i] {
			return false
		}
	}
	return true
}

// RemoveHooks drops every entry drop accepts, and then the now-empty
// PreToolUse and hooks members, so an uninstall leaves the file as it was.
func RemoveHooks(settings []byte, drop func(HookEntry) bool) ([]byte, bool, error) {
	hsp, hasHooks, err := findMember(settings, "hooks")
	if err != nil || !hasHooks {
		return settings, false, err
	}
	hooksRaw := append([]byte(nil), settings[hsp.start:hsp.end]...)

	psp, hasPT, err := findMember(hooksRaw, "PreToolUse")
	if err != nil || !hasPT {
		return settings, false, err
	}
	var entries []HookEntry
	if err := json.Unmarshal(hooksRaw[psp.start:psp.end], &entries); err != nil {
		return nil, false, fmt.Errorf("hooks.PreToolUse: %w", err)
	}
	kept := make([]HookEntry, 0, len(entries))
	for _, e := range entries {
		if !drop(e) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return settings, false, nil
	}

	var newHooks []byte
	if len(kept) == 0 {
		newHooks = removeMember(hooksRaw, psp)
		if isEmptyObject(newHooks) {
			newHooks = nil
		}
	} else {
		prefix := lineIndent(hooksRaw, psp.start)
		val, err := marshalIndent(kept, prefix, indentUnit(prefix))
		if err != nil {
			return nil, false, err
		}
		newHooks = splice(hooksRaw, psp, val)
	}
	if newHooks == nil {
		return removeMember(settings, hsp), true, nil
	}
	return splice(settings, hsp, newHooks), true, nil
}

// IsWhyfailHook reports whether an entry is one of ours, for RemoveHooks.
func IsWhyfailHook(e HookEntry) bool {
	for _, h := range e.Hooks {
		if hasHookPrefix(h.Command) {
			return true
		}
	}
	return false
}

// ---------- small JSON editing helpers ----------

// span locates one member of a JSON object: the value's bytes and the key's
// opening quote.
type span struct {
	start int
	end   int
	key   int
}

// findMember locates key's value in the JSON object obj.
func findMember(obj []byte, key string) (span, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return span{}, false, errors.New("empty document")
		}
		return span{}, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return span{}, false, errors.New("not a JSON object")
	}
	for dec.More() {
		before := dec.InputOffset()
		kt, err := dec.Token()
		if err != nil {
			return span{}, false, err
		}
		k, ok := kt.(string)
		if !ok {
			return span{}, false, errors.New("object key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return span{}, false, err
		}
		end := int(dec.InputOffset())
		if k == key {
			return span{start: end - len(raw), end: end, key: nextToken(obj, int(before))}, true, nil
		}
	}
	return span{}, false, nil
}

// nextToken returns the index of the first byte at or after from that is
// neither whitespace nor a separating comma.
func nextToken(obj []byte, from int) int {
	i := from
	for i < len(obj) && (isSpace(obj[i]) || obj[i] == ',') {
		i++
	}
	return i
}

// lineIndent returns the leading whitespace of the line containing at.
func lineIndent(obj []byte, at int) string {
	if at > len(obj) {
		at = len(obj)
	}
	start := 0
	if i := bytes.LastIndexByte(obj[:at], '\n'); i >= 0 {
		start = i + 1
	}
	i := start
	for i < len(obj) && (obj[i] == ' ' || obj[i] == '\t') {
		i++
	}
	return string(obj[start:i])
}

// indentUnit picks the indent step a file already uses, so inserted members
// match the surrounding style.
func indentUnit(indent string) string {
	for i := 0; i < len(indent); i++ {
		if indent[i] == '\t' {
			return "\t"
		}
	}
	return "  "
}

// memberIndent is the indentation of obj's first nested member, which is what
// a new sibling member should use.
func memberIndent(obj []byte) string {
	if i := bytes.IndexByte(obj, '\n'); i >= 0 {
		j := i + 1
		for j < len(obj) && (obj[j] == ' ' || obj[j] == '\t') {
			j++
		}
		if j > i+1 {
			return string(obj[i+1 : j])
		}
	}
	return "  "
}

func splice(obj []byte, sp span, value []byte) []byte {
	out := make([]byte, 0, len(obj)-(sp.end-sp.start)+len(value))
	out = append(out, obj[:sp.start]...)
	out = append(out, value...)
	out = append(out, obj[sp.end:]...)
	return out
}

// insertMember adds key to the object obj, indenting the new member by ind and
// keeping the closing brace on its own line. ind is passed rather than sniffed
// because obj may be a rewritten "{}" whose members belong at their parent's
// indent, not at column zero.
func insertMember(obj []byte, key string, value []byte, ind string) ([]byte, error) {
	close := bytes.LastIndexByte(obj, '}')
	if close < 0 {
		return nil, errors.New("not a JSON object")
	}
	head := bytes.TrimRight(obj[:close], " \t\r\n")
	sep := ",\n"
	if isEmptyObjectBytes(head) {
		sep = "\n"
		head = []byte("{")
	}
	closeInd := strings.TrimSuffix(ind, indentUnit(ind))
	if bytes.ContainsAny(obj[:close], "\n") {
		closeInd = lineIndent(obj, close)
	}
	out := make([]byte, 0, len(obj)+len(value)+32)
	out = append(out, head...)
	out = append(out, sep...)
	out = append(out, ind...)
	out = append(out, quoteKey(key)...)
	out = append(out, ": "...)
	out = append(out, value...)
	out = append(out, '\n')
	out = append(out, closeInd...)
	out = append(out, '}')
	out = append(out, obj[close+1:]...)
	return out, nil
}

func quoteKey(k string) string {
	b, _ := json.Marshal(k)
	return string(b)
}

// removeMember deletes key's member from obj, taking the adjoining separator
// with it so the result stays valid JSON.
func removeMember(obj []byte, sp span) []byte {
	start := sp.key
	j := start - 1
	for j >= 0 && isSpace(obj[j]) {
		j--
	}
	if j >= 0 && obj[j] == ',' {
		start = j
		return splice(obj, span{start: start, end: sp.end}, nil)
	}
	// First member: consume the comma that follows it instead.
	end := sp.end
	for end < len(obj) && isSpace(obj[end]) {
		end++
	}
	if end < len(obj) && obj[end] == ',' {
		end++
		for end < len(obj) && isSpace(obj[end]) {
			end++
		}
	}
	out := splice(obj, span{start: start, end: end}, nil)
	if isEmptyObject(out) {
		return []byte("{}")
	}
	return out
}

func isEmptyObject(obj []byte) bool {
	t := bytes.TrimSpace(obj)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
		return false
	}
	return len(bytes.TrimSpace(t[1:len(t)-1])) == 0
}

// marshalIndent encodes v without HTML escaping. Hook commands are shell
// fragments, and json.MarshalIndent would turn "a && b" into "a && b",
// changing lines init did not mean to touch.
func marshalIndent(v any, prefix, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// isEmptyObjectBytes reports whether obj, an object being built, has no
// members yet.
func isEmptyObjectBytes(obj []byte) bool {
	trimmed := bytes.TrimSpace(obj)
	return len(trimmed) <= 1
}
