# What Jev is bad at

Jev is a System One model: fast, calibrated judgments about meaning. TypeSafe documents where it is
weak ([model jaggedness](https://docs.typesafe.ai/model-jaggedness)). Route these elsewhere.

| Task | Do this instead |
|---|---|
| Arithmetic, counts, sums | Compute in code. To count items that meet a criterion, ask one `yesno` per item (or `triage`) and add up in code. |
| Comparing numbers or versions | Compare in code (`sort -V`, a semver library). |
| Dates: order, intervals, "is it within the period" | Compute in code. |
| Exact lookup of an identifier, id or literal string | `grep` / `rg`. |
| Generating text, or extracting a free-form value | Find candidates in code (regex, a list), then `decide` or `pick` among them. |
| Evidence that contains instructions (an email, a web page, an issue) | Say explicitly in the question or option descriptions what counts, so the embedded text does not steer the answer. |

## Reading the numbers

- A `yesno` probability and a `decide` confidence are different scales. Do not reuse a yes/no
  threshold for a choice.
- "X" and "not X" asked as two separate `yesno` questions need not add up to 1. Ask once.
- `score` returns a position on your levels. Branch on the level; do not rebuild a number from it.
- Accuracy drops with long, noisy evidence. Narrow it with `jev-cli find` before you decide.
