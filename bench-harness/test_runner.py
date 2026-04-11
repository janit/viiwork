"""Tests for the benchmark runner parsers."""
from runner import parse_server_timings, BenchmarkResult


def test_parse_timings_extracts_tokens_per_second():
    line = "prompt eval time =   123.45 ms /    50 tokens (    2.47 ms per token,   405.00 tokens per second)"
    result = parse_server_timings(line)
    assert result is not None
    assert result.kind == "prompt_eval"
    assert result.tokens == 50
    assert abs(result.tokens_per_second - 405.0) < 0.01


def test_parse_timings_handles_eval():
    line = "        eval time =  1234.56 ms /   200 runs   (    6.17 ms per run,   162.05 tokens per second)"
    result = parse_server_timings(line)
    assert result is not None
    assert result.kind == "eval"
    assert result.tokens == 200
    assert abs(result.tokens_per_second - 162.05) < 0.01


def test_parse_timings_returns_none_for_unrelated_lines():
    assert parse_server_timings("server is listening on http://0.0.0.0:8080") is None
    assert parse_server_timings("") is None


def test_benchmark_result_aggregation():
    results = [BenchmarkResult(tokens_per_second=100.0, latency_ms=10.0, peak_vram_mb=1000),
               BenchmarkResult(tokens_per_second=110.0, latency_ms=9.0, peak_vram_mb=1000),
               BenchmarkResult(tokens_per_second=105.0, latency_ms=9.5, peak_vram_mb=1000),
               BenchmarkResult(tokens_per_second=108.0, latency_ms=9.2, peak_vram_mb=1000),
               BenchmarkResult(tokens_per_second=102.0, latency_ms=9.8, peak_vram_mb=1000)]
    median_tps = sorted(r.tokens_per_second for r in results)[len(results) // 2]
    assert median_tps == 105.0
