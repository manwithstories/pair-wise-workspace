"""Read-only scanning and concurrent decode/fingerprint indexing."""

from __future__ import annotations

import json
import os
import time
from concurrent.futures import ProcessPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from mediaidx.canonical import (
    MediaFamily,
    MediaKind,
    PathValidationError,
    classify_exception,
    chunked_sha256,
    sniff_kind,
    validate_media_path,
)
from mediaidx.dedup import Deduper, discover_truth_manifest, load_truth_labels
from mediaidx.fingerprint import (
    candidate_hashes,
    fingerprint_loaded,
    pack_fingerprint,
)
from mediaidx.loaders import LOADER_VERSION, REGISTRY, DecodeFailure
from mediaidx.store import Store


@dataclass(frozen=True)
class ScanEntry:
    rel_path: str
    absolute_path: str
    kind: MediaKind
    size: int
    mtime_ns: int
    content_hash: str


@dataclass
class ScanStats:
    skipped_hidden_directories: int = 0
    skipped_hidden_files: int = 0
    skipped_symlinks: int = 0
    skipped_zero_byte: int = 0
    skipped_non_media: int = 0
    skipped_io_errors: int = 0
    entries: int = 0

    def as_dict(self) -> dict[str, int]:
        return {
            "media_files": self.entries,
            "skipped_hidden_directories": self.skipped_hidden_directories,
            "skipped_hidden_files": self.skipped_hidden_files,
            "skipped_symlinks": self.skipped_symlinks,
            "skipped_zero_byte": self.skipped_zero_byte,
            "skipped_non_media": self.skipped_non_media,
            "skipped_io_errors": self.skipped_io_errors,
        }


@dataclass
class IndexStats:
    decode_calls: int = 0
    decode_cache_hits: int = 0
    unchanged_files: int = 0
    failed_unchanged: int = 0
    deleted_files: int = 0
    failed_decodes: int = 0
    successful_decodes: int = 0
    scan: ScanStats = field(default_factory=ScanStats)

    def as_dict(self) -> dict[str, Any]:
        return {
            "decode_calls": self.decode_calls,
            "decode_cache_hits": self.decode_cache_hits,
            "unchanged_files": self.unchanged_files,
            "failed_unchanged": self.failed_unchanged,
            "deleted_files": self.deleted_files,
            "failed_decodes": self.failed_decodes,
            "successful_decodes": self.successful_decodes,
            "scan": self.scan.as_dict(),
        }


def scan_corpus(
    root: str | os.PathLike[str], exclude_paths: Iterable[str | os.PathLike[str]] | None = None
) -> tuple[list[ScanEntry], ScanStats]:
    root_path = Path(root).resolve(strict=True)
    if not root_path.is_dir():
        raise PathValidationError(f"corpus root is not a directory: {root_path}")
    entries: list[ScanEntry] = []
    stats = ScanStats()
    excluded = {
        str(Path(path).expanduser().resolve(strict=False)) for path in (exclude_paths or ())
    }
    directory_stack: list[Path] = [root_path]
    while directory_stack:
        directory = directory_stack.pop()
        try:
            with os.scandir(directory) as scanner:
                children = sorted(list(scanner), key=lambda item: item.name)
        except OSError:
            stats.skipped_io_errors += 1
            continue
        for child in children:
            if child.is_symlink():
                stats.skipped_symlinks += 1
                continue
            try:
                is_directory = child.is_dir(follow_symlinks=False)
            except OSError:
                stats.skipped_io_errors += 1
                continue
            if is_directory:
                if child.name.startswith("."):
                    stats.skipped_hidden_directories += 1
                else:
                    directory_stack.append(Path(child.path))
                continue
            try:
                file_stat = child.stat(follow_symlinks=False)
            except OSError:
                stats.skipped_io_errors += 1
                continue
            if not child.is_file(follow_symlinks=False):
                continue
            if file_stat.st_size == 0:
                stats.skipped_zero_byte += 1
                continue
            absolute = Path(child.path)
            if str(absolute.resolve(strict=False)) in excluded:
                continue
            try:
                with open(absolute, "rb") as stream:
                    kind = sniff_kind(stream.read(64))
            except OSError:
                stats.skipped_io_errors += 1
                continue
            if kind is None:
                stats.skipped_non_media += 1
                continue
            try:
                content_hash = chunked_sha256(absolute)
            except OSError:
                stats.skipped_io_errors += 1
                continue
            relative = absolute.relative_to(root_path).as_posix()
            entries.append(
                ScanEntry(
                    rel_path=relative,
                    absolute_path=str(absolute),
                    kind=kind,
                    size=int(file_stat.st_size),
                    mtime_ns=int(file_stat.st_mtime_ns),
                    content_hash=content_hash,
                )
            )
    entries.sort(key=lambda entry: entry.rel_path)
    stats.entries = len(entries)
    return entries, stats


