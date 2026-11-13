"""Banded LSH candidates, verification, DSU clustering and cluster splitting."""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from mediaidx.fingerprint import FINGERPRINT_VERSION, verify_fingerprints
from mediaidx.store import Store, StoreError


class DisjointSet:
    def __init__(self, members: Iterable[int]) -> None:
        self.parent = {int(member): int(member) for member in members}
        self.rank = {int(member): 0 for member in self.parent}

    def find(self, item: int) -> int:
        parent = self.parent[item]
        if parent != item:
            self.parent[item] = self.find(parent)
        return self.parent[item]

    def union(self, left: int, right: int) -> bool:
        root_left = self.find(left)
        root_right = self.find(right)
        if root_left == root_right:
            return False
        if self.rank[root_left] < self.rank[root_right]:
            root_left, root_right = root_right, root_left
        self.parent[root_right] = root_left
        if self.rank[root_left] == self.rank[root_right]:
            self.rank[root_left] += 1
        return True


@dataclass
class DedupStats:
    candidate_pairs: int = 0
    verified_pairs: int = 0
    rejected_pairs: int = 0
    verification_cache_hits: int = 0
    verification_errors: int = 0
    clusters: int = 0
    duplicate_clusters: int = 0
    clustered_members: int = 0
    group_details: dict[str, dict[str, int]] = field(default_factory=dict)
    conflict_pairs: int = 0
    truth_pairs_evaluated: int = 0
    conflict_rate: float = 0.0
    truth_available: bool = False
    elapsed_seconds: float = 0.0

    def as_dict(self) -> dict[str, Any]:
        return {
            "candidate_pairs": self.candidate_pairs,
            "verified_pairs": self.verified_pairs,
            "rejected_pairs": self.rejected_pairs,
            "verification_cache_hits": self.verification_cache_hits,
            "verification_errors": self.verification_errors,
            "clusters": self.clusters,
            "duplicate_clusters": self.duplicate_clusters,
            "clustered_members": self.clustered_members,
            "conflict_pairs": self.conflict_pairs,
            "truth_pairs_evaluated": self.truth_pairs_evaluated,
            "conflict_rate": self.conflict_rate,
            "truth_available": self.truth_available,
            "groups": self.group_details,
            "elapsed_seconds": self.elapsed_seconds,
        }


def _compact_detail(detail: dict[str, Any]) -> dict[str, Any]:
    compact = {key: value for key, value in detail.items() if key != "pairs"}
    if "pairs" in detail:
        compact["paired_frames"] = len(detail["pairs"])
    return compact


