package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// resolvePrompt returns the prompt text for a run: args joined by spaces
// when given, else one line read from stdin. ok is false when neither yields
// non-empty text, in which case the caller has no prompt to drive.
func resolvePrompt(args []string, stdin io.Reader) (text string, ok bool, err error) {
	if len(args) > 0 {
		return strings.Join(args, " "), true, nil
	}

	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", false, fmt.Errorf("read prompt from stdin: %w", err)
		}
		return "", false, nil
	}
	line := strings.TrimSpace(scanner.Text())
	return line, line != "", nil
}
