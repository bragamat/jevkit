---
name: jev-cli
description: "Use jev-cli (TypeSafe's Jev model) to read less and to make calibrated judgment calls. Use it BEFORE reading a large file, log or doc page to find something (jev-cli find), before choosing one of many tools, files or options (pick), when the same question applies to many items (triage), before stating a fact about code or docs (check), and whenever a yes/no or multiple-choice decision about your own work depends on meaning: classifying, choosing between approaches, judging whether something applies, is current, or is relevant (decide, yesno, score)."
---

# jev-cli: read less, decide with calibrated confidence

`jev-cli` puts Jev, TypeSafe's System One model, in your toolbox. Jev answers *typed* questions
(choice, yes/no, score) with probabilities in about a second, at $0.042 per million input tokens.
Two things follow:

- **Reading through Jev is cheaper than reading yourself.** Every token you paste into the
  conversation is paid again on every following turn. What Jev reads never comes back to you.
  Ask Jev *where* the answer is and read only those lines.
- **Jev's confidence is calibrated.** Hand it a judgment call with the evidence, and the verdict
  tells you whether to act, gather more evidence, or rethink the question.

You still own the work. Jev owns narrow judgments you would otherwise make by gut feel.

## Before the first call

```sh
# Linux and macOS: Homebrew when present, otherwise the install script (~/.local/bin).
command -v jev-cli >/dev/null || { command -v brew >/dev/null && brew install --cask bragamat/tap/jev-cli; } \
  || curl -fsSL https://raw.githubusercontent.com/bragamat/jevkit/main/install.sh | sh
test -n "$TYPESAFE_API_KEY" || echo "TYPESAFE_API_KEY is not set: ask the user for a key from console.typesafe.ai"
```

```powershell
# Windows: Scoop when present, otherwise the install script (%LOCALAPPDATA%\Programs\jev-cli).
if (-not (Get-Command jev-cli -ErrorAction SilentlyContinue)) {
  if (Get-Command scoop -ErrorAction SilentlyContinue) { scoop bucket add bragamat https://github.com/bragamat/scoop-bucket; scoop install bragamat/jev-cli }
  else { irm https://raw.githubusercontent.com/bragamat/jevkit/main/install.ps1 | iex }
}
if (-not $env:TYPESAFE_API_KEY) { "TYPESAFE_API_KEY is not set: ask the user for a key from console.typesafe.ai" }
```

Installing software on the user's machine needs their go-ahead if your instructions require it. Never print, log or commit the key. If it is missing, stop and ask the user; do not work around it.

## Reading

| Situation | Command |
|---|---|
| Find where a large file answers a question, and whether it does at all | `jev-cli find FILE "question" [--top 5]` |
| A statement you are about to publish: does the source back it? | `jev-cli check "claim" FILE` |
| Choose one of many items (tools, skills, files, tables) | `jev-cli pick "question" OPTIONS_FILE [--state FILE]` |
| The same questions about many items | `jev-cli triage ITEMS SPEC [--label field] [--sort id]` |

- **`find`** prints `exists=` (does the file answer the question: `answers` ≥ 0.6, `partial`
  ≥ 0.3) and the top lines with their scores. Read only those lines, with a few lines around them
  if needed (`sed -n 'A,Bp'`). With `exists` below 0.3 the file does not answer: look elsewhere
  instead of reading it to be sure.
- **`check`** returns `supports`, `partially`, `contradicts` or `not_addressed`. With the last
  two, fix your statement before you publish it.
- **`pick`**: `OPTIONS_FILE` is one option per line, a JSON list, or a JSON object of option →
  description. Descriptions improve accuracy. Large lists run in two rounds automatically.
- **`triage`**: see [references/triage.md](references/triage.md) for the spec format and the
  shared-context batch mode.

## Deciding

```sh
jev-cli decide "QUESTION" opt1="what it means" opt2="what it means" ... [--ctx FILE|-] [--text "..."] [--risk high]
jev-cli yesno  "QUESTION" [--yes "what counts as yes"] [--no "what counts as no"] [--ctx ...] [--text ...]
jev-cli score  "QUESTION" "lowest level" "middle level" "highest level" [--ctx ...] [--text ...]
```

1. **Recognize the decision.** "Is this test failure caused by my change?", "which module owns
   this?", "is this doc page out of date?", "rewrite or patch?". If the answer depends on meaning,
   ask Jev. If it depends on arithmetic, dates or exact matching, compute it (see below).
2. **Gather only the evidence the question needs** and pass it with `--ctx` (repeatable, `-` for
   stdin) or `--text`. Evidence is capped at 60,000 characters; large, noisy evidence lowers
   accuracy, so filter it first with `find`.
3. **Ask one literal question.** Jev answers what is written, not what you meant. Put edge cases
   in the option descriptions. For `decide`, include a "none of these" option when nothing may fit.
4. **Follow the verdict.**

| Verdict | What you do |
|---|---|
| `ACT`, `YES`, `NO` | Proceed with the answer; do not reopen it. When it matters, tell the user ("Jev classified this as X, conf 0.93"). |
| `CONFIRM` | Gather more evidence and ask again. If it stays in the middle, take the more conservative option and say so. |
| `REPHRASE`, `UNSURE` | Do not act on the guess. The question is vague, mixes two judgments, or lacks evidence: split it, reword it, or bring the missing evidence. |

Use `--risk high` when a wrong call is costly (hard to undo, visible to others). The bands rise:
`ACT` needs confidence ≥ 0.90; `YES` needs p ≥ 0.85 and `NO` p ≤ 0.15.

- **`decide`** (choice) is relative: some option always wins, and the confidence says whether it
  won clearly.
- **`yesno`** is an absolute probability of yes. A value near 0.5 is doubt, not "half yes".
- **`score`** places the case on an ordered scale. Give each level as a concrete, self-contained
  situation, lowest first. Branch on the level; do not treat it as a measurement.

**Jev decides judgment, not authorization.** A destructive, external or irreversible action still
needs whatever confirmation the user or your instructions require, whatever the verdict. A
decision the user already made does not go to Jev.

## Keep it in code, not in Jev

- Arithmetic, counting, comparing numbers or versions, dates and ranges. To count items that meet
  a criterion, ask one `yesno` per item (or `triage`) and add them up in code.
- Exact search for a function name, an id or a literal string: use `grep`.
- Generating text or extracting a free-form value: find the candidates in code, then let Jev
  choose among them with `decide` or `pick`.

More on the model's limits: [references/limits.md](references/limits.md).

## Output and errors

- Text by default, short enough to keep. `--json` (anywhere on the line) prints one JSON
  document per call, JSONL for `triage`. Print little: `--top`, `head`, selected fields.
- Errors are one line on stderr. Exit status 1 means the call failed (bad input, API, network),
  2 means you misused the CLI (read `jev-cli CMD --help`), 130 means interrupted.
- **Quote heredocs that hold a spec or question with backticks** (`<<'EOF'`). Unquoted, the shell
  runs the backticks and Jev gets a question with the field names missing, with no error.
- Every decision is appended to `$XDG_STATE_HOME/jev/decisions.jsonl` for audit;
  `jev-cli usage [--today]` sums the tokens Jev read and their cost.
