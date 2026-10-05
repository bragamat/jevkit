package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bragamat/jevkit/internal/typesafe"
)

type rankedLine struct {
	numberedLine
	Score float64
}

// find ranks the lines of a document by how well each answers question, and
// says whether the document answers it at all. Documents longer than one
// choice are split into windows that run in parallel.
func (a *App) find(ctx context.Context, lines []numberedLine, question string) (float64, []rankedLine, error) {
	windows := splitWindows(lines)
	type result struct {
		exists float64
		lines  []rankedLine
	}
	results, err := parallel(ctx, len(windows), concurrency, func(ctx context.Context, w int) (result, error) {
		var state strings.Builder
		criteria := typesafe.NewFields()
		for i, l := range windows[w] {
			if i > 0 {
				state.WriteByte('\n')
			}
			state.WriteString(windowLine(l))
			criteria.Set(fmt.Sprintf("L%d", l.N), nil)
		}
		where := fmt.Sprintf("Which line of the document contains the answer to: %q?", question)
		// Asked in both orders and averaged: jev-1.13 leans toward the first option.
		questions := typesafe.NewFields().
			Set("where", typesafe.Choice(where, criteria)).
			Set("where:reversed", typesafe.Choice(where, reverseFields(criteria))).
			Set("exists", typesafe.Noul(fmt.Sprintf("Does any line of the document address or answer: %q?", question),
				"At least one line states or directly implies the answer",
				"No line of the document addresses this"))
		r, err := a.ask(ctx, state.String(), questions)
		if err != nil {
			return result{}, err
		}
		probs := averaged(r.Answers["where"].Probabilities, r.Answers["where:reversed"].Probabilities)
		out := result{exists: r.Answers["exists"].Noul}
		for _, l := range windows[w] {
			out.lines = append(out.lines, rankedLine{l, probs[fmt.Sprintf("L%d", l.N)]})
		}
		return out, nil
	})
	if err != nil {
		return 0, nil, err
	}
	exists := 0.0
	var all []rankedLine
	for _, r := range results {
		exists = max(exists, r.exists)
		// A window that does not answer must not push its lines up: weight by its own exists.
		for _, l := range r.lines {
			l.Score *= r.exists
			all = append(all, l)
		}
	}
	slices.SortStableFunc(all, func(x, y rankedLine) int { return cmp.Compare(y.Score, x.Score) })
	return exists, all, nil
}

// windowLine is how one line appears in a window's state.
func windowLine(l numberedLine) string {
	return fmt.Sprintf("L%d: %s", l.N, truncate(l.Text, 400))
}

// splitWindows cuts lines into windows of at most windowSize lines and
// windowChars characters, so each window's state stays under the API limit.
func splitWindows(lines []numberedLine) [][]numberedLine {
	var windows [][]numberedLine
	start, size := 0, 0
	for i, l := range lines {
		n := utf8.RuneCountInString(windowLine(l)) + 1
		if i > start && (i-start >= windowSize || size+n > windowChars) {
			windows = append(windows, lines[start:i])
			start, size = i, 0
		}
		size += n
	}
	if start < len(lines) {
		windows = append(windows, lines[start:])
	}
	return windows
}

func (a *App) runFind(ctx context.Context, path, question string, top int) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	exists, ranked, err := a.find(ctx, lines, question)
	if err != nil {
		return err
	}
	ranked = ranked[:min(max(top, 0), len(ranked))]
	if a.jsonOut {
		out := []map[string]any{}
		for _, l := range ranked {
			out = append(out, map[string]any{"line": l.N, "text": l.Text, "score": round(l.Score, 3)})
		}
		return a.emit(typesafe.NewFields().Set("exists", round(exists, 3)).Set("lines", out))
	}
	verdict := "does NOT answer"
	switch {
	case exists >= 0.6:
		verdict = "answers"
	case exists >= 0.3:
		verdict = "partial"
	}
	a.printf("exists=%.2f (%s)  %s\n", exists, verdict, path)
	for _, l := range ranked {
		a.printf("  %.2f  L%d: %s\n", l.Score, l.N, truncate(l.Text, 160))
	}
	return nil
}

// runCheck finds the passage closest to claim, then asks how it relates.
func (a *App) runCheck(ctx context.Context, claim, path string) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	exists, ranked, err := a.find(ctx, lines, claim)
	if err != nil {
		return err
	}
	top := ranked[:min(6, len(ranked))]
	section := slices.Clone(top)
	slices.SortStableFunc(section, func(x, y rankedLine) int { return cmp.Compare(x.N, y.N) })
	var text strings.Builder
	for i, l := range section {
		if i > 0 {
			text.WriteByte('\n')
		}
		fmt.Fprintf(&text, "L%d: %s", l.N, l.Text)
	}
	criteria := typesafe.NewFields().
		Set("supports", "The section states or directly implies everything the claim says").
		Set("partially", "The section supports part of the claim; some detail is missing or different").
		Set("contradicts", "The section states something incompatible with the claim").
		Set("not_addressed", "The section does not speak to the claim")
	r, err := a.ask(ctx,
		typesafe.NewFields().Set("claim", claim).Set("section", text.String()),
		typesafe.NewFields().Set("relation", typesafe.Choice("How does `section` relate to `claim`?", criteria)))
	if err != nil {
		return err
	}
	rel := r.Answers["relation"]
	evidence := top[:min(3, len(top))]
	if a.jsonOut {
		ev := []map[string]any{}
		for _, l := range evidence {
			ev = append(ev, map[string]any{"line": l.N, "text": l.Text})
		}
		return a.emit(typesafe.NewFields().
			Set("relation", rel.Choice).
			Set("confidence", round(rel.Confidence, 2)).
			Set("exists", round(exists, 2)).
			Set("evidence", ev))
	}
	a.printf("%s (conf %.2f, exists %.2f)\n", rel.Choice, rel.Confidence, exists)
	for _, l := range evidence {
		a.printf("  L%d: %s\n", l.N, truncate(l.Text, 160))
	}
	return nil
}
