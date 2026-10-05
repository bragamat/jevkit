package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// specQuestion is one entry of a triage spec: {"type", "instructions", "criteria"}.
type specQuestion struct {
	ID           string
	Type         string
	Instructions any
	Criteria     any
}

func readSpec(path string) ([]specQuestion, error) {
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(text, path)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(*typesafe.Fields)
	if !ok || obj.Len() == 0 {
		return nil, inputErrorf("%s must be a JSON object of questions: {\"id\": {\"type\": ..., \"instructions\": ..., \"criteria\": ...}}", path)
	}
	var spec []specQuestion
	for _, id := range obj.Keys() {
		raw, _ := obj.Get(id)
		q, ok := raw.(*typesafe.Fields)
		if !ok {
			return nil, inputErrorf("question %q must be an object", id)
		}
		t, _ := q.Get("type")
		typ, _ := t.(string)
		typ = strings.ToLower(typ)
		if typ != "choice" && typ != "noul" && typ != "score" {
			return nil, inputErrorf("question %q: type must be choice, noul or score (got %s)", id, display(t))
		}
		instr, _ := q.Get("instructions")
		crit, _ := q.Get("criteria")
		spec = append(spec, specQuestion{ID: id, Type: typ, Instructions: instr, Criteria: crit})
	}
	return spec, nil
}

// question turns a spec entry into an API question.
func (q specQuestion) question() (typesafe.Question, error) {
	switch q.Type {
	case "choice":
		crit := typesafe.NewFields()
		switch c := q.Criteria.(type) {
		case *typesafe.Fields:
			crit = c
		case []any:
			for _, x := range c {
				crit.Set(display(x), nil)
			}
		default:
			return typesafe.Question{}, inputErrorf("question %q: choice criteria must be an object or a list", q.ID)
		}
		if n := crit.Len(); n < 2 || n > typesafe.MaxChoiceOptions {
			return typesafe.Question{}, inputErrorf("question %q: a choice takes 2 to %d options (got %d)", q.ID, typesafe.MaxChoiceOptions, n)
		}
		return typesafe.Choice(q.Instructions, crit), nil
	case "noul":
		if q.Criteria == nil {
			return typesafe.Noul(q.Instructions, nil, nil), nil
		}
		return typesafe.Question{Type: "noul", Instructions: q.Instructions, Criteria: q.Criteria}, nil
	default:
		levels, ok := q.Criteria.([]any)
		if !ok || len(levels) < typesafe.MinScoreLevels || len(levels) > typesafe.MaxScoreLevels {
			return typesafe.Question{}, inputErrorf("question %q: score criteria must be a list of %d to %d levels", q.ID, typesafe.MinScoreLevels, typesafe.MaxScoreLevels)
		}
		return typesafe.Score(q.Instructions, levels), nil
	}
}

// record copies one answer into a result row.
func (q specQuestion) record(a typesafe.Answer, row *typesafe.Fields) {
	switch q.Type {
	case "choice":
		row.Set(q.ID, a.Choice).Set(q.ID+"_conf", round(a.Confidence, 2))
	case "noul":
		row.Set(q.ID, round(a.Noul, 2))
	default:
		row.Set(q.ID, round(a.Score, 2)).Set(q.ID+"_conf", round(a.Confidence, 2))
	}
}

// forItem points a question at items[i] of a batched state.
func (q specQuestion) forItem(i int) specQuestion {
	ref := fmt.Sprintf("items[%d]", i)
	swap := func(s string) string {
		s = strings.ReplaceAll(s, "`item.", "`"+ref+".")
		return strings.ReplaceAll(s, "`item`", "`"+ref+"`")
	}
	out := q
	out.ID = fmt.Sprintf("%s::%d", q.ID, i)
	out.Instructions = rewrite(q.Instructions, swap)
	out.Criteria = rewrite(q.Criteria, swap)
	if !strings.Contains(display(out.Instructions), ref) {
		out.Instructions = fmt.Sprintf("About `%s`: %s", ref, display(out.Instructions))
	}
	return out
}

// rewrite applies f to every string inside a decoded JSON value.
func rewrite(v any, f func(string) string) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case *typesafe.Fields:
		out := typesafe.NewFields()
		for _, k := range t.Keys() {
			x, _ := t.Get(k)
			out.Set(k, rewrite(x, f))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = rewrite(x, f)
		}
		return out
	}
	return v
}

// readItems accepts a JSON array or JSONL.
func readItems(path string) ([]any, error) {
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "[") {
		v, err := decodeJSON(text, path)
		if err != nil {
			return nil, err
		}
		list, ok := v.([]any)
		if !ok {
			return nil, inputErrorf("%s: expected a JSON array or JSONL", path)
		}
		return list, nil
	}
	var items []any
	for n, l := range lineBreak.Split(text, -1) {
		if strings.TrimSpace(l) == "" {
			continue
		}
		v, err := decodeJSON(l, fmt.Sprintf("%s line %d", path, n+1))
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, nil
}

