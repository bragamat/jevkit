# jev triage

Ask the same typed questions about every item of a list, and read only the result table.

```sh
jev triage ITEMS SPEC [--label field] [--sort question_id] [--context FILE [--batch 20]] [--json]
```

## Items

`ITEMS` is a JSON array or JSONL. Each item can be a string or an object. `--label field` names
each output row by that field of the item; without it the row starts with the item itself, cut to
80 characters.

## Spec

`SPEC` is a JSON object of questions, keyed by an id you choose:

```json
{
  "urgent": {"type": "noul", "instructions": "Does `item` need action today?"},
  "area":   {"type": "choice", "instructions": "Which team owns `item`?", "criteria": ["billing", "infra", "docs"]},
  "effort": {"type": "score", "instructions": "How much work is `item`?", "criteria": ["small", "medium", "large"]}
}
```

- `noul`: probability of yes, 0 to 1.
- `choice`: one of `criteria`. It can also be an object of option → description, which is more
  accurate when options are close.
- `score`: a position on the ordered levels in `criteria`, lowest first.

Refer to the item as `` `item` `` in the instructions; the state sent for each item is
`{"item": ...}`. All the questions about one item go in a single request, which is cheaper and
does not change the answers.

Write the spec with a quoted heredoc, because it contains backticks:

```sh
cat > spec.json <<'EOF'
{"stale": {"type": "noul", "instructions": "Does `item` describe behavior the code no longer has?"}}
EOF
```

## Output

```console
$ jev triage tickets.jsonl spec.json --label id --sort urgent
T-104 | urgent=0.97 | area=infra | area_conf=0.91 | effort=1.2 | effort_conf=0.74
T-101 | urgent=0.12 | area=docs | area_conf=0.88 | effort=0.1 | effort_conf=0.93
```

`--sort id` orders rows by that question, highest first. Treat low `_conf` values the way you treat
`CONFIRM`: look at those items yourself or ask about them one at a time.

## Shared context in batches

When every item is judged against the same context (a case profile, a meeting summary, a style
guide), pass it with `--context FILE`. The context then goes once per batch of `--batch` items
(default 20) instead of once per item: the state becomes `{"context": ..., "items": [...]}` and each
`` `item` `` in the instructions is rewritten to `` `items[i]` ``, one question per item.

This cuts tokens a lot (in one measurement, 91 items cost 31k tokens instead of about 190k), but
fine-grained classification gets less accurate in batches. Use batch mode as a first pass and
rerun the low-confidence items on their own.
