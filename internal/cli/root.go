package cli

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

const rootLong = `jev puts Jev, TypeSafe's System One model, in a coding agent's toolbox.

Reading: instead of pasting a file into the agent's context, ask Jev where the
answer is and read only those lines.

Deciding: hand Jev a binary or categorical call and follow the verdict:
decide and score answer ACT | CONFIRM | REPHRASE, yesno answers YES | NO | UNSURE.

Output is short text; --json prints machine-readable JSON instead. Errors are one
line on stderr; the exit status is 1 for a failed call, 2 for bad usage, 130 when
interrupted. Requires TYPESAFE_API_KEY. Unofficial; not affiliated with TypeSafe AI.`

// usageError is a command line the parser rejected: wrong arguments or flags.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// IsUsageError reports whether err comes from a malformed command line.
func IsUsageError(err error) bool {
	var u usageError
	// Cobra reports an unknown subcommand of the root as a plain error.
	return errors.As(err, &u) || strings.HasPrefix(err.Error(), "unknown command ")
}

// usageArgs marks argument validation failures as usage errors.
func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// NewRoot builds the command tree.
func NewRoot(a *App, version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "jev",
		Short:         "Jev recipes for coding agents: read less, decide with calibrated confidence",
		Long:          rootLong,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			a.command = cmd.Name()
			return nil
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddGroup(
		&cobra.Group{ID: "read", Title: "Reading:"},
		&cobra.Group{ID: "decide", Title: "Deciding:"},
		&cobra.Group{ID: "gateway", Title: "Gateway:"},
	)
	// Handled in main before parsing, so the agent's own flags pass through; declared
	// here only so --help lists them.
	root.Flags().Bool("claude", false, "start Claude Code through the gateway; every later argument goes to claude")
	root.Flags().Bool("codex", false, "start Codex through the gateway; every later argument goes to codex")
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "print JSON")
	root.PersistentFlags().StringVar(&a.model, "model", "", "model name (default: $TYPESAFE_DEFAULT_MODEL or jev-latest)")

	var top int
	find := &cobra.Command{
		Use:   "find FILE QUESTION",
		Short: "Rank a file's lines by how well they answer QUESTION",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runFind(cmd.Context(), args[0], args[1], top)
		},
	}
	find.Flags().IntVar(&top, "top", 5, "lines to show")

	var pickTop int
	var pickState string
	pick := &cobra.Command{
		Use:   "pick QUESTION OPTIONS_FILE",
		Short: "Choose one of the options in OPTIONS_FILE",
		Long: "Choose one of the options in OPTIONS_FILE: one option per line, a JSON list, or a JSON\n" +
			"object of option → description. More than 250 options run in two rounds.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runPick(cmd.Context(), args[0], args[1], pickState, pickTop)
		},
	}
	pick.Flags().IntVar(&pickTop, "top", 5, "options to show")
	pick.Flags().StringVar(&pickState, "state", "", "file with the context for the choice")

	var to triageOptions
	triage := &cobra.Command{
		Use:   "triage ITEMS SPEC",
		Short: "Ask the same typed questions about every item",
		Long: "Ask the questions in SPEC about every item in ITEMS (a JSON array or JSONL).\n\n" +
			"SPEC is a JSON object of questions:\n" +
			`  {"urgent": {"type": "noul", "instructions": "Is ` + "`item`" + ` urgent?"},` + "\n" +
			`   "area": {"type": "choice", "instructions": "Which area owns ` + "`item`" + `?", "criteria": ["billing", "infra"]},` + "\n" +
			`   "effort": {"type": "score", "instructions": "How much work is ` + "`item`" + `?", "criteria": ["small", "medium", "large"]}}` + "\n\n" +
			"Without --context each item is its own request and the state is {\"item\": ...}.\n" +
			"With --context the shared context goes once per batch, the state is {\"context\": ..., \"items\": [...]},\n" +
			"and references to `item` in the questions are rewritten to `items[i]`.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTriage(cmd.Context(), args[0], args[1], to)
		},
	}
	triage.Flags().StringVar(&to.Label, "label", "", "item field that names each row")
	triage.Flags().StringVar(&to.SortBy, "sort", "", "question id to sort by, highest first")
	triage.Flags().StringVar(&to.Context, "context", "", "file (JSON or text) sent once per batch")
	triage.Flags().IntVar(&to.Batch, "batch", 20, "items per request with --context")

	check := &cobra.Command{
		Use:   "check CLAIM FILE",
		Short: "Say whether FILE supports, contradicts or ignores CLAIM",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runCheck(cmd.Context(), args[0], args[1])
		},
	}

	var e evidence
	decide := &cobra.Command{
		Use:   "decide QUESTION OPTION OPTION...",
		Short: "Choose between options (each OPTION is name or name=description)",
		Args:  cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runDecide(cmd.Context(), args[0], args[1:], e)
		},
	}
	var yes, no string
	yesno := &cobra.Command{
		Use:   "yesno QUESTION",
		Short: "Answer a yes-or-no question",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runYesNo(cmd.Context(), args[0], yes, no, e)
		},
	}
	yesno.Flags().StringVar(&yes, "yes", "", "what counts as yes")
	yesno.Flags().StringVar(&no, "no", "", "what counts as no")
	score := &cobra.Command{
		Use:   "score QUESTION LEVEL...",
		Short: "Place the evidence on a scale of levels, lowest first",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runScore(cmd.Context(), args[0], args[1:], e)
		},
	}
	for _, c := range []*cobra.Command{decide, yesno, score} {
		c.Flags().StringArrayVar(&e.Files, "ctx", nil, "file with evidence (repeatable; - reads stdin)")
		c.Flags().StringVar(&e.Text, "text", "", "short inline evidence")
		c.Flags().StringVar(&e.Risk, "risk", "low", "cost of a wrong call: low or high")
		_ = c.RegisterFlagCompletionFunc("risk", cobra.FixedCompletions([]string{"low", "high"}, cobra.ShellCompDirectiveNoFileComp))
	}

	var today bool
	usage := &cobra.Command{
		Use:   "usage",
		Short: "Show the tokens Jev read, from the local usage log",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runUsage(today)
		},
	}
	usage.Flags().BoolVar(&today, "today", false, "only today")

	models := &cobra.Command{
		Use:   "models",
		Short: "List the models the API serves",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runModels(cmd.Context())
		},
	}

	for _, c := range []*cobra.Command{find, pick, triage, check} {
		c.GroupID = "read"
	}
	for _, c := range []*cobra.Command{decide, yesno, score} {
		c.GroupID = "decide"
	}
	for _, c := range []*cobra.Command{find, pick, triage, check, decide, yesno, score, usage, models} {
		c.Args = usageArgs(c.Args)
	}
	root.AddCommand(find, pick, triage, check, decide, yesno, score, usage, models)
	root.AddCommand(a.gatewayCommands()...)
	return root
}
