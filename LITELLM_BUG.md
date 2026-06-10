# LiteLLM v1.87.1: `ssl_verify` not propagated in `BaseLLMAIOHTTPHandler`

**Component:** `litellm/llms/custom_httpx/aiohttp_handler.py`
**Version:** 1.87.1
**Severity:** Medium — affects `ollama/` provider embeddings + any provider
using `BaseLLMAIOHTTPHandler` when `ssl_verify: false` is configured.

## Summary

Two related bugs cause `ssl_verify: false` to be silently ignored in specific
code paths:

1. **`BaseLLMAIOHTTPHandler` never accepts `ssl_verify`** — the constructor
   has no `ssl_verify` parameter, `_get_or_create_transport()` calls
   `AsyncHTTPHandler._create_aiohttp_transport()` without SSL args, and
   `_make_common_async_call()` calls `client_session.post()` without an
   `ssl=` kwarg. The result: aiohttp applies a default SSL context even on
   plain `http://` URLs.

2. **`AsyncHTTPHandler` retry path drops `ssl_verify`** — on
   `ConnectError`/`RemoteProtocolError`, the `post()`, `put()`, `patch()`,
   and `delete()` methods call `self.create_client(timeout=...,
   event_hooks=...)` *without* forwarding `ssl_verify`. The default (`None`)
   resolves to `True` via `get_ssl_configuration()`, so the retry attempt
   has SSL verification enabled regardless of the original setting.

## Affected code paths

| Path | Used by | Status |
|------|---------|--------|
| `AsyncHTTPHandler.__init__` → first `create_client()` | chat completions via `BaseLLMHTTPHandler` | correct |
| `AsyncHTTPHandler.post()` retry → second `create_client()` | any retry after `ConnectError` | **BUG: drops `ssl_verify`** |
| `BaseLLMAIOHTTPHandler._get_or_create_transport()` | `ollama/` embeddings, other aiohttp providers | **BUG: never receives `ssl_verify`** |

## Root cause trace

### Bug 1: `BaseLLMAIOHTTPHandler`

```
litellm_params.ssl_verify = False
  ↓ (Router spreads litellm_params as **kwargs)
litellm.aembedding(ssl_verify=False, ...)
  ↓ (main.py extracts ssl_verify from kwargs)
litellm_params dict has ssl_verify=False
  ↓ (BUT: ollama embeddings use BaseLLMAIOHTTPHandler, not BaseLLMHTTPHandler)
BaseLLMAIOHTTPHandler (instantiated once at main.py:317, no ssl_verify param)
  ↓ _get_or_create_transport() at aiohttp_handler.py:64
AsyncHTTPHandler._create_aiohttp_transport()  ← called with NO ssl args
  ↓ _get_ssl_connector_kwargs(ssl_verify=None, ssl_context=None)
connector_kwargs = {}  ← no 'ssl' key, aiohttp uses default SSL context
  ↓ _make_common_async_call() at aiohttp_handler.py:196-201
client_session.post(url=..., headers=..., json=...)  ← no ssl= kwarg
```

### Bug 2: `AsyncHTTPHandler` retry

```
AsyncHTTPHandler.__init__(ssl_verify=False)
  ↓ create_client(ssl_verify=False)  ← correct, ssl=False on transport
  ↓ post() → ConnectError or RemoteProtocolError
  ↓ retry at http_handler.py:644-648
self.create_client(timeout=timeout, event_hooks=self.event_hooks)
                    ← NO ssl_verify argument!
  ↓ get_ssl_configuration(None) → True
  ↓ httpx.AsyncClient(verify=True)  ← SSL verification re-enabled
```

## Observable symptom

```
OllamaException - Cannot connect to host 172.16.2.X:11434 ssl:<ssl.SSLContext object at 0x...>
```

This is aiohttp's `ClientConnectorError` format — the `ssl:<ssl.SSLContext>`
confirms a non-`None` SSL context was attached to a plain HTTP connection.

## Reproducer

Requires Docker/Podman + Python 3.11+.

