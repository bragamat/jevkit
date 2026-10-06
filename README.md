# jevkit

`jev` is a command-line toolkit that puts **Jev**, TypeSafe's System One model, in a coding agent's toolbox.

Coding agents (Claude Code, Codex, Cursor, …) spend most of their budget re-reading context. Every file an
agent pastes into the conversation is paid for again on every following turn. `jev` gives the agent two
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
brew install --cask bragamat/tap/jev
```

**Scoop** (Windows):

```powershell
scoop bucket add bragamat https://github.com/bragamat/scoop-bucket
scoop install bragamat/jev
```

`brew upgrade` and `scoop update jev` pick up new releases.

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
`checksums.txt`, and install `jev` to `~/.local/bin` (Windows: `%LOCALAPPDATA%\Programs\jev`,
added to your user `PATH`). `JEV_VERSION=0.1.1` pins a version and `JEV_INSTALL_DIR` changes the
directory; run the same command again to upgrade.

**Manually:** download `jevkit_<version>_<os>_<arch>` from the
[releases page](https://github.com/bragamat/jevkit/releases), check it with `sha256sum -c checksums.txt
--ignore-missing`, and put `jev` (or `jev.exe`) on your `PATH`. On macOS, a file downloaded
with a browser is quarantined; clear it with `xattr -d com.apple.quarantine jev`.

**With Go 1.26+:** `go install github.com/bragamat/jevkit/cmd/jev@latest`.

> [!NOTE]
> The macOS binaries are not signed or notarized by Apple yet; that is on the way. Until then,
> macOS may refuse to open a `jev` downloaded with a browser ("cannot be opened because the
> developer cannot be verified"). Homebrew and the install script are not affected. For a manual
> download, run `xattr -d com.apple.quarantine jev` once, or allow it in System Settings →
> Privacy & Security.

Then set your key and check the setup:

```sh
export TYPESAFE_API_KEY=...
jev models
```

## Reading

### `find FILE QUESTION` — rank lines by meaning

```console
$ jev find docs/deploy.md "where does the deploy key come from?" --top 3
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
$ jev check "The cache TTL is one hour" config/README.md
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
$ jev triage tickets.jsonl spec.json --label id --sort urgent
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
$ jev decide "Is this failure caused by the change under review?" yes no=pre-existing \
    --ctx failing-test.log --text "The test also fails on main." --risk high
CONFIRM: no  (conf 0.81, risk high, jev-1.13)
  no=0.81  yes=0.19
```

| Command | Question type | Verdicts |
|---|---|---|
| `decide QUESTION OPTION OPTION...` | choice (`name` or `name=description`), 2 to 255 options | `ACT`, `CONFIRM`, `REPHRASE` |
| `yesno QUESTION [--yes ...] [--no ...]` | noul | `YES`, `NO`, `UNSURE` |
| `score QUESTION LEVEL...` | score, 2 to 10 levels lowest first; the verdict names the most likely level | `ACT`, `CONFIRM`, `REPHRASE` |

`decide`, `pick` and `find` ask each choice in two option orders in the same request and average them,
because jev-1.13 leans toward the option listed first. When `decide`'s two orders pick different options,
an `ACT` becomes `CONFIRM` and `--json` reports `"order_consistent": false`.

Evidence comes from `--ctx FILE` (repeatable; `-` reads stdin) and `--text`. It is capped at 60,000
characters: filter first with `jev find` and send only what the decision needs.

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

## Gateway

`jev gateway` is a local proxy between a coding agent and its LLM. Before each request reaches the
model, Jev reads the conversation and the tool list and predicts which tool the next step needs. When it is
confident, the gateway steers the request; otherwise it passes the request through unchanged. Responses,
including streams, go back byte for byte.

```sh
jev --claude [ARGS]   # Claude Code through the gateway (starts it if needed)
jev --codex [ARGS]    # Codex through the gateway (ChatGPT login or API key)
jev gateway status    # ports, upstreams, routing state
jev gateway start|stop|run
```

Everything after `--claude` or `--codex` goes to the agent unchanged, for example
`jev --claude --dangerously-skip-permissions` or `jev --codex --yolo`. A `jev-claude` or `jev-codex`
symlink to `jev` behaves like `jev --claude` / `jev --codex`.

`jev --claude` also sets `ENABLE_TOOL_SEARCH=true` unless you set it yourself. Claude Code turns tool search
off behind any custom `ANTHROPIC_BASE_URL`, which sends every tool definition, MCP servers included, on every
request; with it on, tools load on demand. On a session with one MCP server this cut the first request from
201k to 129k characters.

| Agent | Port | API | How it steers |
|---|---|---|---|
| Claude Code | 8789 | Anthropic Messages | a `<system-reminder>` hint, so thinking and prompt caching keep working |
| Codex | 8790 | OpenAI Responses | `tool_choice` set to the predicted tool, or `none` |

Gateway failures never fail the agent's request: a Jev error, timeout or low confidence means passthrough,
and an upstream 400/422 on a rewritten body is replayed with the original body. Every response carries
`x-jev-gateway-mode`, `-tool`, `-reason`, `-confidence` and `-latency-ms`. Send `x-jev-gateway: off` to skip
routing for one request.

**How it asks Jev.** Two short requests, in the shape of TypeSafe's skill-suggestion cookbook. The first
asks which tool comes next, in two option orders (jev-1.13 leans toward the first option, so the orders must
agree, with `no_tool_needed` first in the main one), plus whether a tool is needed at all. The second
re-checks the top three tools with one yes/no question each, using their full descriptions; if none fits at
least 0.3, the request passes through. More than 120 tools are shortlisted first, in as many parallel requests
as the 64k-token limit needs. Set `JEV_VERIFY=false` to skip the second request.

**Dashboard.** `http://127.0.0.1:8789/dashboard` shows the gateway and jev live over SSE. Gateway
panels: requests per mode, why requests were not steered, Jev latency, and LLM and Jev tokens with cost.
jev panels: the decisions other processes log, and today's calls per command. A switch turns routing
on and off for the whole gateway, and only the dashboard page itself can flip it. Bind to `127.0.0.1` and
publish over a private network such as `tailscale serve`, never to the internet.

