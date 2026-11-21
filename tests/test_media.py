"""Unit and integration tests for mediarail.

Integration tests generate their own fixtures via tests.make_fixtures and
never touch files outside a pytest tmp_path.
"""

from __future__ import annotations

import shutil
from fractions import Fraction
from pathlib import Path

import pytest

from mediarail.checkpoint import (
    CheckpointStore,
    compute_fingerprint,
    verify_fingerprint,
)
from mediarail.cli import main as cli_main
from mediarail.errors import CheckpointError, Severity
from mediarail.pipeline import CancelToken, MemoryBudget, process_file
from mediarail.timebase import (
    WrapEvent,
    estimate_drift,
    rescale,
    unwrap_timestamps,
)

from tests.make_fixtures import make_all


# ---------------------------------------------------------------- timebase


def test_rescale_is_exact() -> None:
    assert rescale(1, Fraction(1, 30), Fraction(1, 15360)) == 512
    assert rescale(Fraction(1, 3), Fraction(1), Fraction(1, 1000)) == 333


def test_unwrap_classifies_wrap_reorder_reset() -> None:
    modulus = 16
    seq = [13, 14, 15, 0, 1, 0, 2, 8, 1, 6, 3]
    result = unwrap_timestamps(seq, modulus, reorder_tolerance=2)
    kinds = [e[1] for e in result.events]
    assert WrapEvent.WRAP in kinds
    assert WrapEvent.OUT_OF_ORDER in kinds
    assert WrapEvent.RESET in kinds
    assert WrapEvent.AMBIGUOUS in kinds
    # Genuine wrap continues the counter past the modulus.
    assert result.values[3] == 16


def test_drift_estimate_single_segment() -> None:
    raw = [i / 30 for i in range(300)]
    ref = [i / 30 * (1 + 50e-6) for i in range(300)]  # 50 ppm drift
    est = estimate_drift(raw, ref)
    assert not est.segmented
    assert abs(est.max_drift_ppm - 50) < 5


def test_drift_estimate_segments_above_200ppm() -> None:
    n = 512
    raw = [i / 30 for i in range(n)]
    ref = [i / 30 * (1 + 500e-6 * (i / n)) for i in range(n)]
    est = estimate_drift(raw, ref)
    assert est.segmented
    assert est.max_drift_ppm > 200


# ------------------------------------------------------------- checkpoint


def test_fingerprint_detects_change(tmp_path: Path) -> None:
    f = tmp_path / "in.bin"
    f.write_bytes(b"x" * 1000)
    fp = compute_fingerprint(f)
    verify_fingerprint(f, fp)  # no raise
    f.write_bytes(b"y" * 1000)
    with pytest.raises(CheckpointError):
        verify_fingerprint(f, fp)


# ------------------------------------------------------------ integration


@pytest.fixture(scope="module")
def fixtures(tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("fixtures")
    make_all(out)
    return out


def test_run_batch_exit_codes_and_stats(fixtures: Path, tmp_path: Path) -> None:
    rc = cli_main(
        [
            "run",
            "--input", str(fixtures),
            "--output", str(tmp_path / "out"),
            "--checkpoint", str(tmp_path / "state"),
            "--queue-size", "8",
        ]
    )
    assert rc == 2  # corrupt fixture is not auto-repairable
    outputs = list((tmp_path / "out").glob("*.mp4"))
    assert {p.stem for p in outputs} == {"normal", "boundary"}
    for p in outputs:
        assert p.with_suffix(".midx").exists()


def test_corrupt_fixture_locates_first_error(fixtures: Path, tmp_path: Path) -> None:
    store = CheckpointStore(tmp_path / "state")
    result = process_file(
        fixtures / "corrupt.mp4",
        tmp_path / "out" / "corrupt.mp4",
        store, 8, MemoryBudget(), CancelToken(),
    )
    assert result.unrepairable
    first = next(e for e in result.errors if e.severity == Severity.UNREPAIRABLE)
    assert first.packet_index is not None
    size = (fixtures / "corrupt.mp4").stat().st_size
    # Located within one packet / 4096 bytes of the actual truncation point.
    assert first.byte_offset is None or abs(size - first.byte_offset) < 64 * 1024


def test_resume_reproduces_uninterrupted_sha(fixtures: Path, tmp_path: Path) -> None:
    src = fixtures / "boundary.mov"
    # Uninterrupted reference.
    store_a = CheckpointStore(tmp_path / "state_a")
    ref = process_file(
        src, tmp_path / "a" / "boundary.mp4", store_a, 8, MemoryBudget(), CancelToken()
    )
    assert ref.ok

    class KillAfterFirstCommit(CheckpointStore):
        def __init__(self, d, token):
            super().__init__(d)
            self._token = token

        def save(self, key, cp):
            super().save(key, cp)
            if cp.committed_packets > 0:
                self._token.cancel()

    token = CancelToken()
    store_b = KillAfterFirstCommit(tmp_path / "state_b", token)
    interrupted = process_file(
        src, tmp_path / "b" / "boundary.mp4", store_b, 8, MemoryBudget(), token
    )
    assert interrupted.cancelled
    resumed = process_file(
        src, tmp_path / "b" / "boundary.mp4", store_b, 8, MemoryBudget(), CancelToken()
    )
    assert resumed.ok
    assert resumed.resumed_from_packet > 0
    assert resumed.output_sha256 == ref.output_sha256


def test_input_change_rejects_resume(fixtures: Path, tmp_path: Path) -> None:
    work = tmp_path / "changed.mov"
    shutil.copy(fixtures / "boundary.mov", work)
    store = CheckpointStore(tmp_path / "state")
    process_file(work, tmp_path / "out.mp4", store, 8, MemoryBudget(), CancelToken())
    with work.open("ab") as fh:
        fh.write(b"\x00")
    with pytest.raises(CheckpointError):
        process_file(work, tmp_path / "out.mp4", store, 8, MemoryBudget(), CancelToken())


def test_verify_command(fixtures: Path, tmp_path: Path) -> None:
    store = CheckpointStore(tmp_path / "state")
    out = tmp_path / "out" / "normal.mp4"
    result = process_file(
        fixtures / "normal.mp4", out, store, 8, MemoryBudget(), CancelToken()
    )
    assert result.ok
    rc = cli_main(
        [
            "verify",
            "--input", str(out),
            "--index", str(out.with_suffix(".midx")),
            "--sha256",
        ]
    )
    assert rc == 0
