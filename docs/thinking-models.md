# Thinking Models

Some models (e.g. Gemma 4, Qwen3, DeepSeek-R1) produce a reasoning phase before the final answer. The backend (`llama-server`) may return all output in `reasoning_content` with an empty `content` field, regardless of what the client requests. Viiwork's proxy normalizes this so clients always get usable responses.

## Disabling thinking

Add `"think": false` to your request body:

```json
{
  "model": "gemma-4-27b-it",
  "messages": [{"role": "user", "content": "What is 2+2?"}],
  "think": false
}
```

### What happens

**Non-streaming:** Viiwork buffers the response, strips `<think>...</think>` blocks from `reasoning_content`, and returns the final answer in `content`. The `reasoning_content` field is removed.

```json
{
  "choices": [{
    "message": {
      "role": "assistant",
      "content": "4"
    }
  }]
}
```

**Streaming:** Tokens inside `<think>...</think>` are suppressed. Once the thinking block closes, subsequent tokens are streamed as `delta.content`. The client sees nothing until the model starts producing the actual answer.

```
data: {"choices":[{"delta":{"content":"4"}}]}
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

### Models without think tags

If the model doesn't use `<think>...</think>` delimiters (or the backend strips them), the entire `reasoning_content` is moved to `content` as-is. The client gets all output in the right field, though it may include reasoning mixed with the answer.

## Default behavior (rewriting is ON)

**Omitting `think` is the same as sending `"think": false`.** The proxy code is
`thinkDisabled := reqBody.Think == nil || !*reqBody.Think` — only an explicit
`"think": true` turns rewriting off.

This matters because it is the path almost every client takes: an OpenAI SDK
that knows nothing about viiwork sends no `think` field, so its responses are
rewritten. That is usually what you want (clients get the answer in `content`
rather than an empty `content` beside a full `reasoning_content`), but it is not
passthrough, and it is worth knowing when a response does not look like what the
backend produced.

Set `"think": true` for genuine passthrough: the response is returned exactly as
the backend sent it, `reasoning_content` and all.

## Summary

| `think` value | Non-streaming | Streaming |
|---|---|---|
| omitted (**default**) | Think blocks stripped, answer in `content` | Think tokens suppressed, answer streamed as `delta.content` |
| `false` | Think blocks stripped, answer in `content` | Think tokens suppressed, answer streamed as `delta.content` |
| `true` | Transparent passthrough | Transparent passthrough |

## Diagnosing "the model is dumping its monologue into content"

If a response arrives as raw reasoning prose ("Okay, the user is asking...")
rather than an answer, **check `finish_reason` before suspecting viiwork**.

`length` means the client's `max_tokens` cut generation off mid-thought. The
proxy can only strip a **closed** `<think>...</think>` block; an unterminated one
falls through verbatim by design, because the alternative is discarding output
that may be all the client is going to get. Raise `max_tokens`.

If `finish_reason` is `stop` and the monologue is still there, the model emitted
no think delimiters at all — see "Models without think tags" above.

Separately, some models take a *template* argument that decides whether they
delimit their reasoning, and getting it wrong produces exactly this symptom with
nothing wrong on the viiwork side. Laguna is one: its chat template must be run
with `enable_thinking=true` (via `LLAMA_ARG_CHAT_TEMPLATE_KWARGS`, since
llama-server accepts `--chat-template-kwargs` and then ignores it) or the model
reasons with no tags around it and the whole monologue lands in `content`.

## Capping how much a model thinks

Thinking volume, not tok/s, is usually what makes a reasoning model feel slow.
Measured on Laguna-S-2.1: "write a Python IPv4 validator, just the function"
produced **1767 tokens, of which ~1667 were thinking** — 80 seconds of silence
before ~5 seconds of correct code, 94% of the wall clock spent on output nobody
sees.

`--reasoning-budget N` in `backend.extra_args` makes llama.cpp inject the
end-of-thinking tag after N tokens, forcing the model to answer:

```yaml
backend:
  extra_args: ["--jinja", "--reasoning-budget", "512"]
```

Same prompt at 512: **612 tokens / 29.7 s**, a 2.9× cut, and the capped answer
was equivalent. `-1` is unbounded, `0` ends thinking immediately.

Two caveats:

- The **per-request** `reasoning_budget` field in the JSON body is **ignored**
  (verified at 0 / 128 / 256, all identical to baseline). Only the server flag
  works, so tuning it costs a restart.
- A budget validated on small tasks says nothing about long-horizon agentic
  work, which is exactly where a thinking cap would be expected to hurt. If
  answers start feeling shallow on hard problems, raise it before suspecting
  anything else.

## Telling how much of a response was thinking

`usage.completion_tokens` counts thinking; the visible `content` does not. The
gap between them is the invisible cost:

```
completion_tokens 1767, content ~100 tokens  ->  ~1667 spent thinking
```

Worth checking before concluding a model is slow — decode rate may be fine.
