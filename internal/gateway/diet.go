package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// Diet trims the context Claude Code repeats on every request: the skill
// listing and the memory index. Jev scores each entry against the project and
// the first request once per conversation; entries it calls unrelated keep only
// their name (a skill) or their title and file (a memory note), so the agent can
// still reach them. The decision is replayed unchanged on every later request,
// so the prompt cache sees the same prefix all session long.

// Diet modes.
const (
	DietOff = "off"
	DietOn  = "on"
	DietAB  = "ab" // half the sessions, by a hash of the session id
	// DietAll trims every entry without asking Jev; a baseline for measuring Jev's choice.
	DietAll = "all"
)

const (
	armDiet    = "diet"
	armControl = "control"

	skillsHeader = "The following skills are available for use with the Skill tool:"
	// dietTTL is how long a conversation's decision is kept for replay.
	dietTTL = 7 * 24 * time.Hour

	dietContextChars     = 6000
	dietRequestChars     = 2000
	dietSkillChars       = 400
	dietMemoryChars      = 300
	dietConcurrency      = 8
	dietSkillQuestion    = "A coding agent starts the session described in `context`. How likely is it to use `%s`, a skill it can invoke, during this session?"
	dietMemoryQuestion   = "A coding agent starts the session described in `context`. `%s` is one line of its memory index (a note it may open). How likely is that note to matter in this session?"
	dietProjectSectionRe = `(?m)^Contents of [^\n]*\(project instructions[^\n]*\n`
)

var (
	dietSkillLevels  = []any{"unrelated: another domain, project or client", "possible: generic engineering help", "likely: matches this project or request"}
	dietMemoryLevels = []any{"unrelated to this request", "possibly related", "directly related to this request"}

	memoryHeader   = regexp.MustCompile(`(?m)^Contents of [^\n]*MEMORY\.md \(user's auto-memory[^\n]*\n`)
	memoryLine     = regexp.MustCompile(`^- \[[^\]]+\]\([^)]+\)`)
	projectSection = regexp.MustCompile(dietProjectSectionRe)
	workingDir     = regexp.MustCompile(`Primary working directory: (\S+)`)
)

// DietConfig controls the context diet.
type DietConfig struct {
	Mode string
	// SkillFloor and MemoryFloor are the relevance scores (0 unrelated to 2
	// likely) below which an entry is trimmed.
	SkillFloor  float64
	MemoryFloor float64
	// KeepSkills and KeepMemory always keep that many of the best-scored
	// entries whole, whatever their score.
	KeepSkills int
	KeepMemory int
	Batch      int
	Budget     time.Duration
	Log        string
}

// dietRecord is what the gateway log keeps about one request's diet.
type dietRecord struct {
	Arm           string `json:"arm"`
	Decided       bool   `json:"decided,omitempty"`
	Skills        int    `json:"skills,omitempty"`
	SkillsTrimmed int    `json:"skillsTrimmed,omitempty"`
	Memory        int    `json:"memory,omitempty"`
	MemoryTrimmed int    `json:"memoryTrimmed,omitempty"`
	SavedChars    int    `json:"savedChars,omitempty"`
	JevTokens     int    `json:"jevTokens,omitempty"`
	LatencyMs     int64  `json:"latencyMs,omitempty"`
	Error         string `json:"error,omitempty"`
	Rejected      bool   `json:"rejected,omitempty"`
}

// dietDecision is one conversation's verdict, persisted so a restarted
// gateway trims the same way.
type dietDecision struct {
	Key    string    `json:"key"`
	Time   time.Time `json:"time"`
	Skills []string  `json:"skills,omitempty"`
	Memory []string  `json:"memory,omitempty"`
}

type dieter struct {
	cfg   DietConfig
	jev   jevClient
	model string

	mu        sync.Mutex
	decisions map[string]dietDecision
}

