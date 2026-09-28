//go:build windows

package gsocket

import "fmt"

func expandFTCommandWords(string, string) ([]string, error) {
	return nil, fmt.Errorf("POSIX command substitution in transfer patterns requires Unix")
}
