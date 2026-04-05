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

## Default behavior (thinking enabled)

When `think` is omitted or set to `true`, the proxy is fully transparent. The response passes through exactly as the backend returns it, including any `reasoning_content` field.

## Summary

| `think` value | Non-streaming | Streaming |
|---|---|---|
| `false` | Think blocks stripped, answer in `content` | Think tokens suppressed, answer streamed as `delta.content` |
| `true` or omitted | Transparent passthrough | Transparent passthrough |
