package gateway

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Tool kinds: function and custom tools run on the client and can be forced
// by name; hosted tools run at the provider and can only be logged.
const (
	kindFunction = "function"
	kindCustom   = "custom"
	kindHosted   = "hosted"
)

// tool is one tool offered to the LLM, normalized across wire formats.
type tool struct {
	Kind        string
	Name        string
	Description string
	Params      []string
	Namespace   string
}

// toolCall and turn are what Jev reads: one JSON object per conversation step.
type toolCall struct {
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"`
}

type turn struct {
	Role      string     `json:"role"`
	Text      *string    `json:"text,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	Tool      string     `json:"tool,omitempty"`
	Content   *string    `json:"content,omitempty"`
}

func textTurn(role, text string) turn { return turn{Role: role, Text: &text} }

func callTurn(name, args string) turn {
	return turn{Role: "assistant", ToolCalls: []toolCall{{Tool: name, Arguments: args}}}
}

func resultTurn(name, content string) turn {
	return turn{Role: "tool_result", Tool: name, Content: &content}
}

// Steering: a hint is a note appended after the conversation, which keeps the
// prompt cache and works with extended thinking; tool_choice forces the call.
const (
	steerHint       = "hint"
	steerToolChoice = "tool_choice"
)

// Tool choice the client asked for.
const (
	choiceAuto     = "auto"
	choiceRequired = "required"
	choiceDecided  = "decided"
)

// input is a request reduced to what routing needs.
type input struct {
	System     string
	Turns      []turn
	Tools      []tool
	ToolChoice string
	Steer      string
}

const truncMarker = " …[truncated]… "

// truncate keeps the head and tail of long text (60/40); the middle matters
// least for routing. It counts runes, so it never splits a character.
func truncate(text string, maxRunes int) string {
	n := utf8.RuneCountInString(text)
	if n <= maxRunes {
		return text
	}
	r := []rune(text)
	keep := max(0, maxRunes-utf8.RuneCountInString(truncMarker))
	head := (keep*6 + 9) / 10
	return string(r[:head]) + truncMarker + string(r[n-(keep-head):])
}

// textOf flattens message content: a string as is, content parts by their
// text, anything else as a [type] placeholder, since Jev reads only text.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		var part struct {
			Type string `json:"type"`
			Text *string
		}
		_ = json.Unmarshal(p, &part)
		switch {
		case part.Text != nil:
			out = append(out, *part.Text)
		case part.Type != "":
			out = append(out, "["+part.Type+"]")
		default:
			out = append(out, "[attachment]")
		}
	}
	return strings.Join(out, "\n")
}

// jevState is the content Jev's questions are asked about.
type jevState struct {
	AssistantInstructions string `json:"assistant_instructions,omitempty"`
	EarlierTurnsOmitted   int    `json:"earlier_turns_omitted,omitempty"`
	Conversation          []turn `json:"conversation"`
}

// buildState keeps the newest turns that fit the budget; the newest one is
// always kept.
func buildState(in input, maxStateChars, maxMessageChars int) jevState {
	system := truncate(in.System, maxMessageChars)
	budget := maxStateChars - utf8.RuneCountInString(system)
	start := len(in.Turns)
	for i := len(in.Turns) - 1; i >= 0; i-- {
		b, _ := json.Marshal(in.Turns[i])
		budget -= utf8.RuneCount(b)
		if budget < 0 && start < len(in.Turns) {
			break
		}
		start = i
	}
	return jevState{
		AssistantInstructions: system,
		EarlierTurnsOmitted:   start,
		Conversation:          in.Turns[start:],
	}
}
