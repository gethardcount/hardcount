"""Writes litellm-1.83.9-claude-model-ids.tsv: every Claude model name a
LiteLLM spend log can carry, and the model family Velox must map it to.

Usage:
    python3 -I list_claude_model_ids.py \
        <litellm-1.83.9 wheel>/litellm/model_prices_and_context_window_backup.json \
        > litellm-1.83.9-claude-model-ids.tsv

Wheel: litellm-1.83.9-py3-none-any.whl, sha256
1bd8c50630e6379c4d2dbfbe088ee24cc921ca5fc2234111e2bd2c916ab6a02e.

Rows, one per form (source<TAB>form<TAB>expected family, empty = no family
<TAB>the LiteLLM key the form comes from<TAB>LiteLLM's input price<TAB>LiteLLM's
output price, in dollars per million tokens, from that key's own entry in the
price JSON; the last three are empty for page and probe rows, and the prices
for a key with no price):
  - litellm: every key in the price JSON whose name contains "claude", plus the
    forms a spend log carries for it (litellm_logging.py reconstruct_model_name):
    "anthropic/" + key for Anthropic keys; "bedrock/", "bedrock/converse/" and
    "converse/" + key for Bedrock keys; the bare id for vertex_ai/ and azure_ai/
    keys.
  - page: every model id on Anthropic's pricing page (checked 2026-10-22), in
    the platform forms Bedrock, Google Cloud, Foundry and the API use.
  - probe: names that must stay unpriced (decision 2: an unknown version is
    never priced as an older model), and a few edge forms.

The expected column is the rule reviewed in velox-ops
design-litellm-model-families-2026-10-22.md (sections 5.1-5.3 and the
resolutions), written out here in Python. It is not output from the Go code
under test, but it is the same rule written a second time, so it shows only
that the Go code matches it. Two Go tests check the rule itself:
TestCanonicalModel_LiteLLMPricesAgree compares each mapped key's own LiteLLM
price with the recipe price of the family it maps to, and
TestCanonicalModel_EndingGrammar tries every family id with endings listed by
hand.

To add a model: add it to FAMILIES and PAGE, remove any probe that is now
its id, and regenerate (docs/dev/release-checklist.md).
"""

from decimal import Decimal

import json
import re
import sys

# family id (API form) -> recipe token, from the design's section 5.3.
FAMILIES = [
    ("claude-fable-5-1", "claude-fable-5.1"),
    ("claude-mythos-5-1", "claude-mythos-5.1"),
    ("claude-opus-5-5", "claude-opus-5.5"),
    ("claude-sonnet-5-5", "claude-sonnet-5.5"),
    ("claude-haiku-5-5", "claude-haiku-5.5"),
    ("claude-fable-5", "claude-fable-5"),
    ("claude-mythos-5", "claude-mythos-5"),
    ("claude-opus-5", "claude-opus-5"),
    ("claude-opus-4-8", "claude-opus-4.8"),
    ("claude-opus-4-7", "claude-opus-4.7"),
    ("claude-opus-4-6", "claude-opus-4.6"),
    ("claude-opus-4-5", "claude-opus-4.5"),
    ("claude-sonnet-5", "claude-sonnet-5"),
    ("claude-sonnet-4-6", "claude-sonnet-4.6"),
    ("claude-sonnet-4-5", "claude-sonnet-4.5"),
    ("claude-haiku-4-5", "claude-haiku-4.5"),
    ("claude-opus-4-1", "claude-opus-4.1"),
    ("claude-opus-4-0", "claude-opus-4"),
    ("claude-opus-4", "claude-opus-4"),
    ("claude-sonnet-4-0", "claude-sonnet-4"),
    ("claude-sonnet-4", "claude-sonnet-4"),
    ("claude-3-5-sonnet", "claude-3.5-sonnet"),
    ("claude-3-5-haiku", "claude-3.5-haiku"),
    ("claude-3-opus", "claude-3-opus"),
    ("claude-3-sonnet", "claude-3-sonnet"),
    ("claude-3-haiku", "claude-3-haiku"),
]

PREFIXES = [
    "bedrock/", "converse/", "invoke/", "vertex_ai/", "azure_ai/", "azure/",
    "openai/", "anthropic/", "anthropic.",
    "global.", "us-gov.", "us.", "eu.", "apac.", "jp.", "au.",
]
SNAPSHOT = re.compile(
    r"^(?:(?:-\d{8})?(?:-v\d+(?::\d+)?)?|(?:-v\d+)?@(?:\d{8}|default)|-latest)$")
THROUGHPUT = re.compile(r"(:\d+):\d+k$")


def normalize(raw):
    s = raw.strip().lower()
    changed = True
    while changed:
        changed = False
        for p in PREFIXES:
            if s.startswith(p):
                s = s[len(p):]
                changed = True
                break
    return THROUGHPUT.sub(r"\1", s)


