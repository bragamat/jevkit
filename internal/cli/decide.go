package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// An agent hands Jev the binary and categorical calls of its own work and
// follows the verdict. The bands follow docs.typesafe.ai/confidence: high
// confidence acts, medium asks for more evidence, low does not act on the guess.
const (
	VerdictAct      = "ACT"
	VerdictConfirm  = "CONFIRM"
	VerdictRephrase = "REPHRASE"
	VerdictYes      = "YES"
	VerdictNo       = "NO"
	VerdictUnsure   = "UNSURE"

	maxContextChars = 60000
)

// confidenceBands maps risk to (act from, rephrase below) for choice and score.
var confidenceBands = map[string][2]float64{"low": {0.7, 0.5}, "high": {0.9, 0.5}}

// noulBands maps risk to (yes from, no up to) for yes/no questions.
var noulBands = map[string][2]float64{"low": {0.7, 0.3}, "high": {0.85, 0.15}}

type evidence struct {
	Files []string
	Text  string
	Risk  string
}

// state joins the evidence files (or stdin for "-") and the inline text.
func (a *App) state(e evidence) (any, error) {
	if _, ok := confidenceBands[e.Risk]; !ok {
		return nil, inputErrorf("--risk must be low or high")
	}
	var parts []string
	for _, f := range e.Files {
		if f == "-" {
			b, err := io.ReadAll(a.Stdin)
			if err != nil {
				return nil, inputErrorf("cannot read stdin: %v", err)
			}
			parts = append(parts, string(b))
			continue
		}
		b, err := readFile(f)
		if err != nil {
			return nil, err
		}
		parts = append(parts, strings.ToValidUTF8(string(b), "�"))
	}
	if e.Text != "" {
		parts = append(parts, e.Text)
	}
	text := strings.Join(parts, "\n\n")
	if utf8.RuneCountInString(text) > maxContextChars {
		return nil, inputErrorf("context too large: filter it first (jev find) and send only what the decision needs")
	}
	if text == "" {
		text = "(no context beyond the question)"
	}
	return typesafe.NewFields().Set("context", text), nil
}

func confidenceVerdict(conf float64, risk string) string {
	b := confidenceBands[risk]
	switch {
	case conf >= b[0]:
		return VerdictAct
	case conf >= b[1]:
		return VerdictConfirm
	}
	return VerdictRephrase
}

func (a *App) logDecision(kind, question string, answer any, verdict, model string) {
	a.appendLog(a.DecisionLog, map[string]any{
		"ts":       a.timestamp(),
		"cwd":      cwd(),
		"kind":     kind,
		"question": question,
		"answer":   answer,
		"verdict":  verdict,
		"model":    model,
	})
}

// parseChoices reads "name" or "name=description" arguments.
func parseChoices(args []string) *typesafe.Fields {
	out := typesafe.NewFields()
	for _, arg := range args {
		name, desc, _ := strings.Cut(arg, "=")
		var d any
		if desc = strings.TrimSpace(desc); desc != "" {
			d = desc
		}
		out.Set(strings.TrimSpace(name), d)
	}
	return out
}

func (a *App) runDecide(ctx context.Context, question string, choices []string, e evidence) error {
	opts := parseChoices(choices)
	if opts.Len() < 2 {
		return inputErrorf("give at least two distinct options")
	}
	state, err := a.state(e)
	if err != nil {
		return err
	}
	r, err := a.ask(ctx, state, typesafe.NewFields().Set("q", typesafe.Choice(question, opts)))
	if err != nil {
		return err
	}
	ans := r.Answers["q"]
	verdict := confidenceVerdict(ans.Confidence, e.Risk)
	probs := typesafe.NewFields()
	for _, o := range byProbability(ans.Probabilities, opts.Keys()) {
		probs.Set(o.Name, round(o.P, 3))
	}
	a.logDecision("decide", question, typesafe.NewFields().
		Set("choice", ans.Choice).Set("confidence", round(ans.Confidence, 3)).Set("probabilities", probs),
		verdict, r.Model)
	if a.jsonOut {
		return a.emit(typesafe.NewFields().
			Set("choice", ans.Choice).
			Set("confidence", round(ans.Confidence, 3)).
			Set("verdict", verdict).
			Set("probabilities", probs).
			Set("model", r.Model))
	}
	a.printf("%s: %s  (conf %.2f, risk %s, %s)\n", verdict, ans.Choice, ans.Confidence, e.Risk, r.Model)
	var parts []string
	for _, k := range probs.Keys() {
		p, _ := probs.Get(k)
		parts = append(parts, fmt.Sprintf("%s=%.2f", k, p))
	}
	a.printf("  %s\n", strings.Join(parts, "  "))
	return nil
}

func (a *App) runYesNo(ctx context.Context, question, yes, no string, e evidence) error {
	state, err := a.state(e)
	if err != nil {
		return err
	}
	var y, n any
	if yes != "" {
		y = yes
	}
	if no != "" {
		n = no
	}
	r, err := a.ask(ctx, state, typesafe.NewFields().Set("q", typesafe.Noul(question, y, n)))
	if err != nil {
		return err
	}
	p := r.Answers["q"].Noul
	b := noulBands[e.Risk]
	verdict := VerdictUnsure
	switch {
	case p >= b[0]:
		verdict = VerdictYes
	case p <= b[1]:
		verdict = VerdictNo
	}
	a.logDecision("yesno", question, map[string]any{"p_yes": round(p, 3)}, verdict, r.Model)
	if a.jsonOut {
		return a.emit(typesafe.NewFields().Set("p_yes", round(p, 3)).Set("verdict", verdict).Set("model", r.Model))
	}
	a.printf("%s  (p_yes %.2f, risk %s, %s)\n", verdict, p, e.Risk, r.Model)
	return nil
}

func (a *App) runScore(ctx context.Context, question string, levels []string, e evidence) error {
	state, err := a.state(e)
	if err != nil {
		return err
	}
	scale := make([]any, len(levels))
	for i, l := range levels {
		scale[i] = l
	}
	r, err := a.ask(ctx, state, typesafe.NewFields().Set("q", typesafe.Score(question, scale)))
	if err != nil {
		return err
	}
	ans := r.Answers["q"]
	idx := min(len(levels)-1, max(0, int(math.RoundToEven(ans.Score))))
	level := levels[idx]
	verdict := confidenceVerdict(ans.Confidence, e.Risk)
	a.logDecision("score", question, typesafe.NewFields().
		Set("score", round(ans.Score, 3)).Set("level", level).Set("confidence", round(ans.Confidence, 3)),
		verdict, r.Model)
	if a.jsonOut {
		return a.emit(typesafe.NewFields().
			Set("score", round(ans.Score, 3)).
			Set("level", level).
			Set("confidence", round(ans.Confidence, 3)).
			Set("verdict", verdict).
			Set("model", r.Model))
	}
	a.printf("%s: %s  (score %.2f of 0–%d, conf %.2f, %s)\n", verdict, level, ans.Score, len(levels)-1, ans.Confidence, r.Model)
	return nil
}