type triageOptions struct {
	Label   string
	SortBy  string
	Context string
	Batch   int
}

func (a *App) runTriage(ctx context.Context, itemsPath, specPath string, o triageOptions) error {
	items, err := readItems(itemsPath)
	if err != nil {
		return err
	}
	spec, err := readSpec(specPath)
	if err != nil {
		return err
	}
	var shared any
	if o.Context != "" {
		text, err := readText(o.Context)
		if err != nil {
			return err
		}
		if shared, err = typesafe.DecodeOrdered([]byte(text)); err != nil {
			shared = text
		}
	}
	var rows []*typesafe.Fields
	if shared == nil {
		rows, err = a.triageEach(ctx, items, spec)
	} else {
		if o.Batch < 1 {
			return inputErrorf("--batch must be at least 1")
		}
		rows, err = a.triageBatched(ctx, items, spec, shared, o.Batch)
	}
	if err != nil {
		return err
	}
	for i, row := range rows {
		label := truncate(display(items[i]), 80)
		if obj, ok := items[i].(*typesafe.Fields); ok && o.Label != "" {
			v, _ := obj.Get(o.Label)
			label = display(v)
		}
		labelled := typesafe.NewFields().Set("_", label)
		for _, k := range row.Keys() {
			v, _ := row.Get(k)
			labelled.Set(k, v)
		}
		rows[i] = labelled
	}
	if o.SortBy != "" {
		key := func(r *typesafe.Fields) float64 {
			v, _ := r.Get(o.SortBy)
			f, _ := v.(float64)
			return f
		}
		slices.SortStableFunc(rows, func(x, y *typesafe.Fields) int { return cmp.Compare(key(y), key(x)) })
	}
	for _, row := range rows {
		if a.jsonOut {
			if err := a.emit(row); err != nil {
				return err
			}
			continue
		}
		var parts []string
		for _, k := range row.Keys() {
			v, _ := row.Get(k)
			if k == "_" {
				parts = append(parts, display(v))
			} else {
				parts = append(parts, k+"="+display(v))
			}
		}
		a.printf("%s\n", strings.Join(parts, " | "))
	}
	return nil
}

func questionsFor(spec []specQuestion) (*typesafe.Fields, error) {
	qs := typesafe.NewFields()
	for _, q := range spec {
		apiQ, err := q.question()
		if err != nil {
			return nil, err
		}
		qs.Set(q.ID, apiQ)
	}
	return qs, nil
}

// triageEach sends one request per item; the item is the whole state.
func (a *App) triageEach(ctx context.Context, items []any, spec []specQuestion) ([]*typesafe.Fields, error) {
	qs, err := questionsFor(spec)
	if err != nil {
		return nil, err
	}
	return parallel(ctx, len(items), concurrency, func(ctx context.Context, i int) (*typesafe.Fields, error) {
		r, err := a.ask(ctx, typesafe.NewFields().Set("item", items[i]), qs)
		if err != nil {
			return nil, err
		}
		row := typesafe.NewFields()
		for _, q := range spec {
			q.record(r.Answers[q.ID], row)
		}
		return row, nil
	})
}

// triageBatched sends the shared context once per batch and asks every
// question once per item ("count in code, one question per item").
func (a *App) triageBatched(ctx context.Context, items []any, spec []specQuestion, shared any, batch int) ([]*typesafe.Fields, error) {
	var groups [][]any
	for i := 0; i < len(items); i += batch {
		groups = append(groups, items[i:min(i+batch, len(items))])
	}
	results, err := parallel(ctx, len(groups), concurrency, func(ctx context.Context, g int) ([]*typesafe.Fields, error) {
		group := groups[g]
		var perItem []specQuestion
		for i := range group {
			for _, q := range spec {
				perItem = append(perItem, q.forItem(i))
			}
		}
		qs, err := questionsFor(perItem)
		if err != nil {
			return nil, err
		}
		r, err := a.ask(ctx, typesafe.NewFields().Set("context", shared).Set("items", group), qs)
		if err != nil {
			return nil, err
		}
		rows := make([]*typesafe.Fields, len(group))
		for i := range group {
			rows[i] = typesafe.NewFields()
			for _, q := range spec {
				q.record(r.Answers[fmt.Sprintf("%s::%d", q.ID, i)], rows[i])
			}
		}
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	var rows []*typesafe.Fields
	for _, g := range results {
		rows = append(rows, g...)
	}
	return rows, nil
}
