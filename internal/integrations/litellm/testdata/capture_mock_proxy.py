"""Produce the litellm-1.83.9-proxy-*.json fixtures.

LiteLLM 1.83.9's own proxy builds every record: the route type that becomes
the record's call_type, the end user from the request headers, the
provider transformations, and the standard_logging_object that the
generic_api callback posts. Only the upstream HTTP responses are mocked, so
these are not captures from the providers. The upstream bodies follow each
provider's documented shape.

    python3 -m venv venv && ./venv/bin/pip install 'litellm[proxy]==1.83.9'
    LITELLM_LOCAL_MODEL_COST_MAP=True ./venv/bin/python capture_mock_proxy.py OUTDIR VARIANT

VARIANT is one of:
  default   every fixture, with the proxy configured as
            docs/integrations/litellm.md step 2 says
  postcall  only the bridged /v1/responses runs (F7), with a post_call
            guardrail configured (the proxy defers async logging)
  loghook   only F7, with a custom logging callback whose
            async_logging_hook awaits (it yields before LiteLLM's
            one-record guard is set)

OUTDIR gets one JSON file per request (every record the callback posted for
it) and summary.json (records and distinct ids per request, the callback
batches, and whether the batch answered 503 was sent again).
"""
import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

OUT = sys.argv[1]
VARIANT = sys.argv[2] if len(sys.argv) > 2 else "default"
PROXY, MOCK, CAPTURE = 18100, 18101, 18102
MASTER_KEY = "sk-capture"
ANTHROPIC_MODEL = "claude-sonnet-4-5-20250929"

# Anthropic usage per keyword. SPLIT: 1,000 fresh input, 40,000 5m writes,
# 60,000 1h writes, 5,000 cache reads, 500 output.
SPLIT_START = {"input_tokens": 1000, "cache_creation_input_tokens": 100000, "cache_read_input_tokens": 5000,
               "cache_creation": {"ephemeral_5m_input_tokens": 40000, "ephemeral_1h_input_tokens": 60000},
               "output_tokens": 1}
SPLIT_FULL = dict(SPLIT_START, output_tokens=500)
PLAIN = {"input_tokens": 1000, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 500}
# message_delta.usage shapes. Anthropic's streaming examples show
# {"output_tokens": N}; the cumulative shapes cover a delta that repeats the
# input-side counts, with and without the cache_creation split.
DELTAS = {
    "KW_DOUT": {"output_tokens": 500},
    "KW_DCUM": {"input_tokens": 1000, "cache_creation_input_tokens": 100000, "cache_read_input_tokens": 5000,
                "output_tokens": 500},
    "KW_DCUMSPLIT": dict(SPLIT_FULL),
}
# OpenAI Responses usage: 2,000 input of which 1,500 cached, 300 output of
# which 200 reasoning.
RESP_USAGE = {"input_tokens": 2000, "input_tokens_details": {"cached_tokens": 1500},
              "output_tokens": 300, "output_tokens_details": {"reasoning_tokens": 200}, "total_tokens": 2300}
# Gemini usage: 2,000 prompt of which 1,500 cached, 200 candidates, 100 thoughts.
GEMINI_USAGE = {"promptTokenCount": 2000, "cachedContentTokenCount": 1500, "candidatesTokenCount": 200,
                "thoughtsTokenCount": 100, "totalTokenCount": 2300}
# gpt-image-1 usage (image tokens, priced unlike text).
IMAGE_USAGE = {"total_tokens": 4210, "input_tokens": 50, "output_tokens": 4160,
               "input_tokens_details": {"text_tokens": 50, "image_tokens": 0}}

upstream_log = []


def keyword(raw):
    m = re.search(r"KW_[A-Z]+", raw)
    return m.group(0) if m else ""


class Upstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def _sse(self, events):
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("transfer-encoding", "chunked")
        self.end_headers()
        for name, data in events:
            chunk = ""
            if name:
                chunk += "event: %s\n" % name
            chunk += "data: %s\n\n" % json.dumps(data)
            b = chunk.encode()
            self.wfile.write(b"%x\r\n%s\r\n" % (len(b), b))
            self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n")

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        raw = self.rfile.read(n).decode() if n else "{}"
        body = json.loads(raw or "{}")
        kw = keyword(raw)
        upstream_log.append({"path": self.path, "kw": kw, "stream": bool(body.get("stream"))})
        p = self.path.split("?")[0]
        if p.endswith("/messages/count_tokens"):
            return self._json(200, {"input_tokens": 1234})
        if p.endswith("/v1/messages"):
            return self.anthropic(body, kw)
        if p.endswith("/responses"):
            return self.openai_responses(body, kw)
        if ":generateContent" in p or ":streamGenerateContent" in p:
            return self.gemini(":streamGenerateContent" in p)
        if p.endswith("/embeddings"):
            return self._json(200, {"object": "list", "model": "text-embedding-3-small",
                                    "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2]}],
                                    "usage": {"prompt_tokens": 8, "total_tokens": 8}})
        if p.endswith("/images/generations"):
            return self._json(200, {"created": int(time.time()), "data": [{"b64_json": "aGVsbG8="}],
                                    "usage": IMAGE_USAGE})
        return self._json(404, {"error": "mock: no route for " + p})

    def anthropic(self, body, kw):
        if kw == "KW_FAIL":
            return self._json(529, {"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}})
        mid = "msg_" + uuid.uuid4().hex[:24]
        if not body.get("stream"):
            usage = SPLIT_FULL if kw == "KW_SPLIT" else PLAIN
            return self._json(200, {"id": mid, "type": "message", "role": "assistant", "model": ANTHROPIC_MODEL,
                                    "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                                    "stop_sequence": None, "usage": usage})
        start = SPLIT_START if kw in DELTAS or kw == "KW_SPLIT" else dict(PLAIN, output_tokens=1)
        delta = DELTAS.get(kw, {"output_tokens": 500})
        self._sse([
            ("message_start", {"type": "message_start", "message": {
                "id": mid, "type": "message", "role": "assistant", "model": ANTHROPIC_MODEL, "content": [],
                "stop_reason": None, "stop_sequence": None, "usage": start}}),
            ("content_block_start", {"type": "content_block_start", "index": 0,
                                     "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                     "delta": {"type": "text_delta", "text": "ok"}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                               "usage": delta}),
            ("message_stop", {"type": "message_stop"}),
        ])

    def openai_responses(self, body, kw):
        rid = "resp_" + uuid.uuid4().hex
        status = "incomplete" if kw == "KW_INCOMPLETE" else "completed"
        resp = {"id": rid, "object": "response", "created_at": int(time.time()), "status": status,
                "model": body.get("model", "gpt-5.4"),
                "output": [{"type": "message", "id": "msg_" + uuid.uuid4().hex[:12], "status": "completed",
                            "role": "assistant",
                            "content": [{"type": "output_text", "text": "ok", "annotations": []}]}],
                "usage": RESP_USAGE}
        if status == "incomplete":
            resp["incomplete_details"] = {"reason": "max_output_tokens"}
        if not body.get("stream"):
            return self._json(200, resp)
        created = dict(resp, status="in_progress", output=[], usage=None)
        created.pop("incomplete_details", None)
        self._sse([
            ("response.created", {"type": "response.created", "sequence_number": 0, "response": created}),
            ("response.output_text.delta", {"type": "response.output_text.delta", "sequence_number": 1,
                                            "item_id": resp["output"][0]["id"], "output_index": 0,
                                            "content_index": 0, "delta": "ok"}),
            ("response." + status, {"type": "response." + status, "sequence_number": 2, "response": resp}),
        ])

    def gemini(self, stream):
        chunk = {"candidates": [{"content": {"role": "model", "parts": [{"text": "ok"}]}, "finishReason": "STOP",
                                 "index": 0}],
                 "usageMetadata": GEMINI_USAGE, "modelVersion": "gemini-2.5-flash",
                 "responseId": "gemresp_" + uuid.uuid4().hex[:16]}
        if not stream:
            return self._json(200, chunk)
        first = {"candidates": [{"content": {"role": "model", "parts": [{"text": "o"}]}, "index": 0}],
                 "modelVersion": "gemini-2.5-flash", "responseId": chunk["responseId"]}
        self._sse([(None, first), (None, chunk)])


batches = []


class Capture(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        data = json.loads(self.rfile.read(n))
        records = data if isinstance(data, list) else data.get("events", [data])
        first = not batches
        batches.append({"answer": 503 if first else 200, "records": records})
        self.send_response(503 if first else 200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")


def serve(cls, port):
    s = ThreadingHTTPServer(("127.0.0.1", port), cls)
    threading.Thread(target=s.serve_forever, daemon=True).start()


def write_config(d):
    callbacks = ["hardcount"]
    extra = ""
    if VARIANT == "loghook":
        with open(os.path.join(d, "slow_hook.py"), "w") as f:
            f.write("import asyncio\n"
                    "from litellm.integrations.custom_logger import CustomLogger\n\n\n"
                    "class SlowHook(CustomLogger):\n"
                    "    async def async_logging_hook(self, kwargs, result, call_type):\n"
                    "        await asyncio.sleep(0.05)\n"
                    "        return kwargs, result\n\n\n"
                    "slow_hook_instance = SlowHook()\n")
        callbacks.append("slow_hook.slow_hook_instance")
    if VARIANT == "postcall":
        with open(os.path.join(d, "custom_guard.py"), "w") as f:
            f.write("from litellm.integrations.custom_guardrail import CustomGuardrail\n\n\n"
                    "class PostCallGuard(CustomGuardrail):\n"
                    "    async def async_post_call_success_hook(self, data, user_api_key_dict, response):\n"
                    "        return response\n")
        extra = ("guardrails:\n"
                 "  - guardrail_name: capture-post-call\n"
                 "    litellm_params:\n"
                 "      guardrail: custom_guard.PostCallGuard\n"
                 "      mode: post_call\n"
                 "      default_on: true\n")
    mock = "http://127.0.0.1:%d" % MOCK
    cfg = """model_list:
  - model_name: claude-sonnet-4-5
    litellm_params:
      model: anthropic/%(am)s
      api_base: %(mock)s
      api_key: sk-ant-mock
  - model_name: gpt-5.4
    litellm_params:
      model: openai/gpt-5.4
      api_base: %(mock)s/v1
      api_key: sk-mock
  - model_name: gemini-2.5-flash
    litellm_params:
      model: gemini/gemini-2.5-flash
      api_base: %(mock)s/v1beta
      api_key: gemini-mock
  - model_name: text-embedding-3-small
    litellm_params:
      model: openai/text-embedding-3-small
      api_base: %(mock)s/v1
      api_key: sk-mock
  - model_name: gpt-image-1
    litellm_params:
      model: openai/gpt-image-1
      api_base: %(mock)s/v1
      api_key: sk-mock
litellm_settings:
  callbacks: %(cbs)s
callback_settings:
  hardcount:
    callback_type: generic_api
    endpoint: "http://127.0.0.1:%(cap)d/v1/integrations/litellm/spend"
    headers:
      Authorization: Bearer hc_secret_test_capture
    max_retries: 4
    retry_delay: 1
    timeout: 10
general_settings:
  master_key: %(mk)s
%(extra)s""" % {"am": ANTHROPIC_MODEL, "mock": mock, "cbs": json.dumps(callbacks), "cap": CAPTURE,
               "mk": MASTER_KEY, "extra": extra}
    path = os.path.join(d, "config.yaml")
    with open(path, "w") as f:
        f.write(cfg)
    return path


def call(name, path, body, headers=None, auth="bearer"):
    h = {"content-type": "application/json"}
    if auth == "bearer":
        h["authorization"] = "Bearer " + MASTER_KEY
    else:
        h["x-api-key"] = MASTER_KEY
    h.update(headers or {})
    req = urllib.request.Request("http://127.0.0.1:%d%s" % (PROXY, path), data=json.dumps(body).encode(),
                                 headers=h, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            r.read()
            code = r.status
    except urllib.error.HTTPError as e:
        e.read()
        code = e.code
    print("%-34s %s -> %d" % (name, path, code), flush=True)
    return code


def msgs(kw):
    return [{"role": "user", "content": kw}]


def main():
    os.makedirs(OUT, exist_ok=True)
    serve(Upstream, MOCK)
    serve(Capture, CAPTURE)
    cfgdir = os.path.join(OUT, "cfg-" + VARIANT)
    os.makedirs(cfgdir, exist_ok=True)
    cfg = write_config(cfgdir)
    env = dict(os.environ, LITELLM_LOCAL_MODEL_COST_MAP="True", ANTHROPIC_API_BASE="http://127.0.0.1:%d" % MOCK,
               ANTHROPIC_API_KEY="sk-ant-mock", DEFAULT_FLUSH_INTERVAL_SECONDS="2", PYTHONPATH=cfgdir)
    log = open(os.path.join(OUT, "proxy-%s.log" % VARIANT), "w")
    proxy = subprocess.Popen([os.path.join(os.path.dirname(sys.executable), "litellm"), "--config", cfg,
                              "--port", str(PROXY), "--host", "127.0.0.1"], cwd=cfgdir, env=env, stdout=log,
                             stderr=subprocess.STDOUT)
    try:
        for _ in range(120):
            try:
                urllib.request.urlopen("http://127.0.0.1:%d/health/liveliness" % PROXY, timeout=1).read()
                break
            except Exception:
                time.sleep(1)
        else:
            raise SystemExit("proxy did not start; see proxy-%s.log" % VARIANT)

        runs = []  # (name, customer header)

        def run(name, path, body, extra_headers=None, auth="bearer", customer=True):
            h = dict(extra_headers or {})
            cust = None
            if customer:
                cust = "cus_" + name
                h.setdefault("x-litellm-customer-id", cust)
            # The S1 check needs a first batch that holds only this marker
            # call, so it is sent alone and the script waits for its flush.
            runs.append((name, cust, call(name, path, body, h, auth)))
            time.sleep(0.2)

        if VARIANT == "default":
            run("s1-marker", "/v1/chat/completions",
                {"model": "claude-sonnet-4-5", "messages": msgs("KW_PLAIN")})
            time.sleep(5)
            # F1 + F12: Claude Code-shaped body, customer in the header.
            run("f1-messages-split", "/v1/messages",
                {"model": "claude-sonnet-4-5", "max_tokens": 100, "messages": msgs("KW_SPLIT"),
                 "metadata": {"user_id": "user_claude-code-session-0f3a"}})
            # R6: the same body without the header.
            run("f1b-messages-no-customer-header", "/v1/messages",
                {"model": "claude-sonnet-4-5", "max_tokens": 100, "messages": msgs("KW_SPLIT"),
                 "metadata": {"user_id": "user_claude-code-session-0f3a"}}, customer=False)
            # F2/F3: streamed, three message_delta usage shapes.
            for kw, name in (("KW_DOUT", "f2-messages-stream-delta-output-only"),
                             ("KW_DCUM", "f3-messages-stream-delta-cumulative"),
                             ("KW_DCUMSPLIT", "f3b-messages-stream-delta-cumulative-split")):
                run(name, "/v1/messages", {"model": "claude-sonnet-4-5", "max_tokens": 100, "stream": True,
                                           "messages": msgs(kw)})
            # R1: two streamed calls with the same x-litellm-call-id.
            for i in (1, 2):
                run("r1-same-call-id-%d" % i, "/v1/messages",
                    {"model": "claude-sonnet-4-5", "max_tokens": 100, "stream": True, "messages": msgs("KW_DOUT")},
                    {"x-litellm-call-id": "client-chosen-id-1"})
            # R1 on the call types billed before this change.
            for i in (1, 2):
                run("r1-chat-same-call-id-%d" % i, "/v1/chat/completions",
                    {"model": "claude-sonnet-4-5", "messages": msgs("KW_PLAIN")},
                    {"x-litellm-call-id": "client-chosen-id-2"})
                run("r1-chat-stream-same-call-id-%d" % i, "/v1/chat/completions",
                    {"model": "claude-sonnet-4-5", "messages": msgs("KW_PLAIN"), "stream": True},
                    {"x-litellm-call-id": "client-chosen-id-3"})
                run("r1-embedding-same-call-id-%d" % i, "/v1/embeddings",
                    {"model": "text-embedding-3-small", "input": "KW_EMB"},
                    {"x-litellm-call-id": "client-chosen-id-4"})
            run("f4-messages-failure", "/v1/messages",
                {"model": "claude-sonnet-4-5", "max_tokens": 100, "messages": msgs("KW_FAIL")})
            run("f5-responses", "/v1/responses", {"model": "gpt-5.4", "input": "KW_RESP"})
            run("f6-responses-stream", "/v1/responses", {"model": "gpt-5.4", "input": "KW_RESP", "stream": True})
            run("f6b-responses-stream-incomplete", "/v1/responses",
                {"model": "gpt-5.4", "input": "KW_INCOMPLETE", "stream": True})
            # R9: /v1/messages to an OpenAI model (Messages-to-Responses adapter).
            run("r9-messages-to-openai", "/v1/messages",
                {"model": "gpt-5.4", "max_tokens": 100, "messages": msgs("KW_RESP")})
            gem = {"contents": [{"role": "user", "parts": [{"text": "KW_GEM"}]}]}
            run("f8-generate-content", "/v1beta/models/gemini-2.5-flash:generateContent", gem)
            run("f8b-generate-content-stream", "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", gem)
            # F9: Anthropic pass-through, not streamed and streamed; count_tokens.
            run("f9-passthrough-messages", "/anthropic/v1/messages",
                {"model": ANTHROPIC_MODEL, "max_tokens": 100, "messages": msgs("KW_SPLIT")}, auth="x-api-key")
            run("f9b-passthrough-messages-stream", "/anthropic/v1/messages",
                {"model": ANTHROPIC_MODEL, "max_tokens": 100, "stream": True, "messages": msgs("KW_DOUT")},
                auth="x-api-key")
            run("f9c-passthrough-count-tokens", "/anthropic/v1/messages/count_tokens",
                {"model": ANTHROPIC_MODEL, "messages": msgs("KW_PLAIN")}, auth="x-api-key")
            run("f10-image-generation", "/v1/images/generations", {"model": "gpt-image-1", "prompt": "a cat"})
            # F11: Gemini route to an Anthropic model (adapter), both forms.
            run("f11-generate-content-adapter", "/v1beta/models/claude-sonnet-4-5:generateContent", gem)
            run("f11b-generate-content-adapter-stream",
                "/v1beta/models/claude-sonnet-4-5:streamGenerateContent?alt=sse", gem)
        # F7: bridged /v1/responses to an Anthropic model, every variant.
        n7 = int(os.environ.get("F7_RUNS", "20"))
        for i in range(n7):
            run("f7-%s-%02d" % (VARIANT, i), "/v1/responses", {"model": "claude-sonnet-4-5", "input": "KW_SPLIT"})
        run("f7-%s-stream" % VARIANT, "/v1/responses",
            {"model": "claude-sonnet-4-5", "input": "KW_SPLIT", "stream": True})

        # Let the callback flush (and retry the 503'd batch if it does).
        time.sleep(15)
    finally:
        proxy.terminate()
        try:
            proxy.wait(timeout=20)
        except subprocess.TimeoutExpired:
            proxy.kill()

    all_records = [r for b in batches for r in b["records"]]
    summary = {"variant": VARIANT, "batches": [{"answer": b["answer"], "n": len(b["records"]),
                                                "ids": [r.get("id") for r in b["records"]]} for b in batches],
               "upstream": upstream_log, "runs": []}
    first_ids = set(summary["batches"][0]["ids"]) if batches else set()
    later_ids = set(i for b in summary["batches"][1:] for i in b["ids"])
    summary["s1_first_batch_answered_503_resent"] = bool(first_ids) and first_ids <= later_ids
    for name, cust, code in runs:
        recs = [r for r in all_records if cust and (r.get("end_user") == cust)]
        if name == "f1b-messages-no-customer-header":
            recs = [r for r in all_records if r.get("end_user") == "user_claude-code-session-0f3a"
                    or (r.get("metadata") or {}).get("user_api_key_end_user_id") == "user_claude-code-session-0f3a"]
        uniq = {}
        for r in recs:
            uniq.setdefault((r.get("id"), r.get("startTime")), r)
        summary["runs"].append({"name": name, "status": code, "records": len(recs),
                                "distinct_records": len(uniq),
                                "ids": sorted(set(r.get("id") for r in recs)),
                                "call_types": sorted(set(r.get("call_type") for r in recs))})
        with open(os.path.join(OUT, name + ".json"), "w") as f:
            json.dump(list(uniq.values()), f, indent=2, sort_keys=True, default=str)
    with open(os.path.join(OUT, "all-records-%s.json" % VARIANT), "w") as f:
        json.dump(all_records, f, indent=2, sort_keys=True, default=str)
    with open(os.path.join(OUT, "summary-%s.json" % VARIANT), "w") as f:
        json.dump(summary, f, indent=2, sort_keys=True)
    for r in summary["runs"]:
        print("%-44s status=%s records=%d distinct=%d call_types=%s" % (
            r["name"], r["status"], r["records"], r["distinct_records"], r["call_types"]))
    print("batches:", [(b["answer"], b["n"]) for b in summary["batches"]])
    print("S1 503'd batch sent again:", summary["s1_first_batch_answered_503_resent"])


if __name__ == "__main__":
    main()
