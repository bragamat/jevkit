package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// inputError is a problem with what the caller passed. It prints as one line,
// never as a stack trace, so it costs an agent almost no context.
type inputError struct{ msg string }

func (e inputError) Error() string { return e.msg }

func inputErrorf(format string, args ...any) error {
	return inputError{fmt.Sprintf(format, args...)}
}

// readFile reads path and turns the usual failures into short messages.
func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, inputErrorf("file not found: %s", path)
	case errors.Is(err, fs.ErrPermission):
		return nil, inputErrorf("permission denied: %s", path)
	}
	if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
		return nil, inputErrorf("is a directory, not a file: %s", path)
	}
	return nil, inputErrorf("cannot read %s: %v", path, err)
}

// readText reads a file that must be UTF-8 text.
func readText(path string) (string, error) {
	data, err := readFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", inputErrorf("not UTF-8 text: %s", path)
	}
	return string(data), nil
}

// decodeJSON parses JSON keeping object key order, with a line and column on failure.
func decodeJSON(data string, source string) (any, error) {
	v, err := typesafe.DecodeOrdered([]byte(data))
	if err == nil {
		return v, nil
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		line, col := position(data, syntaxErr.Offset)
		return nil, inputErrorf("invalid JSON in %s (line %d, column %d)", source, line, col)
	}
	return nil, inputErrorf("invalid JSON in %s: %v", source, err)
}

func position(data string, offset int64) (line, col int) {
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	before := data[:offset]
	line = strings.Count(before, "\n") + 1
	col = utf8.RuneCountInString(before[strings.LastIndex(before, "\n")+1:])
	if col == 0 {
		col = 1
	}
	return line, col
}

var lineBreak = regexp.MustCompile(`\r\n|\r|\n`)

// numberedLine is a non-blank line of a document and its 1-based number.
type numberedLine struct {
	N    int
	Text string
}

// readLines returns the non-blank lines of a file, trimmed, with their
// original numbers. Invalid UTF-8 is tolerated: the JSON encoder replaces it.
func readLines(path string) ([]numberedLine, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var out []numberedLine
	for i, l := range lineBreak.Split(string(data), -1) {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, numberedLine{N: i + 1, Text: t})
		}
	}
	return out, nil
}

// truncate cuts s to n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// display renders a decoded JSON value as plain text.
func display(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case float64:
		return formatFloat(t)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// formatFloat prints the shortest form but always with a decimal point (1.0, not 1).
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// thousands formats n with comma separators.
func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