| Variable | Default |
|---|---|
| `JEV_GATEWAY_HOST` | `127.0.0.1` |
| `JEV_CLAUDE_PORT` / `JEV_CODEX_PORT` | `8789` / `8790` |
| `JEV_CLAUDE_UPSTREAM_BASE_URL` | `https://api.anthropic.com/v1` |
| `JEV_CODEX_UPSTREAM_BASE_URL` | ChatGPT backend with a ChatGPT login, else `https://api.openai.com/v1` |
| `JEV_MIN_CONFIDENCE` | `0.7` |
| `JEV_ON_NONE` | `passthrough` (leave the request alone when Jev sees no tool needed); `force_none` sends `tool_choice: none`, but only after the conversation has used a tool |
| `JEV_ROUTING` | on (`false` starts with routing disabled) |
| `JEV_BUDGET_MS` | `2500`, the total time Jev may add to one request |
| `JEV_VERIFY` | on (`false` routes on the first answer alone) |
| `JEV_MAX_STATE_CHARS` | `54000`, conversation sent to Jev (newest turns kept) |
| `JEV_GATEWAY_LOG` | `$XDG_STATE_HOME/jev/gateway.jsonl` (rotated at 20 MB) |

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
| `TYPESAFE_DEFAULT_MODEL` | `jev-1.13.0`, pinned because the thresholds were tuned on it (or pass `--model`; `jev-latest` follows new releases) |
| `JEV_USAGE_LOG` | `$XDG_STATE_HOME/jev/usage.jsonl` |
| `JEV_DECISION_LOG` | `$XDG_STATE_HOME/jev/decisions.jsonl` |

Requests that fail with 408, 429 or 5xx, or with a transient network error (timeout, refused or reset
connection, truncated response), are retried up to 2 times with exponential backoff, like TypeSafe's SDK; each attempt times out after 30 s. Other errors, such as
a malformed `TYPESAFE_BASE_URL`, fail at once. A `Retry-After` header is honored up to 60 seconds; longer
values fall back to the normal backoff, so the CLI never stalls an agent for minutes. (The official Python
SDK honors any value and relies on its overall timeout instead.)

Shell completion: `jev completion bash|zsh|fish|powershell --help`. `jev --version` prints the
version, commit and build date.

## Files

`jev usage [--today]` sums the local usage log: calls and input tokens per subcommand, with a cost
estimate. Nothing leaves your machine except the API calls themselves.

## Using it from an agent

The repository ships an [Agent Skill](skills/jev-cli/SKILL.md) that teaches a coding agent when and
how to use `jev`: read with `find` before opening a large file, `check` a claim before stating
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
- Before reading a large file to find something, run `jev find FILE "question"` and read only the lines it returns.
- Before stating a fact about code or docs, run `jev check "claim" FILE`.
- For a yes/no or multiple-choice judgment about your own work, run `jev decide` / `jev yesno` with the
  evidence and follow the verdict: ACT → proceed, CONFIRM → gather more evidence, REPHRASE → rethink the question.
```

## Development

```sh
go test ./...
go build -o jev ./cmd/jev
```

`scripts/ci.sh` fetches a pinned, checksum-verified Go toolchain when none is installed, then checks
`go mod tidy`, gofmt and vet, runs the tests with the race detector, and builds. `LINT=1` adds
[golangci-lint](.golangci.yml) and govulncheck; `TIDY=fix` rewrites `go.mod`/`go.sum` instead of failing.
The tests use a fake API server and never call TypeSafe. `go test -fuzz FuzzDecodeOrdered ./internal/typesafe`
fuzzes the order-preserving JSON decoder.

Releases are cut by pushing a `vX.Y.Z` tag: CI runs first, then GoReleaser publishes the binaries.

## License

[MIT](LICENSE)
