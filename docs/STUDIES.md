# What we learned trying to cut coding-agent tokens with Jev

jevkit was an experiment: can a small, cheap, calibrated classifier (Jev, TypeSafe's System One model) make
coding agents such as Claude Code and Codex spend fewer tokens? This page records what we measured before
archiving the project. All numbers are aggregates. No prompts, transcripts or project data are included.

## Setup

- One Linux VPS running several long-lived Claude Code sessions and some Codex sessions across a dozen
  repositories.
- `jev gateway` sat between the agents and the model APIs, so every request could be logged, measured and,
  optionally, rewritten.
- Each idea was tested with an A/B: two gateways on the same build (one with the feature, one without), the
  same short read-only tasks run in both, arms alternated, and a per-run nonce in the prompt so the arms could
  not share a prompt cache. Correctness was checked against expected answers. Most A/Bs used 6–8 tasks × 2
  repetitions, so differences under ~5% are noise.
- Cost was compared in relative units: plain input 1, cache write 1.25, cache read 0.1, output 5.

## Where the tokens go

A 7-day audit of Claude Code usage logs on that machine (about 14k requests) found:

- **Re-reading dominates.** 99% of input tokens were cache reads; weighted by price, cache reads were about
  three quarters of the cost and cache writes most of the rest. Output was negligible.
- **Context is large.** The average request carried ~450k tokens of context, because sessions run for days
  and grow toward the model's window before compacting.
- **The fixed base is not small.** A fresh session started at roughly 50k tokens (tool and MCP definitions,
  instructions, skill listing, memory index) and that base is re-sent on every request.

So the levers are: a smaller base, less history per request, and fewer requests. Saving output or
"thinking" does not move the bill.

## Studies

### 1. Tool routing (the gateway's original job)

Jev predicts the next tool from the conversation, then the gateway steers the agent: a hint for Claude Code
(so thinking and prompt caching keep working), a forced `tool_choice` for Codex.

- **Claude Code, hint mode:** cost 1.04× of the control, 16/16 correct in both arms. No saving. A hint
  cannot shorten anything; at best it saves a wrong turn.
- **Codex, forced mode:** the first run looked like −25% input, but 4 of 16 answers broke. The cause was
  forcing `tool_choice: none` on the first turn, so the agent claimed it could not see the workspace. After
  fixing that (never force `none` before the conversation has used a tool), the rerun gave input 0.90×,
  turns 0.87×, with 16/16 correct (control 15/16). That is a real but small gain.

### 2. Tool search behind a proxy (no Jev involved)

Claude Code turns **tool search** off whenever `ANTHROPIC_BASE_URL` points at a proxy, so every tool and MCP
definition goes into every request. Setting `ENABLE_TOOL_SEARCH=true` brought it back. On a session with one
MCP server, the first request fell from 59 tools / 201k characters to 13 tools / 129k characters, and the
first turn of a short task from ~95k to ~50k tokens. A deferred tool loaded on demand did not break the cache.

**This was the largest single cut we found, and it was a configuration flag.** `jev claude` sets it unless
you set it yourself.

### 3. Pruning old tool results

The gateway replaced tool results that Jev judged no longer needed with a short placeholder. It stored the
cut ids and re-applied them on every later turn, so the cached prefix stayed stable.

- **Round 1, one yes/no per result ("still needed?"):** Jev's probabilities clustered around 0.5 (median
  0.49). Only 1 cut in 29 questions. No effect.
- **Round 2, relative ranking plus a deterministic rule:** Jev picked "least needed" among the candidates,
  in both option orders, with a `keep_all` escape. A rule also cut results whose exact call repeated later.
  On synthetic tasks: cost 0.83×, 12/12 correct. The tasks favoured pruning, though.
- **Real sessions:** long writing sessions were mostly base, edits and text. A ~210k-token session had only
  ~6k tokens of cuttable tool output. Cuts made only after the cache had gone cold (>1 h idle) were safe but
  showed no measurable saving.

Not shipped.

### 4. Context diet: skill listing and memory index

Claude Code repeats its skill listing and the auto-memory index on every request. Once per conversation, on
its first request, the gateway asked Jev how likely each entry was to matter. The context was the project
instructions and the first prompt. Unlikely entries shrank to their name. The decision was stored and
replayed on every turn, so the cache stayed valid.

| Arm | Cost vs control | Correct | Turns |
|---|---|---|---|
| Jev scores, keeps the top 10 skills / 8 memory lines | 0.64 | 16/16 | 25 |
| Trim everything, no Jev | 0.67 | 16/16 | 30 |
| Control | 1.00 | 16/16 | 25 |

- **Most of the gain comes from trimming, not from choosing.** Jev's absolute scores fell below the floor
  for almost every entry. What it added was its *ranking*: the kept top-k saved a few file look-ups (25 vs
  30 turns).
- **Short `-p` tasks overstate this.** There the first cache write dominates; in long sessions the saving is
  a fixed ~12k tokens per turn.
- **A parser bug to avoid:** plugin skills are listed as `plugin:skill: description`. Splitting on the first
  `:` reduced them to the plugin name.

Shipped as `JEV_DIET`, off by default.

### 5. Topic-shift detection (suggest `/clear`)

This study was offline only. We extracted ~2,200 real prompts from transcripts, each with the session so
far, and asked Jev four narrow yes/no questions about the pair (session, new prompt):
- does it depend on the session?
- is it a reply to the last message?
- does it start a different task?
- could it be done without the session?

- The mean of the first three separated "safe to clear" from "keep" with **AUC 0.87**, measured against
  blind labels from a larger model. The fourth question was useless.
- At a 0.8 cut: 5% of "keep" cases flagged, half of the "clear" cases caught. On real prompts, about 1 in 55
  would trigger a hint.
- Roughly half of the strongest real flags were true topic changes. The rest were short replies such as
  "and?" or answers to a question.
- The upside is large, because sessions often sit at 500k+ tokens of context. But a hint only saves tokens if
  the user acts on it. Doing the clear automatically in the gateway is possible: store a cut point and replay
  it. It is also risky, because the user still sees history the model no longer has.

Not built.

## Lessons about asking Jev

These match TypeSafe's own cookbooks, which we read too late:

- **Ask narrow, checkable facts, not the whole decision.**
  - "Is this item still needed?" gives mushy probabilities near 0.5.
  - "Does the new request depend on a file or decision from the session?" separates well.
  - Combine several narrow answers in code.
- **Put the pair in the state.** Send the query and the item together, so every question is about their
  relation.
- **Rank, don't threshold at 0.5.** Use the probability to sort, then cut by rank or by a token budget. Tune
  any threshold per task on labelled data.
- **Give choices an escape.** Choice probabilities always sum to 1, so add "none of these" or a separate
  "does any fit?" question.
- **Repeating a question does not help.** Jev is close to deterministic. Use an abstain band, e.g. 0.3–0.7,
  instead.
- **Jev is cheap enough to ignore as a cost.** All of the above cost cents, and calls take ~100–700 ms.

## Conclusion

- The token savings that mattered came from plain mechanisms: turning tool search back on and trimming the
  repeated base. The biggest remaining lever is long session history, which no request-level trick touched.
- Jev was decisive only where a decision needs meaning:
  - which tool comes next: real, but small;
  - which listed entries to keep: marginal over "trim all";
  - whether the conversation changed topic: promising, but the saving depends on acting on it.

We removed the gateway with that result; it remains in git history (up to e1cd2ea) as a reference for proxying Claude Code and
Codex, cache-safe request rewriting, and using a calibrated classifier inside an agent loop.