```bash
# 1. Create a minimal HTTP echo server (stands in for Ollama)
cat > /tmp/echo_server.py << 'EOF'
from http.server import HTTPServer, BaseHTTPRequestHandler
import json

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        # Return a minimal OpenAI-shaped response
        resp = {"object": "list", "data": [{"embedding": [0.1, 0.2]}]}
        payload = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"models":[]}')

HTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
EOF

python3 /tmp/echo_server.py &
ECHO_PID=$!

# 2. Install litellm
pip install 'litellm==1.87.1'

# 3. Reproduce Bug 1: BaseLLMAIOHTTPHandler ignores ssl_verify
python3 -c "
import litellm, asyncio

async def test_bug1():
    '''BaseLLMAIOHTTPHandler does not propagate ssl_verify=False.
    Even on a plain http:// URL, aiohttp attaches an SSL context.
    With the ollama/ prefix, embedding calls go through BaseLLMAIOHTTPHandler.
    '''
    try:
        resp = await litellm.aembedding(
            model='ollama/test-model',
            input=['hello'],
            api_base='http://localhost:8000',
            ssl_verify=False,
        )
        print('BUG 1: PASS (no SSL error)')
    except Exception as e:
        err = str(e)
        if 'ssl' in err.lower() or 'SSL' in err:
            print(f'BUG 1: CONFIRMED — SSL context on plain HTTP: {err}')
        else:
            print(f'BUG 1: different error (may be unrelated): {err}')

asyncio.run(test_bug1())
"

# 4. Reproduce Bug 2: AsyncHTTPHandler retry drops ssl_verify
# (Requires a server that resets connections on first attempt)
# This is harder to reproduce in isolation; the code path is clear from reading:
#   http_handler.py:644-648 — create_client() called without ssl_verify

# 5. Cleanup
kill $ECHO_PID 2>/dev/null
```

**Note:** Bug 1 may not manifest on all systems because aiohttp's behavior
with SSL contexts on plain HTTP varies by platform and Python version. The
bug is definitively visible in the source: `_make_common_async_call()` at
`aiohttp_handler.py:196-201` calls `client_session.post()` without `ssl=`
kwarg, and the transport is created without any SSL configuration at
`aiohttp_handler.py:64`.

## Suggested fix

### Bug 1: `BaseLLMAIOHTTPHandler`

Pass `ssl_verify` through the handler chain:

```python
# aiohttp_handler.py

class BaseLLMAIOHTTPHandler:
    def __init__(self, client_session=None, transport=None, connector=None,
                 ssl_verify=None):  # ADD
        self.ssl_verify = ssl_verify  # ADD
        ...

    def _get_or_create_transport(self):
        if self.transport:
            return self.transport
        self.transport = AsyncHTTPHandler._create_aiohttp_transport(
            ssl_verify=self.ssl_verify,  # PASS THROUGH
        )
        ...
```

And in the caller at `main.py`:
```python
base_llm_aiohttp_handler = BaseLLMAIOHTTPHandler()
# → needs to be instantiated per-request with ssl_verify from litellm_params,
#   or ssl_verify must be passed per-call to _make_common_async_call()
```

### Bug 2: `AsyncHTTPHandler` retry

Store `ssl_verify` as instance state and forward on retry:

```python
# http_handler.py

class AsyncHTTPHandler:
    def __init__(self, ..., ssl_verify=None, ...):
        self._ssl_verify = ssl_verify  # ADD: store for retry
        self.client = self.create_client(
            timeout=timeout, event_hooks=event_hooks,
            ssl_verify=ssl_verify, shared_session=shared_session,
        )

    # In post(), put(), patch(), delete() retry blocks:
    except (httpx.RemoteProtocolError, httpx.ConnectError):
        new_client = self.create_client(
            timeout=timeout, event_hooks=self.event_hooks,
            ssl_verify=self._ssl_verify,  # FIX: forward stored value
        )
```

## Workaround

Use the `openai/` provider prefix instead of `ollama/`. This routes through
`BaseLLMHTTPHandler` → `AsyncHTTPHandler` (correct SSL path) for both chat
and embeddings. Ollama exposes an OpenAI-compatible API at `/v1`, so:

```yaml
model_list:
  - model_name: my-model
    litellm_params:
      model: openai/llama3.2:1b          # not ollama/llama3.2:1b
      api_base: http://ollama-host:11434/v1
      api_key: "sk-unused"               # required by openai client, Ollama ignores it
```

This avoids both bugs entirely — no aiohttp transport, no
`BaseLLMAIOHTTPHandler`.
