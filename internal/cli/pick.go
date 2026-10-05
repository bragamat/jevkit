package cli

import (
	"context"
	"sort"
	"strings"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// readOptions accepts a JSON object (option → description), a JSON array, or
// one option per line.
func readOptions(path string) (*typesafe.Fields, error) {
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	opts := typesafe.NewFields()
	if v, err := typesafe.DecodeOrdered([]byte(text)); err == nil {
		switch t := v.(type) {
		case *typesafe.Fields:
			for _, k := range t.Keys() {
				d, _ := t.Get(k)
				opts.Set(k, description(d))
			}
			return opts, nil
		case []any:
			for _, x := range t {
				opts.Set(display(x), nil)
			}
			return opts, nil
		}
	}
	for _, l := range lineBreak.Split(text, -1) {
		if l = strings.TrimSpace(l); l != "" {
			opts.Set(l, nil)
		}
	}
	return opts, nil
}

// description keeps a non-empty description and drops empty ones.
func description(v any) any {
	if v == nil {
		return nil
	}
	if s := display(v); s != "" {
		return s
	}
	return nil
}

type option struct {
	Name string
	P    float64
}

func byProbability(probs map[string]float64, keys []string) []option {
	out := make([]option, 0, len(keys))
	for _, k := range keys {
		out = append(out, option{k, probs[k]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].P > out[j].P })
	return out
}

// pick chooses one option. More options than one choice holds run in two
// rounds: the best three of each group compete in a final.
func (a *App) pick(ctx context.Context, question string, opts *typesafe.Fields, state any) ([]option, error) {
	round := func(ctx context.Context, keys []string) ([]option, error) {
		criteria := typesafe.NewFields()
		for _, k := range keys {
			d, _ := opts.Get(k)
			criteria.Set(k, d)
		}
		r, err := a.ask(ctx, state, typesafe.NewFields().Set("q", typesafe.Choice(question, criteria)))
		if err != nil {
			return nil, err
		}
		return byProbability(r.Answers["q"].Probabilities, keys), nil
	}
	keys := opts.Keys()
	if len(keys) <= windowSize {
		return round(ctx, keys)
	}
	var groups [][]string
	for i := 0; i < len(keys); i += windowSize {
		groups = append(groups, keys[i:min(i+windowSize, len(keys))])
	}
	partial, err := parallel(ctx, len(groups), concurrency, func(ctx context.Context, g int) ([]option, error) {
		return round(ctx, groups[g])
	})
	if err != nil {
		return nil, err
	}
	var finalists []string
	for _, p := range partial {
		for _, o := range p[:min(3, len(p))] {
			finalists = append(finalists, o.Name)
		}
	}
	return round(ctx, finalists)
}

func (a *App) runPick(ctx context.Context, question, optionsPath, statePath string, top int) error {
	opts, err := readOptions(optionsPath)
	if err != nil {
		return err
	}
	if opts.Len() == 0 {
		return inputErrorf("no options in %s", optionsPath)
	}
	var state any = typesafe.NewFields().Set("task", question)
	if statePath != "" {
		if state, err = readText(statePath); err != nil {
			return err
		}
	}
	ranked, err := a.pick(ctx, question, opts, state)
	if err != nil {
		return err
	}
	ranked = ranked[:min(max(top, 0), len(ranked))]
	if a.jsonOut {
		out := []map[string]any{}
		for _, o := range ranked {
			out = append(out, map[string]any{"option": o.Name, "p": round(o.P, 3)})
		}
		return a.emit(out)
	}
	for _, o := range ranked {
		a.printf("  %.2f  %s\n", o.P, o.Name)
	}
	return nil
}