func newDieter(cfg DietConfig, jev jevClient, model string) *dieter {
	d := &dieter{cfg: cfg, jev: jev, model: model, decisions: map[string]dietDecision{}}
	if cfg.Batch <= 0 {
		d.cfg.Batch = 20
	}
	f, err := os.Open(cfg.Log)
	if err != nil {
		return d
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var dec dietDecision
		if json.Unmarshal(sc.Bytes(), &dec) == nil && time.Since(dec.Time) < dietTTL {
			d.decisions[dec.Key] = dec
		}
	}
	return d
}

func (d *dieter) remember(dec dietDecision) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.decisions[dec.Key] = dec
	if d.cfg.Log == "" {
		return
	}
	b, err := json.Marshal(dec)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(d.cfg.Log), 0o700)
	f, err := os.OpenFile(d.cfg.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func (d *dieter) lookup(key string) (dietDecision, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dec, ok := d.decisions[key]
	return dec, ok
}

// textSlot is one text of the opening messages, with a way to replace it.
type textSlot struct {
	role, text string
	set        func(string)
}

// skillEntry is one "- name: description" line of the skill listing, with any
// continuation lines.
type skillEntry struct {
	name, text string
}

type memoryEntry struct {
	file, text, short string
}

// apply returns the trimmed plain body, or nil to send the request as is.
func (d *dieter) apply(ctx context.Context, plain []byte, rec *dietRecord) []byte {
	root, err := typesafe.DecodeOrdered(plain)
	fields, ok := root.(*typesafe.Fields)
	if err != nil || !ok {
		return nil
	}
	slots, first := openingTexts(fields)
	if len(slots) == 0 {
		return nil
	}
	var skills []skillEntry
	var memory []memoryEntry
	for _, s := range slots {
		skills = append(skills, parseSkills(s.text)...)
		memory = append(memory, parseMemory(s.text)...)
	}
	if len(skills) == 0 && len(memory) == 0 {
		return nil
	}
	rec.Arm = d.arm(fields)
	rec.Skills, rec.Memory = len(skills), len(memory)
	if rec.Arm == armControl {
		return nil
	}
	key := conversationKey(fields)
	dec, known := d.lookup(key)
	if !known {
		if !first {
			// Trimming mid-conversation would rewrite the whole cached prefix.
			return nil
		}
		dec = d.decide(ctx, slots, skills, memory, rec)
		dec.Key, dec.Time = key, time.Now().UTC()
		d.remember(dec)
		rec.Decided = true
	}
	trimSkills, trimMemory := setOf(dec.Skills), setOf(dec.Memory)
	changed := false
	for _, s := range slots {
		text := s.text
		for _, e := range parseSkills(text) {
			if trimSkills[e.name] {
				short := "- " + e.name
				text = strings.Replace(text, e.text, short, 1)
				rec.SkillsTrimmed++
				rec.SavedChars += len(e.text) - len(short)
			}
		}
		for _, e := range parseMemory(text) {
			if trimMemory[e.file] {
				text = strings.Replace(text, e.text, e.short, 1)
				rec.MemoryTrimmed++
				rec.SavedChars += len(e.text) - len(e.short)
			}
		}
		if text != s.text {
			s.set(text)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out, err := fields.MarshalJSON()
	if err != nil {
		return nil
	}
	return out
}

// decide asks Jev to score every entry against the session; on any error it
// returns an empty decision, which keeps the conversation untrimmed for good.
func (d *dieter) decide(ctx context.Context, slots []textSlot, skills []skillEntry, memory []memoryEntry, rec *dietRecord) dietDecision {
	start := time.Now()
	defer func() { rec.LatencyMs = time.Since(start).Milliseconds() }()
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Budget)
	defer cancel()
	session := sessionContext(slots)

	var dec dietDecision
	var skillItems, memoryItems []any
	for _, e := range skills {
		if strings.Contains(e.text, "TRIGGER") {
			continue // explicit trigger rules stay whole
		}
		desc := strings.TrimPrefix(strings.TrimPrefix(e.text, "- "+e.name), ": ")
		if strings.TrimSpace(desc) == "" {
			continue // already a bare name
		}
		skillItems = append(skillItems, typesafe.NewFields().Set("name", e.name).Set("description", truncate(strings.TrimSpace(desc), dietSkillChars)))
	}
	for _, e := range memory {
		if e.text != e.short {
			memoryItems = append(memoryItems, truncate(e.text, dietMemoryChars))
		}
	}
	skillScores := make([]float64, len(skillItems))
	memoryScores := make([]float64, len(memoryItems))
	if d.cfg.Mode != DietAll {
		var skillTokens, memoryTokens int
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() (err error) {
			skillScores, skillTokens, err = d.score(gctx, session, skillItems, dietSkillQuestion, dietSkillLevels)
			return err
		})
		g.Go(func() (err error) {
			memoryScores, memoryTokens, err = d.score(gctx, session, memoryItems, dietMemoryQuestion, dietMemoryLevels)
			return err
		})
		err := g.Wait()
		rec.JevTokens = skillTokens + memoryTokens
		if err != nil {
			rec.Error = err.Error()
			return dec
		}
	}
	trimSkill := trimmed(skillScores, d.cfg.SkillFloor, d.cfg.KeepSkills)
	for i, item := range skillItems {
		if trimSkill[i] {
			name, _ := item.(*typesafe.Fields).Get("name")
			dec.Skills = append(dec.Skills, name.(string))
		}
	}
	trimMemory := trimmed(memoryScores, d.cfg.MemoryFloor, d.cfg.KeepMemory)
	i := 0
	for _, e := range memory {
		if e.text == e.short {
			continue
		}
		if trimMemory[i] {
			dec.Memory = append(dec.Memory, e.file)
		}
		i++
	}
	return dec
}

// trimmed marks the entries scored below floor, except the keep best ones.
func trimmed(scores []float64, floor float64, keep int) []bool {
	order := make([]int, len(scores))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	out := make([]bool, len(scores))
	for rank, i := range order {
		out[i] = rank >= keep && scores[i] < floor
	}
	return out
}

// score asks one score question per item, sending the session once per batch.
func (d *dieter) score(ctx context.Context, session string, items []any, question string, levels []any) ([]float64, int, error) {
	scores := make([]float64, len(items))
	var batches [][2]int
	for i := 0; i < len(items); i += d.cfg.Batch {
		batches = append(batches, [2]int{i, min(i+d.cfg.Batch, len(items))})
	}
	tokens := make([]int, len(batches))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(dietConcurrency)
	for b, span := range batches {
		g.Go(func() error {
			questions := typesafe.NewFields()
			for i := span[0]; i < span[1]; i++ {
				questions.Set("item_"+strconv.Itoa(i-span[0]), typesafe.Score(fmt.Sprintf(question, fmt.Sprintf("items[%d]", i-span[0])), levels))
			}
			state := typesafe.NewFields().Set("context", session).Set("items", items[span[0]:span[1]])
			resp, err := d.jev.SystemOne(gctx, typesafe.Request{State: state, Model: d.model, Questions: questions})
			if err != nil {
				return err
			}
			tokens[b] = resp.Usage.InputTokens
			for i := span[0]; i < span[1]; i++ {
				a, ok := resp.Answers["item_"+strconv.Itoa(i-span[0])]
				if !ok {
					return fmt.Errorf("jev skipped item %d", i)
				}
				scores[i] = a.Score
			}
			return nil
		})
	}
	err := g.Wait()
	total := 0
	for _, t := range tokens {
		total += t
	}
	return scores, total, err
}

// arm picks the A/B side from the session id Claude Code sends in metadata.
func (d *dieter) arm(fields *typesafe.Fields) string {
	if d.cfg.Mode != DietAB {
		return armDiet
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionKey(fields)))
	if h.Sum32()%2 == 0 {
		return armDiet
	}
	return armControl
}

