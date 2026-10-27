"""List every call_type string a LiteLLM record can carry, from the wheel's
source read as text (nothing is imported or run).

    python3 list_call_types.py PATH/TO/site-packages/litellm > found.txt

Sources, each printed with the first place it was found:
  enum       values of the CallTypes enum (types/utils.py)
  literal    values of CallTypesLiteral (types/utils.py)
  route      values of the proxy's route_type Literal
             (proxy/common_request_processing.py)
  client     names of functions decorated with @client, at any indent (the
             wrapper sets call_type to the function name)
  json       the sync and async names of the container-file endpoints in
             containers/endpoints.json: the proxy passes the async name as
             the route type, and the SDK registers both names
  setup      function_setup(original_function="...") literals
  keyword    call_type="..." keyword literals (Logging(...),
             update_environment_variables(...), kwargs dicts)

testdata/litellm-1.83.9-call-types.tsv holds these strings with Velox's
verdict for each. TestCallTypeInventory fails when the two disagree, so a
LiteLLM upgrade shows its new call types until each is classified.
"""
import json
import os
import re
import sys

ROOT = sys.argv[1]
found = {}


def add(value, source, where):
    found.setdefault(value, (source, where))


def literal_block(text, start_pattern):
    m = re.search(start_pattern, text)
    if not m:
        return []
    depth, i = 1, m.end()
    while depth and i < len(text):
        depth += {"[": 1, "]": -1}.get(text[i], 0)
        i += 1
    return re.findall(r'"([A-Za-z_][A-Za-z0-9_.]*)"', text[m.end():i])


def read(rel):
    with open(os.path.join(ROOT, rel), encoding="utf-8") as f:
        return f.read()


types_utils = read("types/utils.py")
enum = re.search(r"class CallTypes\(str, Enum\):\n((?:[ \t]+.*\n|\n)+)", types_utils)
for value in re.findall(r'^\s+\w+\s*=\s*"([^"]+)"', enum.group(1), re.M):
    add(value, "enum", "types/utils.py")
for value in literal_block(types_utils, r"CallTypesLiteral = Literal\["):
    add(value, "literal", "types/utils.py")
for value in literal_block(read("proxy/common_request_processing.py"), r"route_type: Literal\["):
    add(value, "route", "proxy/common_request_processing.py")

client_re = re.compile(r"^[ \t]*@client\s*\n(?:[ \t]*@.*\n)*[ \t]*(?:async\s+)?def\s+(\w+)", re.M)
setup_re = re.compile(r'original_function\s*=\s*"([A-Za-z_]\w*)"')
keyword_re = re.compile(r'\bcall_type\s*=\s*"([A-Za-z_]\w*)"')
for endpoint in json.loads(read("containers/endpoints.json"))["endpoints"]:
    add(endpoint["name"], "json", "containers/endpoints.json")
    add(endpoint["async_name"], "json", "containers/endpoints.json")

for dirpath, _, files in os.walk(ROOT):
    for name in sorted(files):
        if not name.endswith(".py"):
            continue
        path = os.path.join(dirpath, name)
        rel = os.path.relpath(path, ROOT)
        with open(path, encoding="utf-8") as f:
            text = f.read()
        for value in client_re.findall(text):
            add(value, "client", rel)
        for value in setup_re.findall(text):
            add(value, "setup", rel)
        for value in keyword_re.findall(text):
            add(value, "keyword", rel)

for value in sorted(found):
    source, where = found[value]
    print("%s\t%s\t%s" % (value, source, where))
