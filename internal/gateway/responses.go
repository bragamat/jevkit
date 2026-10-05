package gateway

import (
	"encoding/json"
	"strings"

	"github.com/bragamat/jevkit/internal/typesafe"
)

type responsesAdapter struct{}

// defaultNamespace holds the tools addressed by bare name, like top-level tools.
const defaultNamespace = "functions"

func qualified(namespace, name string) string {
	if namespace != "" && namespace != defaultNamespace {
		return namespace + "." + name
	}
	return name
}

var hostedDescriptions = map[string]string{
	"web_search":         "Search the web for up-to-date information the assistant does not already have.",
	"web_search_preview": "Search the web for up-to-date information the assistant does not already have.",
	"local_shell":        "Run a shell command on the user's machine.",
	"image_generation":   "Generate an image.",
	"code_interpreter":   "Run Python code in a sandbox.",
	"file_search":        "Search the user's uploaded files.",
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Tools       []responsesTool `json:"tools"`
}

type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      *string         `json:"name"`
	Namespace string          `json:"namespace"`
	CallID    string          `json:"call_id"`
	Arguments *string         `json:"arguments"`
	Input     *string         `json:"input"`
	Output    json.RawMessage `json:"output"`
	Action    struct {
		Command []string `json:"command"`
	} `json:"action"`
	Tools []responsesTool `json:"tools"`
}

type responsesRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions"`
	Input              json.RawMessage `json:"input"`
	Tools              []responsesTool `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	PreviousResponseID string          `json:"previous_response_id"`
}

func (responsesAdapter) toInput(body []byte, maxMessageChars int) (input, string, string, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return input{}, "", "", err
	}
	// With server-side history the router would judge a conversation it cannot see.
	if req.PreviousResponseID != "" {
		return input{}, req.Model, "previous_response_id", nil
	}
	var items []responsesItem
	var s string
	if json.Unmarshal(req.Input, &s) == nil {
		items = []responsesItem{{Type: "message", Role: "user", Content: req.Input}}
	} else {
		_ = json.Unmarshal(req.Input, &items)
	}
	declared := append([]responsesTool(nil), req.Tools...)
	for _, it := range items {
		// Delegated tasks and replies use a separate format whose content may be encrypted.
		if it.Type == "agent_message" {
			return input{}, req.Model, "agent_message", nil
		}
		if it.Type == "additional_tools" {
			declared = append(declared, it.Tools...)
		}
	}

	clip := func(s string) string { return truncate(s, maxMessageChars) }
	in := input{Steer: steerToolChoice, Tools: responsesTools(declared)}
	var system []string
	if req.Instructions != "" {
		system = append(system, req.Instructions)
	}
	names := map[string]string{}
	for _, it := range items {
		kind := it.Type
		if kind == "" && it.Role != "" {
			kind = "message"
		}
		switch {
		case kind == "message":
			text := clip(textOf(it.Content))
			if it.Role == "system" || it.Role == "developer" {
				if text != "" {
					system = append(system, text)
				}
				continue
			}
			role := it.Role
			if role == "" {
				role = "user"
			}
			in.Turns = append(in.Turns, textTurn(role, text))
		case kind == "function_call" || kind == "custom_tool_call":
			name := "unknown"
			if it.Name != nil {
				name = *it.Name
			}
			name = qualified(it.Namespace, name)
			if it.CallID != "" {
				names[it.CallID] = name
			}
			args := ""
			if it.Arguments != nil {
				args = *it.Arguments
			} else if it.Input != nil {
				args = *it.Input
			}
			in.Turns = append(in.Turns, callTurn(name, clip(args)))
		case kind == "local_shell_call":
			if it.CallID != "" {
				names[it.CallID] = "local_shell"
			}
			in.Turns = append(in.Turns, callTurn("local_shell", clip(strings.Join(it.Action.Command, " "))))
		case strings.HasSuffix(kind, "_call_output"):
			name := names[it.CallID]
			if name == "" {
				name = "unknown"
			}
			in.Turns = append(in.Turns, resultTurn(name, clip(textOf(it.Output))))
		}
		// Reasoning items (encrypted), item references and hosted-tool traces carry nothing Jev can read.
	}
	in.System = strings.Join(system, "\n\n")

	in.ToolChoice = choiceDecided
	var choice string
	if len(req.ToolChoice) == 0 || string(req.ToolChoice) == "null" {
		in.ToolChoice = choiceAuto
	} else if json.Unmarshal(req.ToolChoice, &choice) == nil && (choice == choiceAuto || choice == choiceRequired) {
		in.ToolChoice = choice
	}
	return in, req.Model, "", nil
}

// responsesTools flattens namespaces; a later tool with the same name replaces
// an earlier one, as the API does.
func responsesTools(raw []responsesTool) []tool {
	var out []tool
	index := map[string]int{}
	put := func(t tool) {
		if i, ok := index[t.Name]; ok {
			out[i] = t
			return
		}
		index[t.Name] = len(out)
		out = append(out, t)
	}
	var add func(t responsesTool, ns *responsesTool)
	add = func(t responsesTool, ns *responsesTool) {
		switch {
		case t.Type == "namespace":
			for _, nested := range t.Tools {
				add(nested, &t)
			}
		case (t.Type == kindFunction || t.Type == kindCustom) && t.Name != "":
			nsName, group := "", ""
			if ns != nil {
				nsName = ns.Name
			}
			name := qualified(nsName, t.Name)
			desc := ""
			if t.Description != nil {
				desc = *t.Description
			}
			nt := tool{Kind: t.Type, Name: name, Description: desc}
			if t.Type == kindFunction {
				nt.Params = propertyNames(propertiesOf(t.Parameters))
			}
			if name != t.Name {
				nt.Namespace = nsName
				if ns.Description != nil {
					group = strings.TrimSpace(*ns.Description)
				}
				if group != "" {
					nt.Description = strings.TrimSpace("[" + group + "] " + desc)
				}
			}
			put(nt)
		case t.Type != "":
			if _, ok := index[t.Type]; ok {
				return
			}
			desc, ok := hostedDescriptions[t.Type]
			if !ok && t.Description != nil {
				desc = *t.Description
			} else if !ok {
				desc = "The built-in " + t.Type + " tool."
			}
			put(tool{Kind: kindHosted, Name: t.Type, Description: desc})
		}
	}
	for _, t := range raw {
		add(t, nil)
	}
	return out
}

func propertiesOf(schema json.RawMessage) json.RawMessage {
	var s struct {
		Properties json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(schema, &s)
	return s.Properties
}

func (responsesAdapter) apply(body *typesafe.Fields, d decision) bool {
	switch d.Mode {
	case modeForced:
		kind := kindFunction
		if d.Tool.Kind == kindCustom {
			kind = kindCustom
		}
		body.Set("tool_choice", typesafe.NewFields().Set("type", kind).Set("name", d.Tool.Name))
		return true
	case modeNone:
		body.Set("tool_choice", "none")
		return true
	}
	return false
}
