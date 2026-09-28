package gsocket

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ftSource struct {
	local string
	wire  string
	info  os.FileInfo
}

// splitFTWords accepts quoted paths and backslash escapes without invoking a
// shell. Transfer requests are data, including when they come from a peer.
func splitFTWords(input string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, ch := range input {
		switch {
		case escaped:
			word.WriteRune(ch)
			escaped = false
			started = true
		case ch == '\\' && quote != '\'':
			escaped = true
			started = true
		case (ch == '\'' || ch == '"') && quote == 0:
			quote = ch
			started = true
		case ch == quote:
			quote = 0
		case (ch == ' ' || ch == '\t' || ch == '\n') && quote == 0:
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated file pattern")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

func expandFTBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	close := strings.IndexByte(pattern[open:], '}')
	if close < 0 {
		return []string{pattern}
	}
	close += open
	parts := strings.Split(pattern[open+1:close], ",")
	if len(parts) < 2 {
		return []string{pattern}
	}
	var expanded []string
	for _, part := range parts {
		expanded = append(expanded, expandFTBraces(pattern[:open]+part+pattern[close+1:])...)
	}
	return expanded
}

func expandFTWords(expression, cwd string) ([]string, error) {
	if strings.Contains(expression, "$(") || strings.ContainsRune(expression, '`') {
		return expandFTCommandWords(expression, cwd)
	}
	tokens, err := splitFTWords(expression)
	if err != nil {
		return nil, err
	}
	var words []string
	for _, token := range tokens {
		if token == "" {
			continue
		}
		token = os.ExpandEnv(token)
		if token == "~" || strings.HasPrefix(token, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				token = home + strings.TrimPrefix(token, "~")
			}
		}
		for _, pattern := range expandFTBraces(token) {
			if !strings.ContainsAny(pattern, "*?[") {
				words = append(words, pattern)
				continue
			}
			glob := pattern
			if !filepath.IsAbs(glob) {
				glob = filepath.Join(cwd, glob)
			}
			matches, err := filepath.Glob(glob)
			if err != nil {
				return nil, err
			}
			if len(matches) == 0 {
				words = append(words, pattern)
			} else {
				for _, match := range matches {
					if !filepath.IsAbs(pattern) {
						rel, err := filepath.Rel(cwd, match)
						if err != nil {
							return nil, err
						}
						match = rel
					}
					if marker := strings.Index(pattern, "/./"); marker >= 0 {
						prefix := pattern[:marker]
						suffix := strings.TrimPrefix(match, filepath.Clean(prefix)+string(filepath.Separator))
						match = prefix + "/./" + suffix
					}
					words = append(words, match)
				}
			}
			if len(words) > 100000 {
				return nil, fmt.Errorf("file pattern expands too broadly")
			}
		}
	}
	return words, nil
}

func ftCommonBase(words []string) string {
	if len(words) == 0 {
		return ""
	}
	first := strings.TrimRight(words[0], "/")
	base := filepath.Dir(first)
	if strings.HasSuffix(words[0], "/") {
		base = first
	}
	if base == "." {
		base = ""
	}
	for _, word := range words[1:] {
		other := strings.TrimRight(word, "/")
		for base != "" && other != base && !strings.HasPrefix(other, base+string(filepath.Separator)) {
			parent := filepath.Dir(base)
			if parent == base {
				base = ""
				break
			}
			base = parent
			if base == "." {
				base = ""
			}
		}
	}
	return base
}

func ftWirePath(word, cwd, base string) string {
	if strings.Contains(word, "/./") {
		if filepath.IsAbs(word) {
			return word
		}
		return filepath.ToSlash(cwd) + "/" + word
	}
	clean := strings.TrimRight(word, "/")
	if clean == "" || clean == "." {
		return filepath.ToSlash(cwd) + "/./"
	}
	if filepath.IsAbs(clean) {
		if base == "" || base == "/" {
			return "/./" + strings.TrimLeft(clean, "/")
		}
		return filepath.ToSlash(base) + "/./" + strings.TrimLeft(clean[len(base):], "/")
	}
	if base == "" {
		return filepath.ToSlash(cwd) + "/./" + clean
	}
	return filepath.ToSlash(filepath.Join(cwd, base)) + "/./" + strings.TrimLeft(clean[len(base):], "/")
}

func ftRelativeName(wire string) string {
	if marker := strings.Index(wire, "/./"); marker >= 0 {
		return strings.TrimLeft(wire[marker+3:], "/")
	}
	return strings.TrimLeft(wire, "/")
}

func ftSources(expression, cwd string) ([]ftSource, error) {
	words, err := expandFTWords(expression, cwd)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, os.ErrNotExist
	}
	base := ftCommonBase(words)
	seen := make(map[string]bool)
	var sources []ftSource
	var firstErr error
	for _, word := range words {
		local := word
		if !filepath.IsAbs(local) {
			local = filepath.Join(cwd, local)
		}
		rootWire := ftWirePath(word, cwd, base)
		rootInfo, err := os.Stat(local)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", word, err)
			}
			continue
		}
		if !rootInfo.IsDir() && !rootInfo.Mode().IsRegular() {
			continue
		}
		add := func(path, wire string, info os.FileInfo) {
			if ftRelativeName(wire) == "" || seen[wire] {
				return
			}
			seen[wire] = true
			sources = append(sources, ftSource{path, wire, info})
		}
		add(local, rootWire, rootInfo)
		if !rootInfo.IsDir() {
			continue
		}
		err = filepath.WalkDir(local, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == local || (!entry.IsDir() && !entry.Type().IsRegular()) {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(local, path)
			if err != nil {
				return err
			}
			add(path, strings.TrimRight(rootWire, "/")+"/"+filepath.ToSlash(rel), info)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].wire < sources[j].wire })
	return sources, firstErr
}

func ftDestination(root, wire string) (string, error) {
	rel := ftRelativeName(wire)
	if rel == "" || filepath.IsAbs(rel) {
		return "", errFTPacket
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".." || part == "" {
			return "", errFTPacket
		}
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}