// openingTexts returns the texts of the messages before the first assistant
// turn, where Claude Code puts its instructions, and whether the request is the
// conversation's first (no assistant turn yet).
func openingTexts(fields *typesafe.Fields) ([]textSlot, bool) {
	v, _ := fields.Get("messages")
	msgs, _ := v.([]any)
	var slots []textSlot
	for _, m := range msgs {
		msg, ok := m.(*typesafe.Fields)
		if !ok {
			continue
		}
		if stringField(msg, "role") == "assistant" {
			return slots, false
		}
		switch c := mustGet(msg, "content").(type) {
		case string:
			slots = append(slots, textSlot{role: stringField(msg, "role"), text: c, set: func(s string) { msg.Set("content", s) }})
		case []any:
			for _, b := range c {
				blk, ok := b.(*typesafe.Fields)
				if !ok || stringField(blk, "type") != "text" {
					continue
				}
				slots = append(slots, textSlot{role: stringField(msg, "role"), text: stringField(blk, "text"), set: func(s string) { blk.Set("text", s) }})
			}
		}
	}
	return slots, true
}

func mustGet(f *typesafe.Fields, key string) any {
	v, _ := f.Get(key)
	return v
}

// parseSkills reads the skill listing: entries start with "- name" and may
// carry continuation lines; a blank line ends the listing.
func parseSkills(text string) []skillEntry {
	i := strings.Index(text, skillsHeader)
	if i < 0 {
		return nil
	}
	lines := strings.Split(text[i+len(skillsHeader):], "\n")
	var out []skillEntry
	started := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "- "):
			started = true
			// Plugin skills are named "plugin:skill", so only ": " ends the name.
			name, _, _ := strings.Cut(l[2:], ": ")
			out = append(out, skillEntry{name: strings.TrimSpace(name), text: l})
		case l == "":
			if started {
				return out
			}
		case started:
			out[len(out)-1].text += "\n" + l
		}
	}
	return out
}