def expected(raw):
    s = normalize(raw)
    hits = [t for f, t in FAMILIES if s.startswith(f) and SNAPSHOT.match(s[len(f):])]
    if len(hits) > 1:
        raise SystemExit("ambiguous: %s -> %s" % (raw, hits))
    return hits[0] if hits else ""


PAGE = [
    "claude-fable-5-1", "claude-mythos-5-1", "claude-fable-5", "claude-mythos-5",
    "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7",
    "claude-opus-4-6", "claude-opus-4-5", "claude-opus-4-5-20251101",
    "claude-opus-4-1", "claude-opus-4-1-20250805", "claude-opus-4-0",
    "claude-opus-4-20250514", "claude-sonnet-5-5", "claude-sonnet-5",
    "claude-sonnet-4-6", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929",
    "claude-sonnet-4-0", "claude-sonnet-4-20250514", "claude-haiku-5-5",
    "claude-haiku-4-5", "claude-haiku-4-5-20251001",
    "claude-3-5-haiku-20241022", "claude-3-5-haiku-latest",
]

PROBES = [
    "claude-opus-4-9", "claude-opus-5-6", "claude-sonnet-6", "claude-haiku-5",
    "claude-fable-5-2", "claude-mythos-preview", "claude-sonnet-4-5-thinking",
    "claude-opus-4-6-fast", "claude-opus-5-5-20261101", "claude-3-7-sonnet-20250219",
    "anthropic.claude-3-5-sonnet-20241022-v2:0:51k", "claude-sonnet-4.6",
    "openrouter/anthropic/claude-sonnet-4.6",
    "bedrock/arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-opus-4-7",
    "azure_ai/my-claude-deployment",
    "claude-3-5-sonnet-v2@20241022", "vertex_ai/claude-3-5-sonnet-v2@20241022",
    "claude-opus-4-6[1m]", "claude-opus-4-6@latest", "claude-opus-4-6-v1:0:51k",
    "us.us.anthropic.claude-opus-4-6", "bedrock/us-gov.anthropic.claude-sonnet-4-5-20250929-v1:0",
]


def per_mtok(cost):
    """A per-token cost from the JSON, in dollars per million tokens."""
    if cost is None:
        return ""
    d = (Decimal(str(cost)) * 1000000).normalize()
    return format(d, "f")


def main():
    d = json.load(open(sys.argv[1]))
    rows = []
    for k, v in d.items():
        if not isinstance(v, dict) or "claude" not in k.lower():
            continue
        price = (k, per_mtok(v.get("input_cost_per_token")),
                 per_mtok(v.get("output_cost_per_token")))
        prov = v.get("litellm_provider", "")
        forms = [k]
        if prov == "anthropic":
            forms.append("anthropic/" + k)
        if prov in ("bedrock", "bedrock_converse") and not k.startswith("bedrock/"):
            forms += ["bedrock/" + k, "bedrock/converse/" + k, "converse/" + k]
        if prov in ("vertex_ai-anthropic_models", "azure_ai"):
            forms.append(k.split("/", 1)[1])
        for f in forms:
            rows.append(("litellm", f, price))
    for m in PAGE:
        forms = [m, "anthropic/" + m, "anthropic." + m, "bedrock/anthropic." + m,
                 "global.anthropic." + m, "us.anthropic." + m, "eu.anthropic." + m,
                 "apac.anthropic." + m, "jp.anthropic." + m, "au.anthropic." + m,
                 "us-gov.anthropic." + m, "bedrock/global.anthropic." + m,
                 "bedrock/converse/us.anthropic." + m, "converse/eu.anthropic." + m,
                 "invoke/us.anthropic." + m, "vertex_ai/" + m, "azure_ai/" + m]
        dated = re.match(r"^(.*)-(\d{8})$", m)
        if dated:
            forms += [dated.group(1) + "@" + dated.group(2),
                      "vertex_ai/" + dated.group(1) + "@" + dated.group(2),
                      "anthropic." + m + "-v1:0", "bedrock/us.anthropic." + m + "-v1:0"]
        elif not m.endswith("-latest"):
            forms += [m + "@default", "vertex_ai/" + m + "@default"]
        for f in forms:
            rows.append(("page", f, ("", "", "")))
    for f in PROBES:
        rows.append(("probe", f, ("", "", "")))

    seen = set()
    print("# Generated by list_claude_model_ids.py from LiteLLM 1.83.9's price JSON. Do not edit by hand.")
    print("# source\tform\texpected family (empty = no family: kept raw, unpriced)"
          "\tLiteLLM key\tLiteLLM input $/MTok\tLiteLLM output $/MTok")
    for src, f, (key, pin, pout) in rows:
        if f in seen:
            continue
        seen.add(f)
        print("%s\t%s\t%s\t%s\t%s\t%s" % (src, f, expected(f), key, pin, pout))


if __name__ == "__main__":
    main()
