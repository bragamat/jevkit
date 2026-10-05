package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/bragamat/jevkit/internal/typesafe"
)

type usageRow struct {
	Command     string `json:"command"`
	Calls       int    `json:"calls"`
	InputTokens int    `json:"input_tokens"`
}

// runUsage sums the local usage log by subcommand.
func (a *App) runUsage(today bool) error {
	byCmd := map[string]*usageRow{}
	f, err := os.Open(a.UsageLog)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return inputErrorf("cannot read %s: %v", a.UsageLog, err)
	}
	if f != nil {
		defer f.Close()
		day := a.Now().Format("2006-01-02")
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var rec struct {
				TS          string `json:"ts"`
				Cmd         string `json:"cmd"`
				InputTokens int    `json:"input_tokens"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil || (today && !strings.HasPrefix(rec.TS, day)) {
				continue
			}
			row := byCmd[rec.Cmd]
			if row == nil {
				row = &usageRow{Command: rec.Cmd}
				byCmd[rec.Cmd] = row
			}
			row.Calls++
			row.InputTokens += rec.InputTokens
		}
		if err := sc.Err(); err != nil {
			return inputErrorf("cannot read %s: %v", a.UsageLog, err)
		}
	}
	rows := make([]usageRow, 0, len(byCmd))
	total := usageRow{Command: "total"}
	for _, r := range byCmd {
		rows = append(rows, *r)
		total.Calls += r.Calls
		total.InputTokens += r.InputTokens
	}
	slices.SortFunc(rows, func(x, y usageRow) int {
		return cmp.Or(cmp.Compare(y.InputTokens, x.InputTokens), cmp.Compare(x.Command, y.Command))
	})
	usd := float64(total.InputTokens) * usdPerMillionInputTokens / 1e6
	if a.jsonOut {
		return a.emit(typesafe.NewFields().
			Set("commands", rows).
			Set("calls", total.Calls).
			Set("input_tokens", total.InputTokens).
			Set("usd_estimate", round(usd, 4)))
	}
	for _, r := range rows {
		a.printf("  %-8s %5d %-5s  %10s tokens read by Jev\n", r.Command, r.Calls, plural(r.Calls, "call"), thousands(r.InputTokens))
	}
	a.printf("  %-8s %5d %-5s  %10s tokens  ≈ US$ %.4f\n", "total", total.Calls, plural(total.Calls, "call"), thousands(total.InputTokens), usd)
	return nil
}

// runModels lists the models the API serves.
func (a *App) runModels(ctx context.Context) error {
	client, err := a.getClient()
	if err != nil {
		return err
	}
	models, err := client.Models(ctx)
	if err != nil {
		return err
	}
	if a.jsonOut {
		return a.emit(models)
	}
	for _, m := range models {
		a.printf("  %-14s %s  %s\n", m.Name, truncate(m.ReleaseDate, 10), m.Description)
	}
	return nil
}

// plural adds an "s" to word unless n is one.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