def _worker_decode(job: dict[str, Any]) -> dict[str, Any]:
    """Decode, normalize and fingerprint one file.  Never touches SQLite."""

    root = job["root"]
    absolute = job["absolute_path"]
    expected = MediaKind(MediaFamily(job["family"]), job["format"])
    try:
        resolved, _relative = validate_media_path(absolute, root)
        loaded = REGISTRY.decode_for_fingerprint(resolved, expected)
        if isinstance(loaded, DecodeFailure):
            return {
                "status": "failed",
                "kind": expected,
                "error_type": loaded.error_type,
                "message": loaded.message,
            }
        fingerprint = fingerprint_loaded(loaded)
        payload = pack_fingerprint(fingerprint)
        hashes = candidate_hashes(fingerprint)
        return {
            "status": "ok",
            "kind": loaded.kind,
            "payload": payload,
            "hashes": hashes,
            "metadata": loaded.metadata,
            "loader_name": loaded.loader_name,
            "fingerprint_version": fingerprint.get("version"),
        }
    except Exception as exc:
        return {
            "status": "failed",
            "kind": expected,
            "error_type": classify_exception(exc),
            "message": str(exc),
        }


class Indexer:
    def __init__(
        self,
        root: str | os.PathLike[str],
        db_path: str | os.PathLike[str],
        *,
        jobs: int = 1,
        labels_path: str | os.PathLike[str] | None = None,
    ) -> None:
        self.root = Path(root)
        self.db_path = Path(db_path)
        self.jobs = max(1, int(jobs))
        self.labels_path = Path(labels_path) if labels_path is not None else None

    def run(self) -> dict[str, Any]:
        started = time.time()
        database_paths = [self.db_path, Path(f"{self.db_path}-wal"), Path(f"{self.db_path}-shm")]
        entries, scan_stats = scan_corpus(self.root, database_paths)
        store = Store(self.db_path)
        stats = IndexStats(scan=scan_stats)
        store_error: Exception | None = None
        try:
            existing = {str(row["rel_path"]): row for row in store.all_files()}
            # File rows may change below.  Drop the previous snapshot first so
            # a hard interruption can only leave "no cluster snapshot", never a
            # half-updated cluster.  The next run rebuilds from fingerprints.
            store.clear_clusters()
            tasks: list[dict[str, Any]] = []
            for entry in entries:
                row = existing.get(entry.rel_path)
                if row is not None and str(row["content_hash"]) == entry.content_hash:
                    if (
                        str(row["status"]) == "ok"
                        and row["fingerprint"] is not None
                        and str(row["loader_version"] or "") == LOADER_VERSION
                    ):
                        store.touch_success(entry.rel_path, entry.size, entry.mtime_ns)
                        stats.unchanged_files += 1
                        continue
                    if str(row["status"]) == "failed":
                        stats.failed_unchanged += 1
                        continue

                cached = store.lookup_cached_fingerprint(entry.content_hash, LOADER_VERSION)
                if cached is not None:
                    payload, hashes, metadata, cached_family = cached
                    if cached_family == entry.kind.family.value:
                        store.upsert_success(
                            rel_path=entry.rel_path,
                            kind=entry.kind,
                            content_hash=entry.content_hash,
                            size=entry.size,
                            mtime_ns=entry.mtime_ns,
                            payload=payload,
                            hashes=hashes,
                            metadata=metadata,
                            loader_name=metadata.get("loader", "decode-cache"),
                            loader_version=LOADER_VERSION,
                        )
                        stats.decode_cache_hits += 1
                        continue

                tasks.append(
                    {
                        "root": str(self.root.resolve(strict=True)),
                        "absolute_path": entry.absolute_path,
                        "rel_path": entry.rel_path,
                        "family": entry.kind.family.value,
                        "format": entry.kind.format,
                    }
                )

            seen_paths = [entry.rel_path for entry in entries]
            stats.deleted_files = store.delete_missing(seen_paths)
            stats.decode_calls = len(tasks)

            if tasks:
                self._run_tasks(store, entries, tasks, stats)

            truth_manifest = discover_truth_manifest(self.root, self.labels_path)
            truth_labels = load_truth_labels(truth_manifest)
            deduper = Deduper(store)
            dedup_stats = deduper.run(truth_labels)

            report = self._build_report(
                store,
                entries,
                stats,
                dedup_stats,
                truth_manifest,
                started,
            )
        except BaseException as exc:
            store_error = exc
            raise
        finally:
            store.close()
            if store_error is not None:
                # The exception is intentionally re-raised above; this branch
                # exists to document that no report is published on failure.
                pass
        return report

    def _run_tasks(
        self,
        store: Store,
        entries: list[ScanEntry],
        tasks: list[dict[str, Any]],
        stats: IndexStats,
    ) -> None:
        entries_by_path = {entry.rel_path: entry for entry in entries}
        if self.jobs == 1:
            for task in tasks:
                result = _worker_decode(task)
                self._commit_result(store, entries_by_path[task["rel_path"]], result, stats)
            return

        with ProcessPoolExecutor(max_workers=min(self.jobs, len(tasks))) as pool:
            future_map = {pool.submit(_worker_decode, task): task for task in tasks}
            for future in as_completed(future_map):
                task = future_map[future]
                entry = entries_by_path[task["rel_path"]]
                try:
                    result = future.result()
                except Exception as exc:
                    result = {
                        "status": "failed",
                        "kind": entry.kind,
                        "error_type": classify_exception(exc),
                        "message": str(exc),
                    }
                self._commit_result(store, entry, result, stats)

    def _commit_result(self, store: Store, entry: ScanEntry, result: dict[str, Any], stats: IndexStats) -> None:
        if result.get("status") == "ok":
            payload = result["payload"]
            hashes = result["hashes"]
            metadata = result.get("metadata", {})
            kind = result.get("kind", entry.kind)
            loader_name = result.get("loader_name", "unknown")
            store.put_decode_cache(
                entry.content_hash,
                kind.family.value,
                LOADER_VERSION,
                payload,
                hashes,
                metadata,
            )
            store.upsert_success(
                rel_path=entry.rel_path,
                kind=kind,
                content_hash=entry.content_hash,
                size=entry.size,
                mtime_ns=entry.mtime_ns,
                payload=payload,
                hashes=hashes,
                metadata=metadata,
                loader_name=loader_name,
                loader_version=LOADER_VERSION,
            )
            stats.successful_decodes += 1
        else:
            kind = result.get("kind", entry.kind)
            store.mark_failed(
                rel_path=entry.rel_path,
                kind=kind,
                content_hash=entry.content_hash,
                size=entry.size,
                mtime_ns=entry.mtime_ns,
                error_type=str(result.get("error_type", "decode_error")),
                error_message=str(result.get("message", "")),
            )
            stats.failed_decodes += 1

    def _build_report(
        self,
        store: Store,
        entries: list[ScanEntry],
        stats: IndexStats,
        dedup_stats: Any,
        truth_manifest: Path | None,
        started: float,
    ) -> dict[str, Any]:
        counts = store.counts()
        candidate_by_family = store.candidate_counts_by_family()
        verified_by_family = store.verified_counts_by_family()
        duplicate_by_family = store.duplicate_cluster_counts_by_family()
        cluster_by_family = store.cluster_counts_by_family()
        file_by_family = store.group_counts()

        groups: dict[str, dict[str, Any]] = {}
        for family in ("image", "audio", "video"):
            file_group = file_by_family.get(
                family, {"total": 0, "success": 0, "failed": 0}
            )
            cluster_group = cluster_by_family.get(family, {"clusters": 0, "members": 0})
            candidates = int(candidate_by_family.get(family, 0))
            verified = int(verified_by_family.get(family, 0))
            groups[family] = {
                "total": int(file_group["total"]),
                "success": int(file_group["success"]),
                "failed": int(file_group["failed"]),
                "duplicate_clusters": int(duplicate_by_family.get(family, 0)),
                "unique_assets": int(cluster_group["clusters"]),
                "clustered_members": int(cluster_group["members"]),
                "candidate_pairs": candidates,
                "verified_pairs": verified,
                "rejected_pairs": candidates - verified,
            }
        for family, values in file_by_family.items():
            if family not in groups:
                groups[family] = {
                    "total": int(values["total"]),
                    "success": int(values["success"]),
                    "failed": int(values["failed"]),
                    "duplicate_clusters": int(duplicate_by_family.get(family, 0)),
                    "unique_assets": int(cluster_by_family.get(family, {}).get("clusters", 0)),
                    "clustered_members": int(cluster_by_family.get(family, {}).get("members", 0)),
                    "candidate_pairs": int(candidate_by_family.get(family, 0)),
                    "verified_pairs": int(verified_by_family.get(family, 0)),
                    "rejected_pairs": int(candidate_by_family.get(family, 0))
                    - int(verified_by_family.get(family, 0)),
                }
        for family, dedup_group in dedup_stats.group_details.items():
            groups.setdefault(family, {}).update(dedup_group)

        summary = {
            "total": counts["total"],
            "success": counts["success"],
            "failed": counts["failed"],
            "duplicate_clusters": int(dedup_stats.duplicate_clusters),
            "unique_assets": int(dedup_stats.clusters),
            "candidate_pairs": int(dedup_stats.candidate_pairs),
            "verified_pairs": int(dedup_stats.verified_pairs),
            "rejected_pairs": int(dedup_stats.rejected_pairs),
            "conflict_pairs": int(dedup_stats.conflict_pairs),
            "conflict_rate": float(dedup_stats.conflict_rate),
            "conflict_rate_percent": round(float(dedup_stats.conflict_rate) * 100.0, 6),
            "truth_available": bool(dedup_stats.truth_available),
        }
        # Explicit aliases keep machine consumers from having to guess Chinese
        # terms while the CLI keeps its fixed Chinese summary.
        summary.update(
            {
                "total_files": counts["total"],
                "total_assets": counts["total"],
                "decode_success": counts["success"],
                "decode_failed": counts["failed"],
                "decode_failure": counts["failed"],
                "duplicate_cluster_count": int(dedup_stats.duplicate_clusters),
                "unique_asset_count": int(dedup_stats.clusters),
                "candidate_pair_count": int(dedup_stats.candidate_pairs),
                "verified_pair_count": int(dedup_stats.verified_pairs),
                "rejected_pair_count": int(dedup_stats.rejected_pairs),
                "unique_independent_assets": int(dedup_stats.clusters),
            }
        )
        report = {
            "version": 1,
            "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z", time.localtime()),
            "root": str(self.root.resolve(strict=True)),
            "db": str(self.db_path),
            "jobs": self.jobs,
            "summary": summary,
            "groups": groups,
            "index": stats.as_dict(),
            "dedup": {
                **dedup_stats.as_dict(),
                "verification_cache_hits": int(dedup_stats.verification_cache_hits),
            },
            "truth_manifest": str(truth_manifest) if truth_manifest else None,
            "clusters": store.cluster_records(),
            "elapsed_seconds": round(time.time() - started, 3),
        }
        return report


def write_report(report: dict[str, Any], path: str | os.PathLike[str]) -> None:
    report_path = Path(path)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    temporary = report_path.with_name(f".{report_path.name}.{os.getpid()}.tmp")
    temporary.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=False) + "\n",
        encoding="utf-8",
    )
    os.replace(temporary, report_path)


def terminal_summary(report: dict[str, Any]) -> str:
    summary = report["summary"]
    return (
        f"素材总数={summary['total']} "
        f"解码成功={summary['success']} "
        f"解码失败={summary['failed']} "
        f"重复簇数={summary['duplicate_clusters']} "
        f"去重后独立素材={summary['unique_assets']} "
        f"候选对={summary['candidate_pairs']} "
        f"精验通过={summary['verified_pairs']} "
        f"否决={summary['rejected_pairs']} "
        f"冲突率={summary['conflict_rate'] * 100.0:.2f}%"
    )
