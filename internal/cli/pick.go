package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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
	slices.SortStableFunc(out, func(x, y option) int { return cmp.Compare(y.P, x.P) })
	return out
}

// reverseFields returns f with its keys in the opposite order.
func reverseFields(f *typesafe.Fields) *typesafe.Fields {
	keys := slices.Clone(f.Keys())
	slices.Reverse(keys)
	out := typesafe.NewFields()
	for _, k := range keys {
		v, _ := f.Get(k)
		out.Set(k, v)
	}
	return out
}

// averaged is the mean of two probability maps over the keys of either.
func averaged(x, y map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, p := range x {
		out[k] += p / 2
	}
	for k, p := range y {
		out[k] += p / 2
	}
	return out
}

// pick chooses one option. More options than one choice holds run in two
// rounds: the best three of each group compete in a final.
func (a *App) pick(ctx context.Context, question string, opts *typesafe.Fields, state any) ([]option, error) {
	keys := opts.Keys()
	var groups [][]string
	for i := 0; i < len(keys); i += windowSize {
		groups = append(groups, keys[i:min(i+windowSize, len(keys))])
	}
	partial, err := a.rankGroups(ctx, question, opts, state, groups)
	if err != nil || len(groups) == 1 {
		return slices.Concat(partial...), err
	}
	var finalists []string
	for _, p := range partial {
		for _, o := range p[:min(3, len(p))] {
			finalists = append(finalists, o.Name)
		}
	}
	final, err := a.rankGroups(ctx, question, opts, state, [][]string{finalists})
	if err != nil {
		return nil, err
	}
	return final[0], nil
}

// rankGroups ranks the options of each group with one choice, asked in both
// orders and averaged because jev-1.13 leans toward the first option. Groups
// share requests, as many per request as the API's token limit allows.
func (a *App) rankGroups(ctx context.Context, question string, opts *typesafe.Fields, state any, groups [][]string) ([][]option, error) {
	room := typesafe.MaxRequestTokens - typesafe.ApproxTokens(state)
	var batches [][]int
	used := 0
	choices := make([]typesafe.Question, len(groups))
	for g, keys := range groups {
		criteria := typesafe.NewFields()
		for _, k := range keys {
			d, _ := opts.Get(k)
			criteria.Set(k, d)
		}
		choices[g] = typesafe.Choice(question, criteria)
		cost := 2*typesafe.ApproxTokens(choices[g]) + 32
		if len(batches) == 0 || used+cost > room {
			batches = append(batches, nil)
			used = 0
		}
		batches[len(batches)-1] = append(batches[len(batches)-1], g)
		used += cost
	}
	out := make([][]option, len(groups))
	_, err := parallel(ctx, len(batches), concurrency, func(ctx context.Context, b int) (struct{}, error) {
		questions := typesafe.NewFields()
		for _, g := range batches[b] {
			q := choices[g]
			questions.Set(fmt.Sprintf("q%d", g), q)
			questions.Set(fmt.Sprintf("q%d:reversed", g), typesafe.Choice(q.Instructions, reverseFields(q.Criteria.(*typesafe.Fields))))
		}
		r, err := a.ask(ctx, state, questions)
		if err != nil {
			return struct{}{}, err
		}
		for _, g := range batches[b] {
			probs := averaged(r.Answers[fmt.Sprintf("q%d", g)].Probabilities, r.Answers[fmt.Sprintf("q%d:reversed", g)].Probabilities)
			out[g] = byProbability(probs, groups[g])
		}
		return struct{}{}, nil
	})
	return out, err
}

func (a *App) runPick(ctx context.Context, question, optionsPath, statePath string, top int) error {
	opts, err := readOptions(optionsPath)
	if err != nil {
		return err
	}
	if opts.Len() == 0 {
		return inputErrorf("no options in %s", optionsPath)
	}
	// Each group sends 3 finalists, and the final is one choice.
	if opts.Len() > typesafe.MaxChoiceOptions/3*windowSize {
		return inputErrorf("%d options is more than two rounds can pick from", opts.Len())
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
