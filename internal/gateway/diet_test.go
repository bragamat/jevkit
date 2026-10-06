package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bragamat/jevkit/internal/typesafe"
)

const dietInstructions = "<system-reminder>\nContents of /p/CLAUDE.md (project instructions, checked into the codebase):\n\nA billing service.\n\n" +
	"Contents of /h/memory/MEMORY.md (user's auto-memory, persists across conversations):\n\n" +
	"- [Invoice rounding](invoice-rounding.md) — totals round half-even since the March bug\n" +
	"- [Vlog editor](vlog-editor.md) — the editor app may call Jev\n" +
	"</system-reminder>\n"

const dietSystem = "# Environment\n - Primary working directory: /p\n\n" + skillsHeader + "\n\n" +
	"- billing-sql: Query the billing database safely.\n" +
	"- vlog-cut: Cut a vlog from raw clips.\n" +
	"- media:vlog-grade: Grade vlog colors.\n" +
	"  More about cutting.\n" +
	"- claude-api: Build with the API.\nTRIGGER — read before opening an SDK file.\n" +
	"- bare-name\n\nToday's date is 2026-10-06."

// dietBody builds a Claude Code request; turns adds that many assistant/user pairs.
func dietBody(t *testing.T, session string, turns int) []byte {
	t.Helper()
	msgs := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": dietInstructions},
			map[string]any{"type": "text", "text": "fix the invoice total"},
		}},
		map[string]any{"role": "system", "content": dietSystem},
	}
	for i := range turns {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": fmt.Sprint("step ", i)},
			map[string]any{"role": "user", "content": "go on"})
	}
	b, err := json.Marshal(map[string]any{"model": "claude-x", "metadata": map[string]any{"user_id": session}, "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// scoreByWord scores an item 2 when its text mentions a word, else 0.
func scoreByWord(word string) func(typesafe.Request) (*typesafe.Response, error) {
	return func(req typesafe.Request) (*typesafe.Response, error) {
		state := req.State.(*typesafe.Fields)
		items, _ := state.Get("items")
		answers := map[string]typesafe.Answer{}
		for i, it := range items.([]any) {
			b, _ := json.Marshal(it)
			s := 0.0
			if strings.Contains(strings.ToLower(string(b)), word) {
				s = 2
			}
			answers[fmt.Sprintf("item_%d", i)] = typesafe.Answer{Type: "score", Score: s}
		}
		return &typesafe.Response{Usage: typesafe.Usage{InputTokens: 100}, Answers: answers}, nil
	}
}

func testDieter(t *testing.T, mode string, fn func(typesafe.Request) (*typesafe.Response, error)) (*dieter, *fakeJev) {
	t.Helper()
	jev := &fakeJev{fn: fn}
	cfg := DietConfig{Mode: mode, SkillFloor: 0.5, MemoryFloor: 0.5, Batch: 20, Budget: time.Second,
		Log: filepath.Join(t.TempDir(), "diet.jsonl")}
	return newDieter(cfg, jev, "jev-test"), jev
}

func dietTexts(t *testing.T, body []byte) string {
	t.Helper()
	root, err := typesafe.DecodeOrdered(body)
	if err != nil {
		t.Fatal(err)
	}
	slots, _ := openingTexts(root.(*typesafe.Fields))
	var all []string
	for _, s := range slots {
		all = append(all, s.text)
	}
	return strings.Join(all, "\n")
}

func TestDietTrimsUnrelatedEntriesAndReplaysThem(t *testing.T) {
	d, jev := testDieter(t, DietOn, scoreByWord("invoice"))
	rec := &dietRecord{}
	out := d.apply(context.Background(), dietBody(t, "s1", 0), rec)
	if out == nil || !rec.Decided || rec.Skills != 5 || rec.Memory != 2 {
		t.Fatalf("first request: %+v", rec)
	}
	got := dietTexts(t, out)
	for _, want := range []string{
		"- [Invoice rounding](invoice-rounding.md) — totals round half-even",
		"- [Vlog editor](vlog-editor.md)\n",
		"- billing-sql\n", "- vlog-cut\n- media:vlog-grade\n- claude-api: Build with the API.\nTRIGGER", "- bare-name\n\nToday's date",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "More about cutting") || rec.SkillsTrimmed != 3 || rec.MemoryTrimmed != 1 || rec.SavedChars == 0 {
		t.Fatalf("trimmed wrong: %+v\n%s", rec, got)
	}
	// Jev saw the project, the working directory and the first request.
	ctxText, _ := jev.calls[0].State.(*typesafe.Fields).Get("context")
	for _, want := range []string{"Working directory: /p", "A billing service.", "First user request: fix the invoice total"} {
		if !strings.Contains(ctxText.(string), want) {
			t.Fatalf("context lacks %q: %s", want, ctxText)
		}
	}

	// A later turn trims the same way without asking again, also after a restart.
	calls := len(jev.calls)
	later := dietBody(t, "s1", 2)
	rec2 := &dietRecord{}
	out2 := d.apply(context.Background(), later, rec2)
	d2 := newDieter(d.cfg, &fakeJev{fn: scoreByWord("vlog")}, "jev-test")
	out3 := d2.apply(context.Background(), later, &dietRecord{})
	if rec2.Decided || len(jev.calls) != calls || dietTexts(t, out2) != got || dietTexts(t, out3) != got {
		t.Fatalf("replay differs: %+v", rec2)
	}
}

func TestDietLeavesStartedConversationsAlone(t *testing.T) {
	d, jev := testDieter(t, DietOn, scoreByWord("invoice"))
	if out := d.apply(context.Background(), dietBody(t, "s", 3), &dietRecord{}); out != nil || len(jev.calls) != 0 {
		t.Fatal("trimmed a conversation the gateway first saw mid-way")
	}
}

func TestDietJevErrorKeepsTheConversationWhole(t *testing.T) {
	d, _ := testDieter(t, DietOn, func(typesafe.Request) (*typesafe.Response, error) { return nil, errors.New("boom") })
	rec := &dietRecord{}
	if out := d.apply(context.Background(), dietBody(t, "s", 0), rec); out != nil || rec.Error == "" {
		t.Fatalf("got a rewrite on a Jev error: %+v", rec)
	}
	d.jev = &fakeJev{fn: scoreByWord("invoice")}
	if out := d.apply(context.Background(), dietBody(t, "s", 1), &dietRecord{}); out != nil {
		t.Fatal("trimmed after the first request had gone out whole")
	}
}

func TestDietABControlOnlyCounts(t *testing.T) {
	d, jev := testDieter(t, DietAB, scoreByWord("invoice"))
	arms := map[string]int{}
	for i := range 20 {
		rec := &dietRecord{}
		out := d.apply(context.Background(), dietBody(t, fmt.Sprint("session-", i), 0), rec)
		arms[rec.Arm]++
		if rec.Arm == armControl && (out != nil || rec.Skills != 5) {
			t.Fatalf("control arm: %+v", rec)
		}
	}
	if arms[armDiet] == 0 || arms[armControl] == 0 || len(jev.calls) != 2*arms[armDiet] {
		t.Fatalf("arms %v, calls %d", arms, len(jev.calls))
	}
}

func TestDietIgnoresRequestsWithoutListings(t *testing.T) {
	d, jev := testDieter(t, DietOn, scoreByWord("x"))
	b, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if rec := (&dietRecord{}); d.apply(context.Background(), b, rec) != nil || rec.Arm != "" || len(jev.calls) != 0 {
		t.Fatal("touched a request with no skill listing or memory index")
	}
}

func TestDietAllTrimsWithoutJev(t *testing.T) {
	d, jev := testDieter(t, DietAll, scoreByWord("invoice"))
	rec := &dietRecord{}
	if d.apply(context.Background(), dietBody(t, "s", 0), rec) == nil || len(jev.calls) != 0 || rec.SkillsTrimmed != 3 || rec.MemoryTrimmed != 2 {
		t.Fatalf("trim-all: %+v, calls %d", rec, len(jev.calls))
	}
}

func TestDietKeepsTheBestScored(t *testing.T) {
	got := trimmed([]float64{0, 0.4, 2, 0.1}, 0.5, 2)
	if want := []bool{true, false, false, true}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
