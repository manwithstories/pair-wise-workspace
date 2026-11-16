"""Batch scheduling: scan inputs, isolate failures, share one resampler kernel.

Design points that matter operationally:

* **Failure isolation** — one bad file must never abort the run.  Every
  per-file error is caught, recorded with its code and message, and the batch
  continues; the process exit code then reflects whether anything failed.
* **One kernel** — a single :class:`PolyphaseResampler` is created per
  (input rate -> target rate) pair and reused for every file at that rate.
  Each file still gets fresh streaming state, so no audio leaks between files,
  but the (expensive) coefficient bank is built once and the batch provably
  uses one configuration.
* **No partial outputs** — writing goes through
  :func:`audio_tool.io.write_wav_atomic`, so an interrupted file leaves no
  file at the destination path.
* **Deterministic ordering** — inputs are sorted by relative path so two runs
  over the same tree produce the same report ordering.
"""

from __future__ import annotations

import concurrent.futures
import hashlib
import os
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable, Sequence

from . import io as audio_io
from .model import AudioToolError, ProcessConfig, UsageError
from .normalize import NormalizeResult, run_two_pass
from .resampler import PolyphaseResampler

#: Input extensions the scanner picks up (case-insensitive).
INPUT_SUFFIXES = (".wav", ".flac")


@dataclass
class FileOutcome:
    """Result of attempting one input file.

    The report records ``source`` and ``output`` *relative* to their roots so
    it stays portable: it describes the shape of a delivery set instead of
    embedding the machine's absolute paths.  ``destination`` keeps the real
    path for the batch itself to use.
    """

    source: Path
    relative: str
    destination: Path | None
    result: NormalizeResult | None = None
    error_code: str | None = None
    error_message: str | None = None
    input_sha256: str | None = None

    @property
    def ok(self) -> bool:
        return self.error_code is None

    def to_dict(self) -> dict:
        payload = {
            "source": self.relative,
            "output": self.relative if self.destination is not None else None,
            "ok": self.ok,
            "input_sha256": self.input_sha256,
        }
        if self.ok and self.result is not None:
            payload["stats"] = self.result.to_dict()
        else:
            payload["error"] = {"code": self.error_code, "message": self.error_message}
        return payload


@dataclass
class BatchResult:
    """Everything one batch run produced."""

    outcomes: list[FileOutcome] = field(default_factory=list)
    config: ProcessConfig | None = None
    input_root: Path | None = None
    output_root: Path | None = None

    @property
    def succeeded(self) -> list[FileOutcome]:
        return [o for o in self.outcomes if o.ok]

    @property
    def failed(self) -> list[FileOutcome]:
        return [o for o in self.outcomes if not o.ok]

    def exit_code(self) -> int:
        if not self.outcomes:
            return 2
        if self.failed:
            return 1
        return 0


def discover_inputs(root: Path) -> list[Path]:
    """All audio files under ``root``, or ``root`` itself when it is a file."""

    root = Path(root)
    if not root.exists():
        raise UsageError(f"input path does not exist: {root}", code="input_missing")
    if root.is_file():
        return [root] if root.suffix.lower() in INPUT_SUFFIXES else []
    found = [
        path for path in root.rglob("*")
        if path.is_file() and path.suffix.lower() in INPUT_SUFFIXES
    ]
    return sorted(found, key=lambda path: str(path.relative_to(root)))


def parse_manifest(path: Path) -> list[Path]:
    """Read a newline-separated manifest of input paths.

    Blank lines and ``#`` comments are ignored; paths are resolved relative to
    the manifest's own directory so a manifest is portable with its tree.
    """

    path = Path(path)
    if not path.is_file():
        raise UsageError(f"manifest not found: {path}", code="manifest_missing")
    base = path.parent
    entries: list[Path] = []
    for lineno, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        candidate = Path(line)
        if not candidate.is_absolute():
            candidate = base / candidate
        if candidate.suffix.lower() not in INPUT_SUFFIXES:
            raise UsageError(
                f"{path}:{lineno}: {line!r} is not a .wav/.flac input",
                code="manifest_invalid")
        entries.append(candidate)
    if not entries:
        raise UsageError(f"manifest lists no inputs: {path}", code="manifest_invalid")
    return entries


def collect_inputs(inputs: Sequence[Path] | None, manifest: Path | None) -> list[Path]:
    """Merge explicit paths, a manifest, or a directory scan (in that order)."""

    paths: list[Path] = []
    if manifest is not None:
        paths.extend(parse_manifest(manifest))
    for entry in inputs or ():
        entry = Path(entry)
        if entry.is_dir():
            paths.extend(discover_inputs(entry))
        elif entry.exists():
            paths.append(entry)
        else:
            raise UsageError(f"input does not exist: {entry}", code="input_missing")
    if not paths:
        raise UsageError("no inputs found", code="no_inputs")
    # De-duplicate while preserving order.
    seen: set[str] = set()
    unique: list[Path] = []
    for path in paths:
        key = str(Path(path).resolve())
        if key in seen:
            continue
        seen.add(key)
        unique.append(path)
    return unique


