# jevkit

`jev-cli` is a command-line toolkit that puts **Jev**, TypeSafe's System One model, in a coding agent's toolbox.

Coding agents (Claude Code, Codex, Cursor, …) spend most of their budget re-reading context. Every file an
agent pastes into the conversation is paid for again on every following turn. `jev-cli` gives the agent two
cheaper moves:

- **Read less.** Ask Jev *where* the answer is and read only those lines, instead of reading the file.
- **Decide with calibrated confidence.** Hand Jev a yes/no or multiple-choice call about the agent's own
  work, and get back a verdict (`ACT`, `CONFIRM`, `REPHRASE`) tuned to how costly a wrong call would be.

Jev answers typed questions with probabilities in a few hundred milliseconds, at $0.042 per million input
tokens, so a lookup costs a small fraction of what pasting the file would.

> **Unofficial.** jevkit is a community project. It is not affiliated with, endorsed by, or sponsored by
> TypeSafe AI. "Jev" and "TypeSafe" are their names; you need your own API key from
> [console.typesafe.ai](https://console.typesafe.ai).

## Install

Prebuilt binaries for Linux, macOS and Windows (amd64 and arm64). No Go toolchain needed.

**Homebrew** (macOS and Linux):

```sh
brew install --cask bragamat/tap/jev-cli
```

**Scoop** (Windows):

```powershell
scoop bucket add bragamat https://github.com/bragamat/scoop-bucket
scoop install bragamat/jev-cli
```

`brew upgrade` and `scoop update jev-cli` pick up new releases.

**Install script, Linux and macOS:**

```sh
curl -fsSL https://raw.githubusercontent.com/bragamat/jevkit/main/install.sh | sh
```

**Install script, Windows** (PowerShell):

```powershell
irm https://raw.githubusercontent.com/bragamat/jevkit/main/install.ps1 | iex
```

The scripts download the archive for your OS and CPU from the
[latest release](https://github.com/bragamat/jevkit/releases/latest), verify it against
`checksums.txt`, and install `jev-cli` to `~/.local/bin` (Windows: `%LOCALAPPDATA%\Programs\jev-cli`,
added to your user `PATH`). `JEV_VERSION=0.1.1` pins a version and `JEV_INSTALL_DIR` changes the
directory; run the same command again to upgrade.

**Manually:** download `jevkit_<version>_<os>_<arch>` from the
[releases page](https://github.com/bragamat/jevkit/releases), check it with `sha256sum -c checksums.txt
--ignore-missing`, and put `jev-cli` (or `jev-cli.exe`) on your `PATH`. On macOS, a file downloaded
with a browser is quarantined; clear it with `xattr -d com.apple.quarantine jev-cli`.

**With Go 1.26+:** `go install github.com/bragamat/jevkit/cmd/jev-cli@latest`.

Then set your key and check the setup:

```sh
export TYPESAFE_API_KEY=...
jev-cli models
```

## Reading

### `find FILE QUESTION` — rank lines by meaning

```console
$ jev-cli find docs/deploy.md "where does the deploy key come from?" --top 3
exists=0.91 (answers)  docs/deploy.md
  0.84  L212: The deploy key is read from Vault at boot (secret/deploy/key).
  0.05  L48: Keys rotate every 90 days.
  0.01  L213: See ops/vault.md for access.
```

`exists` says whether the document answers the question at all (`answers` ≥ 0.6, `partial` ≥ 0.3,
otherwise `does NOT answer`), so the agent can stop looking instead of reading the whole file to be sure.
Long files are split into windows of 250 lines that run in parallel.

### `check CLAIM FILE` — does the source back the claim?

```console
$ jev-cli check "The cache TTL is one hour" config/README.md
contradicts (conf 0.88, exists 0.93)
  L31: Responses are cached for 10 minutes.
```

Relations: `supports`, `partially`, `contradicts`, `not_addressed`. Use it before publishing a statement
about code or data.

### `pick QUESTION OPTIONS_FILE` — choose one of N

`OPTIONS_FILE` holds one option per line, a JSON list, or a JSON object of option → description. Lists
longer than 250 run in two rounds: the best three of each group compete in a final. `--state FILE` adds
the context the choice depends on.

### `triage ITEMS SPEC` — the same questions about many items

`ITEMS` is a JSON array or JSONL. `SPEC` is a JSON object of typed questions:

```json
{
  "urgent": {"type": "noul", "instructions": "Does `item` need action today?"},
  "area":   {"type": "choice", "instructions": "Which team owns `item`?", "criteria": ["billing", "infra", "docs"]},
  "effort": {"type": "score", "instructions": "How much work is `item`?", "criteria": ["small", "medium", "large"]}
}
```

```console
$ jev-cli triage tickets.jsonl spec.json --label id --sort urgent
T-104 | urgent=0.97 | area=infra | area_conf=0.91 | effort=1.2 | effort_conf=0.74
T-101 | urgent=0.12 | area=docs | area_conf=0.88 | effort=0.1 | effort_conf=0.93
```

By default each item is its own request (state `{"item": ...}`). With `--context FILE`, the shared
context goes once per batch of `--batch` items (default 20): the state becomes
`{"context": ..., "items": [...]}` and every reference to `` `item` `` in the questions is rewritten to
`` `items[i]` ``, one question per item.

## Deciding

The agent states the evidence, asks, and follows the verdict:

```console
$ jev-cli decide "Is this failure caused by the change under review?" yes no=pre-existing \
    --ctx failing-test.log --text "The test also fails on main." --risk high
CONFIRM: no  (conf 0.81, risk high, jev-1.13)
  no=0.81  yes=0.19
```

| Command | Question type | Verdicts |
|---|---|---|
| `decide QUESTION OPTION OPTION...` | choice (`name` or `name=description`) | `ACT`, `CONFIRM`, `REPHRASE` |
| `yesno QUESTION [--yes ...] [--no ...]` | noul | `YES`, `NO`, `UNSURE` |
| `score QUESTION LEVEL...` | score, levels lowest first | `ACT`, `CONFIRM`, `REPHRASE` |

Evidence comes from `--ctx FILE` (repeatable; `-` reads stdin) and `--text`. It is capped at 60,000
characters: filter first with `jev-cli find` and send only what the decision needs.

`--risk` sets how costly a wrong call is. The bands follow TypeSafe's
[confidence guidance](https://docs.typesafe.ai/confidence):

| | `ACT` / `YES` from | `REPHRASE` below / `NO` up to |
|---|---|---|
| choice, score, `--risk low` (default) | 0.70 | 0.50 |
| choice, score, `--risk high` | 0.90 | 0.50 |
| yes/no, `--risk low` | p ≥ 0.70 | p ≤ 0.30 |
| yes/no, `--risk high` | p ≥ 0.85 | p ≤ 0.15 |

`CONFIRM` and `UNSURE` mean: gather more evidence or ask a person. `REPHRASE` means the question itself is
probably wrong.

Every decision is appended to a local log (see [Files](#files)) so you can audit what the agent decided
and why.

## Output

Text by default, short enough to land in an agent's context without cost. `--json` (before or after the
subcommand) prints one JSON document per call, or JSONL for `triage`. Errors print one line on stderr,
prefixed with the subcommand. A missing file, a bad JSON spec or a missing key never produces a stack trace.

| Exit status | Meaning |
|---|---|
| 0 | success |
| 1 | the call failed (bad input, API error, network) |
| 2 | usage error: unknown command or flag, wrong number of arguments |
| 130 | interrupted (Ctrl-C or SIGTERM); in-flight requests are cancelled |

## Configuration

| Variable | Default |
|---|---|
| `TYPESAFE_API_KEY` | required |
| `TYPESAFE_BASE_URL` | `https://api.typesafe.ai` |
| `TYPESAFE_DEFAULT_MODEL` | `jev-latest` (or pass `--model`) |
| `JEV_USAGE_LOG` | `$XDG_STATE_HOME/jev/usage.jsonl` |
| `JEV_DECISION_LOG` | `$XDG_STATE_HOME/jev/decisions.jsonl` |

Requests that fail with 408, 429 or 5xx, or with a transient network error (timeout, refused or reset
connection, truncated response), are retried up to 3 times with exponential backoff. Other errors, such as
a malformed `TYPESAFE_BASE_URL`, fail at once. A `Retry-After` header is honored up to 60 seconds; longer
values fall back to the normal backoff, so the CLI never stalls an agent for minutes. (The official Python
SDK honors any value and relies on its overall timeout instead.)

Shell completion: `jev-cli completion bash|zsh|fish|powershell --help`. `jev-cli --version` prints the
version, commit and build date.

## Files

`jev-cli usage [--today]` sums the local usage log: calls and input tokens per subcommand, with a cost
estimate. Nothing leaves your machine except the API calls themselves.

## Using it from an agent

The repository ships an [Agent Skill](skills/jev-cli/SKILL.md) that teaches a coding agent when and
how to use `jev-cli`: read with `find` before opening a large file, `check` a claim before stating
it, and hand judgment calls to `decide` / `yesno` / `score` and follow the verdict.

**Claude Code** (plugin):

```sh
claude plugin marketplace add bragamat/jevkit
claude plugin install jevkit@jevkit
```

**Codex, Cursor, Gemini CLI and other agents** that read `SKILL.md` folders:

```sh
npx skills add bragamat/jevkit
```

or copy `skills/jev-cli/` into the agent's skills directory (for Codex, `~/.codex/skills/`).

Agents without skill support can take a short rule in `AGENTS.md` or `CLAUDE.md` instead:

```markdown
- Before reading a large file to find something, run `jev-cli find FILE "question"` and read only the lines it returns.
- Before stating a fact about code or docs, run `jev-cli check "claim" FILE`.
- For a yes/no or multiple-choice judgment about your own work, run `jev-cli decide` / `jev-cli yesno` with the
  evidence and follow the verdict: ACT → proceed, CONFIRM → gather more evidence, REPHRASE → rethink the question.
```

## Development

```sh
go test ./...
go build -o jev-cli ./cmd/jev-cli
```

`scripts/ci.sh` fetches a pinned, checksum-verified Go toolchain when none is installed, then checks
`go mod tidy`, gofmt and vet, runs the tests with the race detector, and builds. `LINT=1` adds
[golangci-lint](.golangci.yml) and govulncheck; `TIDY=fix` rewrites `go.mod`/`go.sum` instead of failing.
The tests use a fake API server and never call TypeSafe. `go test -fuzz FuzzDecodeOrdered ./internal/typesafe`
fuzzes the order-preserving JSON decoder.

Releases are cut by pushing a `vX.Y.Z` tag: CI runs first, then GoReleaser publishes the binaries.

## License

[MIT](LICENSE)