// parseMemory reads the MEMORY.md index: "- [Title](file.md) — hook" lines.
func parseMemory(text string) []memoryEntry {
	loc := memoryHeader.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	var out []memoryEntry
	started := false
	for _, l := range strings.Split(text[loc[1]:], "\n") {
		m := memoryLine.FindString(l)
		if m == "" {
			if started {
				return out
			}
			continue
		}
		started = true
		file := m[strings.LastIndex(m, "(")+1 : len(m)-1]
		out = append(out, memoryEntry{file: file, text: l, short: m})
	}
	return out
}

// sessionContext is what Jev reads about the session: the working directory,
// the project instructions and the user's first request.
func sessionContext(slots []textSlot) string {
	var dir, request string
	var project strings.Builder
	for _, s := range slots {
		if m := workingDir.FindStringSubmatch(s.text); m != nil && dir == "" {
			dir = m[1]
		}
		for _, loc := range projectSection.FindAllStringIndex(s.text, -1) {
			rest := s.text[loc[0]:]
			if end := strings.Index(rest[1:], "\nContents of "); end >= 0 {
				rest = rest[:end+1]
			}
			project.WriteString(rest + "\n")
		}
		if s.role == "user" && !strings.HasPrefix(strings.TrimSpace(s.text), "<system-reminder>") && strings.TrimSpace(s.text) != "" {
			request = s.text
		}
	}
	return fmt.Sprintf("Working directory: %s\nProject instructions (excerpt):\n%s\nFirst user request: %s",
		dir, truncate(project.String(), dietContextChars), truncate(request, dietRequestChars))
}

func setOf(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func stringField(f *typesafe.Fields, key string) string {
	s, _ := mustGet(f, key).(string)
	return s
}

func sessionKey(fields *typesafe.Fields) string {
	if md, ok := fieldsAt(fields, "metadata"); ok {
		key, _ := mustGet(md, "user_id").(string)
		return key
	}
	return ""
}

// conversationKey tells the main agent apart from its subagents, which share
// the session id but open with their own first message.
func conversationKey(fields *typesafe.Fields) string {
	h := fnv.New64a()
	if msgs, ok := mustGet(fields, "messages").([]any); ok && len(msgs) > 0 {
		b, _ := json.Marshal(msgs[0])
		_, _ = h.Write(b)
	}
	return sessionKey(fields) + "\x00" + strconv.FormatUint(h.Sum64(), 16)
}
