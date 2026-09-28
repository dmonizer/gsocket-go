//go:build !windows

package gsocket

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// C beta calls wordexp with command execution enabled. The outer expression
// must still be a word list: shell control operators are only valid inside a
// command substitution, as they are with wordexp.
func validFTWordExpression(expression string) bool {
	var quote byte
	var outerQuotes []byte
	var parens []int
	backtick := false
	escaped := false
	for i := 0; i < len(expression); i++ {
		c := expression[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if c == '`' && quote != '\'' {
			backtick = !backtick
			continue
		}
		if backtick {
			continue
		}
		if c == '\'' && quote != '"' {
			if quote == '\'' {
				quote = 0
			} else {
				quote = '\''
			}
			continue
		}
		if c == '"' && quote != '\'' {
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
			continue
		}
		if quote == '\'' {
			continue
		}
		if c == '$' && i+1 < len(expression) && expression[i+1] == '(' {
			outerQuotes = append(outerQuotes, quote)
			parens = append(parens, 1)
			quote = 0
			i++
			continue
		}
		if len(parens) > 0 {
			if quote == 0 {
				last := len(parens) - 1
				if c == '(' {
					parens[last]++
				} else if c == ')' {
					parens[last]--
					if parens[last] == 0 {
						parens = parens[:last]
						quote = outerQuotes[last]
						outerQuotes = outerQuotes[:last]
					}
				}
			}
			continue
		}
		if quote == 0 && strings.ContainsRune(";&|<>\n\r()", rune(c)) {
			return false
		}
	}
	return !escaped && !backtick && len(parens) == 0 && quote == 0
}

type ftLimitedOutput struct{ bytes.Buffer }

func (w *ftLimitedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 1024*1024 {
		return 0, fmt.Errorf("transfer pattern expands beyond 1 MiB")
	}
	return w.Buffer.Write(p)
}

func expandFTCommandWords(expression, cwd string) ([]string, error) {
	if !validFTWordExpression(expression) {
		return nil, fmt.Errorf("invalid transfer word expression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The expression is intentionally evaluated by the local POSIX shell,
	// matching C's wordexp(3), including command substitution. It can arrive
	// from an authenticated remote GET request.
	cmd := exec.CommandContext(ctx, "sh", "-c", "set -- "+expression+"\nprintf '%s\\0' \"$@\"")
	cmd.Dir = cwd
	var output ftLimitedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("transfer pattern expansion timed out: %w", ctx.Err())
		}
		return nil, fmt.Errorf("transfer pattern expansion: %w", err)
	}
	data := output.Bytes()
	if len(data) == 0 || data[len(data)-1] != 0 {
		return nil, fmt.Errorf("invalid transfer pattern expansion")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	if len(parts) > 100000 {
		return nil, fmt.Errorf("file pattern expands too broadly")
	}
	words := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) != 0 {
			words = append(words, string(part))
		}
	}
	return words, nil
}
