"""Tests for workload definitions."""
from pathlib import Path

import pytest

from workloads import Workload, default_workloads


def test_workload_rejects_zero_runs():
    with pytest.raises(ValueError, match="n_runs"):
        Workload(name="x", model_path=Path("/dev/null"), prompt="hi",
                 n_predict=10, n_ctx=100, n_runs=0)


def test_workload_rejects_predict_larger_than_ctx():
    with pytest.raises(ValueError, match="n_ctx"):
        Workload(name="x", model_path=Path("/dev/null"), prompt="hi",
                 n_predict=100, n_ctx=50, n_runs=1)


def test_default_workloads_has_four_entries():
    workloads = default_workloads(Path("/tmp/models"))
    assert len(workloads) == 4


def test_default_workloads_unique_names():
    workloads = default_workloads(Path("/tmp/models"))
    names = [w.name for w in workloads]
    assert len(names) == len(set(names)), "workload names must be unique"


def test_default_workloads_n_predict_le_n_ctx():
    for w in default_workloads(Path("/tmp/models")):
        assert w.n_predict <= w.n_ctx


def test_default_workloads_includes_production_model():
    """The production model is the primary success-criterion workload."""
    workloads = default_workloads(Path("/models"))
    paths = [str(w.model_path) for w in workloads]
    assert any("gemma-4-26B-A4B-it-UD-Q3_K_XL" in p for p in paths)
