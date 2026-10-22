"""Produce the litellm-1.83.9-mock-anthropic-*.json fixtures.

LiteLLM 1.83.9's own code builds every record: the Anthropic transformation,
the cost calculation and the standard_logging_object a callback receives
(the same object the proxy's generic_api callback posts). Only the upstream
HTTP response is mocked, so these are not captures from Anthropic itself.
The usage blocks follow Anthropic's documented shape (usage.cache_creation
with ephemeral_5m_input_tokens and ephemeral_1h_input_tokens).

    python3 -m venv venv && ./venv/bin/pip install litellm==1.83.9
    LITELLM_LOCAL_MODEL_COST_MAP=True ./venv/bin/python capture_mock_anthropic.py OUTDIR

Records written to OUTDIR, renamed into testdata as:
  split.json      -> cache-write-5m-1h: 1,000 fresh input, 40,000 5m writes, 60,000 1h writes, 500 output
  split5m.json    -> cache-write-5m: 1,000 fresh input, 40,000 5m writes, 500 output
  cache_hit.json  -> cache-hit: the second of two identical calls with LiteLLM's local response cache on
  failure.json    -> failure: a call the upstream refuses with HTTP 529
"""
import asyncio
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import litellm
from litellm.integrations.custom_logger import CustomLogger

OUT = sys.argv[1]
MODEL = "claude-sonnet-4-5-20250929"

USAGES = {
    "split": {"input_tokens": 1000, "cache_creation_input_tokens": 100000, "cache_read_input_tokens": 0,
              "cache_creation": {"ephemeral_5m_input_tokens": 40000, "ephemeral_1h_input_tokens": 60000},
              "output_tokens": 500},
    "split5m": {"input_tokens": 1000, "cache_creation_input_tokens": 40000, "cache_read_input_tokens": 0,
                "cache_creation": {"ephemeral_5m_input_tokens": 40000, "ephemeral_1h_input_tokens": 0},
                "output_tokens": 500},
    "plain": {"input_tokens": 1000, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
              "output_tokens": 500},
}


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        text = body["messages"][-1]["content"]
        if isinstance(text, list):
            text = text[0]["text"]
        if text == "fail":
            self.send_response(529)
            self.send_header("content-type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}).encode())
            return
        usage = USAGES[text]
        resp = {"id": "msg_" + text, "type": "message", "role": "assistant", "model": MODEL,
                "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                "stop_sequence": None, "usage": usage}
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(resp).encode())


records = []


class Capture(CustomLogger):
    async def async_log_success_event(self, kwargs, response_obj, start_time, end_time):
        records.append(kwargs["standard_logging_object"])

    async def async_log_failure_event(self, kwargs, response_obj, start_time, end_time):
        records.append(kwargs["standard_logging_object"])


async def call(port, text, **kw):
    try:
        await litellm.acompletion(model="anthropic/" + MODEL, api_base=f"http://127.0.0.1:{port}",
                                  api_key="sk-mock", messages=[{"role": "user", "content": text}],
                                  user="cus_acme_corp", metadata={"user_api_key_end_user_id": "cus_acme_corp"}, num_retries=0, max_retries=0, **kw)
    except Exception as e:  # the failure call
        print("call raised:", type(e).__name__)
    await asyncio.sleep(1.5)
    return records.pop()


async def main():
    srv = HTTPServer(("127.0.0.1", 0), Upstream)
    port = srv.server_address[1]
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    litellm.callbacks = [Capture()]

    out = {}
    out["split"] = await call(port, "split")
    out["split5m"] = await call(port, "split5m")
    out["failure"] = await call(port, "fail")
    litellm.cache = litellm.Cache(type="local")
    await call(port, "plain", caching=True)
    out["cache_hit"] = await call(port, "plain", caching=True)
    for name, rec in out.items():
        with open(f"{OUT}/{name}.json", "w") as f:
            json.dump(rec, f, indent=1, sort_keys=True, default=str)
        print(name, rec.get("status"), rec.get("cache_hit"), rec.get("id"), rec.get("prompt_tokens"), rec.get("response_cost"))


asyncio.run(main())
