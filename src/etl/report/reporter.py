"""Run-metrics aggregation; writes run/report.json and terminal summaries."""
from __future__ import annotations

import json
import os
from collections import Counter

from ..transform.coerce import REASON_ORDER


class Reporter:
    def __init__(self):
        self.sources = {}
        self.plan_info = {}
        self.conflict_samples = {}
        self.load_stats = {}
        self.checksum = None
        self.quarantine_file = None
        self.null_counts = Counter()
        self.collector = None  # FailureCollector, attached by the CLI

    # ---- ingestion ------------------------------------------------------
    def note_plan(self, plan, target_fields: int, source_count: int) -> None:
        self.plan_info = {
            "target_fields": target_fields,
            "sources": source_count,
            "bindings": plan.bindings_total,
            "mapped": len(plan.mapped),
            "renamed": plan.renamed_total,
            "defaulted": len(plan.defaulted),
            "dropped": plan.dropped_total,
            "conflicts": len(plan.conflicts),
            "conflict_fields": {
                f: {s: b.declared_type for s, b in plan.fields[f].bindings.items()}
                for f in plan.conflicts
            },
            "defaulted_fields": list(plan.defaulted),
            "dropped_columns": plan.dropped,
        }

    def note_source(self, name: str, rows: int, elapsed: float) -> None:
        self.sources[name] = {"rows": rows, "elapsed_sec": round(elapsed, 3)}

    def note_conflict_sample(self, field: str, source: str, value: str) -> None:
        self.conflict_samples.setdefault(field, {}).setdefault(source, value)

    def note_nulls(self, counts) -> None:
        self.null_counts.update(counts)

    def note_load(self, stats: dict) -> None:
        self.load_stats = stats

    def note_checksum(self, checksum: str) -> None:
        self.checksum = checksum

    def note_quarantine(self, path: str) -> None:
        self.quarantine_file = path

    # ---- rollups ----------------------------------------------------------
    def reason_counts(self) -> dict:
        return self.collector.reason_counts() if self.collector else {}

    def top_reasons(self, n: int = 3):
        order = {r: i for i, r in enumerate(REASON_ORDER)}
        counts = self.reason_counts()
        return sorted(counts.items(), key=lambda kv: (-kv[1], order.get(kv[0], 99)))[:n]

    def build(self) -> dict:
        info = self.plan_info
        counts = self.reason_counts()
        return {
            "schema": {
                "target_fields": info.get("target_fields"),
                "sources": info.get("sources"),
                "bindings": info.get("bindings"),
            },
            "fields": {
                "mapped": info.get("mapped"),
                "renamed": info.get("renamed"),
                "defaulted": info.get("defaulted"),
                "dropped": info.get("dropped"),
                "conflicts": info.get("conflicts"),
                "defaulted_fields": info.get("defaulted_fields"),
                "dropped_columns": info.get("dropped_columns"),
            },
            "conflicts": {
                f: {
                    "declared_types": types,
                    "samples": self.conflict_samples.get(f, {}),
                }
                for f, types in info.get("conflict_fields", {}).items()
            },
            "conversion_failures": {
                "total": self.collector.failure_count if self.collector else 0,
                "rows": self.collector.row_count if self.collector else 0,
                "reasons": counts,
                "top_raw_values": {
                    r: self.collector.top_raw_values(r) for r in counts
                } if self.collector else {},
            },
            "nulls": dict(self.null_counts),
            "sources": self.sources,
            "load": self.load_stats,
            "checksum": self.checksum,
            "quarantine_file": self.quarantine_file,
        }

    def write_json(self, run_dir: str) -> str:
        path = os.path.join(run_dir, "report.json")
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(self.build(), fh, indent=2, ensure_ascii=False)
        return path

    def summary_lines(self) -> list:
        info = self.plan_info
        lines = [
            "SCHEMA target_fields={target_fields} sources={sources} "
            "bindings={bindings}".format(**info),
            "FIELDS mapped={mapped} renamed={renamed} defaulted={defaulted} "
            "dropped={dropped} conflicts={conflicts}".format(**info),
        ]
        failures = self.collector.failure_count if self.collector else 0
        rows = self.collector.row_count if self.collector else 0
        reasons = " ".join(f"{r}={n}" for r, n in self.top_reasons()) or "-"
        lines.append(f"CONVERT failures={failures} rows={rows} reasons {reasons}")
        load = self.load_stats
        lines.append(
            "LOAD rows_in={rows_in} unique={unique} dup_resolved={dup_resolved} "
            "loaded={loaded} rejected={rejected}".format(**load)
        )
        return lines