def plan_destinations(inputs: Sequence[Path], output_root: Path,
                      input_root: Path | None) -> list[tuple[Path, Path, str]]:
    """Map each input to an output path, preserving relative sub-directories."""

    output_root = Path(output_root)
    plan: list[tuple[Path, Path, str]] = []
    for source in inputs:
        source = Path(source)
        if input_root is not None:
            try:
                relative = source.resolve().relative_to(Path(input_root).resolve())
            except ValueError:
                relative = Path(source.name)
        else:
            relative = Path(source.name)
        destination = output_root / relative
        plan.append((source, destination, str(relative)))
    return plan


class KernelCache:
    """Lazily build and share resampler kernels keyed by input rate.

    Thread-safe: construction happens under a lock, so a concurrent batch
    builds exactly one kernel per rate rather than one per racing worker.
    Kernels are immutable after construction; each file still takes its own
    ``stream()``, so sharing the instance carries no per-file state.
    """

    def __init__(self, config: ProcessConfig):
        self.config = config
        self._kernels: dict[int, PolyphaseResampler] = {}
        self._lock = threading.Lock()

    def get(self, input_rate: int) -> PolyphaseResampler:
        kernel = self._kernels.get(input_rate)
        if kernel is not None:
            return kernel
        with self._lock:
            kernel = self._kernels.get(input_rate)
            if kernel is None:
                kernel = PolyphaseResampler(
                    self.config.resampler, input_rate, self.config.target_rate)
                self._kernels[input_rate] = kernel
        return kernel

    @property
    def rates(self) -> list[int]:
        return sorted(self._kernels)


def _sha256(path: Path, chunk: int = 1 << 20) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for piece in iter(lambda: handle.read(chunk), b""):
            digest.update(piece)
    return digest.hexdigest()


def process_one(source: Path, destination: Path, relative: str,
                config: ProcessConfig, cache: KernelCache,
                *, fingerprint: bool, log: Callable[[str], None] | None = None
                ) -> FileOutcome:
    """Normalize one file, converting failures into an outcome (never raising)."""

    outcome = FileOutcome(source=source, relative=relative, destination=destination)
    try:
        if fingerprint:
            outcome.input_sha256 = _sha256(source)
        kernel = cache.get(audio_io.probe(source).samplerate)
        outcome.result = run_two_pass(source, destination, config,
                                      kernel=kernel, log=log)
        return outcome
    except AudioToolError as exc:
        outcome.error_code = exc.code
        outcome.error_message = exc.message
    except OSError as exc:
        outcome.error_code = "io_error"
        outcome.error_message = f"{type(exc).__name__}: {exc}"
    except Exception as exc:  # defensive: a bug must not take down the batch
        outcome.error_code = "internal_error"
        outcome.error_message = f"{type(exc).__name__}: {exc}"
    # A failed file must not leave a destination file behind that looks done.
    _remove_quietly(destination)
    return outcome


def _remove_quietly(path: Path | None) -> None:
    if path is None:
        return
    try:
        Path(path).unlink(missing_ok=True)
    except OSError:
        pass


def run_batch(
    plan: Sequence[tuple[Path, Path, str]],
    config: ProcessConfig,
    *,
    jobs: int = 1,
    input_root: Path | None = None,
    output_root: Path | None = None,
    fingerprint: bool = True,
    log: Callable[[str], None] | None = None,
    progress: Callable[[FileOutcome], None] | None = None,
) -> BatchResult:
    """Run the plan with a shared kernel cache and failure isolation."""

    config.validate()
    if jobs < 1:
        raise UsageError("--jobs must be >= 1", code="bad_jobs")
    cache = KernelCache(config)
    result = BatchResult(config=config, input_root=input_root,
                         output_root=output_root)

    def work(item: tuple[Path, Path, str]) -> FileOutcome:
        source, destination, relative = item
        return process_one(source, destination, relative, config, cache,
                           fingerprint=fingerprint, log=log)

    if jobs == 1:
        for item in plan:
            outcome = work(item)
            result.outcomes.append(outcome)
            if progress:
                progress(outcome)
    else:
        with concurrent.futures.ThreadPoolExecutor(max_workers=jobs) as pool:
            futures = {pool.submit(work, item): item for item in plan}
            for future in concurrent.futures.as_completed(futures):
                outcome = future.result()
                result.outcomes.append(outcome)
                if progress:
                    progress(outcome)
        # Completion order is nondeterministic; sort back to plan order.
        order = {str(item[0]): index for index, item in enumerate(plan)}
        result.outcomes.sort(key=lambda o: order.get(str(o.source), 0))

    return result
