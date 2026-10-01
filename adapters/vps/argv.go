package vps

import (
	"errors"
	"fmt"
	"strings"
)

// splitCommand splits a spec start command into argv, honoring plain words,
// single and double quotes, and backslash escapes like a POSIX shell. The
// command is then exec'd directly, so shell syntax that needs a real shell
// (pipes, redirects, variables, ...) is rejected rather than passed through
// as literal arguments.
func splitCommand(command string) ([]string, error) {
	var (
		args    []string
		current strings.Builder
		inWord  bool
		quote   rune // 0, '\'' or '"'
		escaped bool
	)
	for _, r := range command {
		switch {
		case escaped:
			if quote == '"' && !strings.ContainsRune("$`\"\\\n", r) {
				current.WriteRune('\\')
			}
			current.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			case '$', '`':
				return nil, shellSyntaxError(r)
			default:
				current.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				args = append(args, current.String())
				current.Reset()
				inWord = false
			}
		case strings.ContainsRune("|&;<>()$`*?#", r):
			return nil, shellSyntaxError(r)
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if escaped {
		return nil, errors.New("trailing backslash")
	}
	if inWord {
		args = append(args, current.String())
	}
	if len(args) == 0 {
		return nil, errors.New("command is empty")
	}
	return args, nil
}

func shellSyntaxError(r rune) error {
	return fmt.Errorf("uses shell syntax %q, which needs a shell; wrap it as: sh -c '...'", r)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@,+%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
