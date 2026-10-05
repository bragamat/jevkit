package cli

import (
	"github.com/spf13/cobra"
)

const rootLong = `jev-cli puts Jev, TypeSafe's System One model, in a coding agent's toolbox.

Reading: instead of pasting a file into the agent's context, ask Jev where the
answer is and read only those lines.
  find    FILE QUESTION        rank lines by meaning and say whether the answer exists
  pick    QUESTION OPTIONS     choose one of N options (one per line, or a JSON list/object)
  triage  ITEMS SPEC           ask the same typed questions about many items (JSON/JSONL)
  check   CLAIM FILE           does the file support the claim?

Deciding: hand Jev a binary or categorical call and follow the verdict.
  decide  QUESTION OPTION...   choose between options          → ACT | CONFIRM | REPHRASE
  yesno   QUESTION             yes or no                       → YES | NO | UNSURE
  score   QUESTION LEVEL...    place on a scale, lowest first  → ACT | CONFIRM | REPHRASE

Output is short text; --json prints machine-readable JSON instead.
Requires TYPESAFE_API_KEY. Unofficial; not affiliated with TypeSafe AI.`

// NewRoot builds the command tree.
func NewRoot(a *App, version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "jev-cli",
		Short:         "Jev recipes for coding agents: read less, decide with calibrated confidence",
		Long:          rootLong,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.command = cmd.Name()
		},
	}
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

	root.AddCommand(find, pick, triage, check, decide, yesno, score, usage, models)
	return root
}