class Deduper:
    def __init__(self, store: Store) -> None:
        self.store = store

    def run(self, truth_labels: dict[str, str] | None = None) -> DedupStats:
        started = time.time()
        files = self.store.successful_files()
        rows_by_id = {int(row["id"]): row for row in files}
        payload_cache: dict[int, dict[str, Any]] = {}

        self.store.rebuild_candidate_tables()
        candidates = self.store.candidate_pairs()
        verified_rows: list[tuple[int, int, str, float, dict[str, Any]]] = []
        stats = DedupStats(candidate_pairs=len(candidates))
        candidate_by_family: dict[str, int] = {}
        verified_by_family: dict[str, int] = {}
        rejected_by_family: dict[str, int] = {}

        for candidate in candidates:
            left_id = int(candidate["pair_a"])
            right_id = int(candidate["pair_b"])
            family = str(candidate["family"])
            candidate_by_family[family] = candidate_by_family.get(family, 0) + 1
            left_row = rows_by_id.get(left_id)
            right_row = rows_by_id.get(right_id)
            if left_row is None or right_row is None:
                stats.rejected_pairs += 1
                rejected_by_family[family] = rejected_by_family.get(family, 0) + 1
                continue
            cache_row = self.store.get_verification_cache(
                str(left_row["content_hash"]),
                str(right_row["content_hash"]),
                family,
                FINGERPRINT_VERSION,
            )
            if cache_row is not None:
                same = bool(cache_row["same"])
                score = float(cache_row["score"])
                detail = json.loads(cache_row["detail"] or "{}")
                stats.verification_cache_hits += 1
            else:
                try:
                    if left_id not in payload_cache:
                        payload_cache[left_id] = self.store.load_signature_payload(left_row)
                    if right_id not in payload_cache:
                        payload_cache[right_id] = self.store.load_signature_payload(right_row)
                    same, detail = verify_fingerprints(
                        family, payload_cache[left_id], payload_cache[right_id]
                    )
                    score = float(detail.get("score", 0.0))
                    self.store.put_verification_cache(
                        str(left_row["content_hash"]),
                        str(right_row["content_hash"]),
                        family,
                        FINGERPRINT_VERSION,
                        same,
                        score,
                        _compact_detail(detail),
                    )
                except (StoreError, ValueError, KeyError, TypeError) as exc:
                    stats.verification_errors += 1
                    stats.rejected_pairs += 1
                    same = False
                    score = 0.0
                    detail = {"decision": "different", "error": str(exc)}

            if same:
                stats.verified_pairs += 1
                verified_by_family[family] = verified_by_family.get(family, 0) + 1
                verified_rows.append((left_id, right_id, family, score, _compact_detail(detail)))
            else:
                stats.rejected_pairs += 1
                rejected_by_family[family] = rejected_by_family.get(family, 0) + 1

        self.store.replace_verified_pairs(verified_rows)

        dsu = DisjointSet(rows_by_id)
        for left_id, right_id, _family, _score, _detail in verified_rows:
            dsu.union(left_id, right_id)

        groups: dict[int, list[Any]] = {}
        for file_id, row in rows_by_id.items():
            root = dsu.find(file_id)
            groups.setdefault(root, []).append(row)

        clusters: list[dict[str, Any]] = []
        for members in groups.values():
            ordered = sorted(members, key=lambda row: str(row["rel_path"]))
            family = str(ordered[0]["family"])
            cluster_id = min(str(row["content_hash"]) for row in ordered)
            member_ids = [int(row["id"]) for row in ordered]
            clusters.append(
                {
                    "cluster_id": cluster_id,
                    "family": family,
                    "member_ids": member_ids,
                    "paths": sorted(str(row["rel_path"]) for row in ordered),
                }
            )
        clusters.sort(key=lambda item: (item["family"], item["cluster_id"]))
        self.store.write_clusters(clusters)

        stats.clusters = len(clusters)
        stats.duplicate_clusters = sum(1 for cluster in clusters if len(cluster["member_ids"]) > 1)
        stats.clustered_members = sum(len(cluster["member_ids"]) for cluster in clusters)

        family_stats: dict[str, dict[str, Any]] = {}
        for cluster in clusters:
            family = cluster["family"]
            family_stats.setdefault(
                family,
                {
                    "clusters": 0,
                    "duplicate_clusters": 0,
                    "unique_assets": 0,
                    "candidate_pairs": 0,
                    "verified_pairs": 0,
                    "rejected_pairs": 0,
                    "conflict_pairs": 0,
                    "truth_pairs_evaluated": 0,
                    "conflict_rate": 0.0,
                },
            )
            family_stats[family]["clusters"] += 1
            family_stats[family]["unique_assets"] += 1
            if len(cluster["member_ids"]) > 1:
                family_stats[family]["duplicate_clusters"] += 1
        for family, count in candidate_by_family.items():
            family_stats.setdefault(
                family,
                {
                    "clusters": 0,
                    "duplicate_clusters": 0,
                    "unique_assets": 0,
                    "candidate_pairs": 0,
                    "verified_pairs": 0,
                    "rejected_pairs": 0,
                    "conflict_pairs": 0,
                    "truth_pairs_evaluated": 0,
                    "conflict_rate": 0.0,
                },
            )
            family_stats[family]["candidate_pairs"] = int(count)
            family_stats[family]["verified_pairs"] = int(verified_by_family.get(family, 0))
            family_stats[family]["rejected_pairs"] = int(rejected_by_family.get(family, 0))
        stats.group_details = family_stats

        if truth_labels is not None:
            stats.truth_available = True
            conflicts_by_family: dict[str, int] = {}
            evaluated_by_family: dict[str, int] = {}
            for left_id, right_id, family, _score, _detail in verified_rows:
                left_row = self.store.file_by_id(left_id)
                right_row = self.store.file_by_id(right_id)
                if left_row is None or right_row is None:
                    continue
                left_label = truth_labels.get(str(left_row["rel_path"]))
                right_label = truth_labels.get(str(right_row["rel_path"]))
                if left_label is None or right_label is None:
                    continue
                evaluated_by_family[family] = evaluated_by_family.get(family, 0) + 1
                if left_label != right_label:
                    conflicts_by_family[family] = conflicts_by_family.get(family, 0) + 1
            stats.conflict_pairs = sum(conflicts_by_family.values())
            stats.truth_pairs_evaluated = sum(evaluated_by_family.values())
            if stats.verified_pairs:
                stats.conflict_rate = stats.conflict_pairs / stats.verified_pairs
            for family, detail in family_stats.items():
                detail["conflict_pairs"] = int(conflicts_by_family.get(family, 0))
                detail["truth_pairs_evaluated"] = int(evaluated_by_family.get(family, 0))
                verified = int(detail.get("verified_pairs", 0))
                detail["conflict_rate"] = (
                    float(detail["conflict_pairs"]) / verified if verified else 0.0
                )
        stats.elapsed_seconds = time.time() - started
        return stats



def load_truth_labels(path: str | Path | None) -> dict[str, str] | None:
    if path is None:
        return None
    manifest_path = Path(path)
    if not manifest_path.is_file():
        return None
    try:
        data = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    labels: dict[str, str] = {}
    if isinstance(data, dict):
        assets = data.get("assets", data.get("groups"))
        if isinstance(assets, dict):
            for asset_id, members in assets.items():
                if isinstance(members, list):
                    for member in members:
                        labels[str(member)] = str(asset_id)
        elif isinstance(assets, list):
            for index, members in enumerate(assets):
                if isinstance(members, list):
                    for member in members:
                        labels[str(member)] = f"asset_{index:04d}"
        mapping = data.get("files", data.get("labels"))
        if isinstance(mapping, dict):
            for member, asset_id in mapping.items():
                labels[str(member)] = str(asset_id)
    elif isinstance(data, list):
        for index, members in enumerate(data):
            if isinstance(members, list):
                for member in members:
                    labels[str(member)] = f"asset_{index:04d}"
    return labels or None


def discover_truth_manifest(root: str | Path, explicit: str | Path | None = None) -> Path | None:
    if explicit is not None:
        candidate = Path(explicit)
        return candidate if candidate.is_file() else None
    root_path = Path(root)
    candidates = [
        root_path.parent / f"{root_path.name}.labels.json",
        root_path.parent / f"{root_path.name}.truth.json",
        root_path / ".labels.json",
        root_path / ".truth.json",
    ]
    for candidate in candidates:
        if candidate.is_file():
            return candidate
    return None
