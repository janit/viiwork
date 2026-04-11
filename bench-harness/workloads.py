"""Workload definitions for the gfx906 fork benchmark harness.

These are the canonical workloads against which every later phase
(strip-down, profile-guided kernel selection, kernel tuning) measures
itself. They run against the binary embedded in the viiwork docker
image (upstream b8660 + the FP8 sed) and against the stripped/tuned
fork that replaces it.

Model selection is deliberately the production family (Gemma-3) rather
than the original spec's Gemma-2 / Qwen-2.5 list, because:

  1. The dev host is the production node — the only models on disk are
     Gemma-3 variants.
  2. The success criterion is "this fork beats upstream on what we
     actually deploy", which is gemma-4-26B-A4B-it-UD-Q3_K_XL.
  3. Downloading 15 GB onto a disk with 31 GB free is risky.

Tier:
  - small  : gemma-4-E2B-it-Q8_0 (4.8 GB) — generation throughput floor
  - medium : gemma-4-E4B-it-Q8_0 (7.7 GB) — sanity middle tier
  - large  : gemma-4-26B-A4B-it-UD-Q3_K_XL (13 GB) — production model;
             primary success-criterion workload
"""
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Workload:
    name: str
    model_path: Path
    prompt: str
    n_predict: int
    n_ctx: int
    n_runs: int

    def __post_init__(self):
        if self.n_runs < 1:
            raise ValueError(f"n_runs must be >= 1, got {self.n_runs}")
        if self.n_predict < 1:
            raise ValueError(f"n_predict must be >= 1, got {self.n_predict}")
        if self.n_ctx < self.n_predict:
            raise ValueError(
                f"n_ctx ({self.n_ctx}) must be >= n_predict ({self.n_predict})"
            )


# A prompt that naturally encourages a long continuation. Avoid prompts that
# the model treats as a "tiny task" (e.g. "write a haiku") — Gemma-3 collapses
# to a degenerate stop-token stream on those, even with ignore_eos forcing the
# generation count up, which makes generation throughput unmeasurable.
SHORT_PROMPT = (
    "The history of the printing press began in the early 15th century when "
    "Johannes Gutenberg refined movable type into a practical system. "
    "Continue this article in detail, covering the technical evolution, "
    "the spread across Europe, and the social consequences:\n\n"
)

# A long-ish prompt for prompt-eval throughput. Repeats a paragraph until it
# crosses the ~3500-token mark on most tokenizers without needing a fixture file.
LONG_PROMPT = (
    "The quick brown fox jumps over the lazy dog near the river bank where "
    "willows bend low and the morning fog lifts slowly from the cool water. "
    "Dragonflies skim the surface while a heron stands motionless among reeds. "
) * 60


def default_workloads(models_dir: Path) -> list[Workload]:
    """Return the standard Gemma-3 workload set used for baseline + comparison."""
    return [
        Workload(
            name="gemma3-e2b-q8-gen-4k",
            model_path=models_dir / "gemma-4-E2B-it-Q8_0.gguf",
            prompt=SHORT_PROMPT,
            n_predict=512,
            n_ctx=4096,
            n_runs=5,
        ),
        Workload(
            name="gemma3-e4b-q8-gen-4k",
            model_path=models_dir / "gemma-4-E4B-it-Q8_0.gguf",
            prompt=SHORT_PROMPT,
            n_predict=512,
            n_ctx=4096,
            n_runs=5,
        ),
        Workload(
            name="gemma3-26b-a4b-q3kxl-gen-4k",
            model_path=models_dir / "gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf",
            prompt=SHORT_PROMPT,
            n_predict=512,
            n_ctx=4096,
            n_runs=5,
        ),
        Workload(
            name="gemma3-26b-a4b-q3kxl-prompt-4k",
            model_path=models_dir / "gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf",
            prompt=LONG_PROMPT,
            n_predict=64,
            n_ctx=4096,
            n_runs=5,
        ),
    ]
