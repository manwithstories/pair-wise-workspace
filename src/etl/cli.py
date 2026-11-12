"""Command line interface: align / verify / report.

Exit codes: 0 clean run, 1 config/source/fail-fast error,
2 quarantined rows, 3 unmerged keys.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time

import pandas as pd

from .config import ConfigError, load_config
from .connectors.registry import ConnectorRegistry
from .report.reporter import Reporter
from .schema.plan import build_plan
from .sink.sqlite_sink import SQLiteSink
from .transform.coerce import CoercionError, FailureCollector, coerce_batch


def _cmd_align(args) -> int:
    try:
        config = load_config(args.config)
    except ConfigError as exc:
        print(f"CONFIG_ERROR {exc}", file=sys.stderr)
        return 1
    policy = args.policy or config.failure_policy
    os.makedirs(config.run_dir, exist_ok=True)

    registry = ConnectorRegistry()
    reporter = Reporter()
    collector = FailureCollector()
    reporter.collector = collector
    sink = None
    try:
        connectors = {s.name: registry.get(s) for s in config.sources}
        schemas = {name: conn.schema() for name, conn in connectors.items()}
        plan = build_plan(config, schemas)
        reporter.note_plan(plan, len(config.target_schema), len(config.sources))

        sink = SQLiteSink(
            os.path.join(config.run_dir, "facts.db"),
            config.target_table,
            config.target_schema,
            plan.pk_field,
        )
        sink.begin()
        rows_in = 0
        for source in config.sources:
            conn = connectors[source.name]
            started = time.perf_counter()
            source_rows = 0
            first_batch = True
            for batch in conn.iter_batches(config.batch_size):
                source_rows += len(batch)
                if first_batch:
                    _sample_conflicts(plan, source.name, batch, reporter)
                    first_batch = False
                clean, null_counts = coerce_batch(
                    batch, source.name, plan, config.enums, collector, policy)
                reporter.note_nulls(null_counts)
                sink.write_batch(clean, source.name, source.priority)
                del batch, clean
            reporter.note_source(source.name, source_rows,
                                 time.perf_counter() - started)
            rows_in += source_rows
        stats = sink.merge()
        checksum = sink.checksum()
    except CoercionError as exc:
        print(f"CONVERT_ERROR {exc}", file=sys.stderr)
        return 1
    except (ValueError, OSError, KeyError) as exc:
        print(f"SOURCE_ERROR {exc}", file=sys.stderr)
        return 1
    finally:
        if sink is not None:
            sink.cleanup()
            sink.close()
        registry.close_all()

    load = {
        "rows_in": rows_in,
        "unique": rows_in - stats["dup_resolved"],
        "dup_resolved": stats["dup_resolved"],
        "unmerged": stats["unmerged"],
        "loaded": stats["loaded"],
        "rejected": collector.row_count,
    }
    reporter.note_load(load)
    reporter.note_checksum(checksum)
    if collector.records:
        quarantine = os.path.join(config.run_dir, "quarantine.csv")
        pd.DataFrame([vars(f) for f in collector.records]).to_csv(
            quarantine, index=False)
        reporter.note_quarantine(quarantine)
    report_path = reporter.write_json(config.run_dir)

    for line in reporter.summary_lines():
        print(line)
    print(f"CHECKSUM facts={checksum}")
    if reporter.quarantine_file:
        print(f"QUARANTINE file={reporter.quarantine_file}")
    print(f"REPORT file={report_path}")
    if stats["unmerged"]:
        print(f"UNMERGED keys={stats['unmerged']}")
        return 3
    if collector.records:
        return 2
    return 0


def _sample_conflicts(plan, source_name, batch, reporter) -> None:
    for field in plan.conflicts:
        binding = plan.fields[field].bindings.get(source_name)
        if binding is None or binding.column not in batch.columns:
            continue
        vals = batch[binding.column]
        vals = vals[vals.notna()]
        if len(vals):
            reporter.note_conflict_sample(field, source_name, str(vals.iloc[0]))


def _cmd_verify(args) -> int:
    try:
        config = load_config(args.config)
    except ConfigError as exc:
        print(f"CONFIG_ERROR {exc}", file=sys.stderr)
        return 1
    registry = ConnectorRegistry()
    if args.replay:
        conn = registry.replay_file(args.replay)
        rows = sum(len(b) for b in conn.iter_batches(config.batch_size))
        print(f"REPLAY file={args.replay} rows={rows} "
              f"columns={len(conn.schema())}")
        registry.close_all()
        return 0
    try:
        schemas = {s.name: registry.get(s).schema() for s in config.sources}
    except (ValueError, OSError) as exc:
        print(f"SOURCE_ERROR {exc}", file=sys.stderr)
        registry.close_all()
        return 1
    registry.close_all()
    plan = build_plan(config, schemas)
    print(f"VERIFY config=ok sources={len(config.sources)} "
          f"target_fields={len(config.target_schema)}")
    print(f"PLAN bindings={plan.bindings_total} mapped={len(plan.mapped)} "
          f"renamed={plan.renamed_total} defaulted={len(plan.defaulted)} "
          f"dropped={plan.dropped_total} conflicts={len(plan.conflicts)}")
    for field in plan.conflicts:
        types = sorted({b.declared_type
                        for b in plan.fields[field].bindings.values()})
        print(f"CONFLICT field={field} types={','.join(types)}")
    for field in plan.defaulted:
        print(f"DEFAULTED field={field}")
    for source, columns in plan.dropped.items():
        for column in columns:
            print(f"DROPPED source={source} column={column}")
    for source in config.sources:
        for target, binding in source.bindings.items():
            if binding.column not in schemas[source.name]:
                print(f"MISSING source={source.name} target={target} "
                      f"column={binding.column}")
    return 0


def _cmd_report(args) -> int:
    path = os.path.join(args.run_dir, "report.json")
    if not os.path.exists(path):
        print(f"REPORT_ERROR {path} not found", file=sys.stderr)
        return 1
    with open(path, encoding="utf-8") as fh:
        report = json.load(fh)
    schema, fields = report["schema"], report["fields"]
    conv, load = report["conversion_failures"], report["load"]
    print(f"SCHEMA target_fields={schema['target_fields']} "
          f"sources={schema['sources']} bindings={schema['bindings']}")
    print(f"FIELDS mapped={fields['mapped']} renamed={fields['renamed']} "
          f"defaulted={fields['defaulted']} dropped={fields['dropped']} "
          f"conflicts={fields['conflicts']}")
    reasons = " ".join(f"{r}={n}" for r, n in
                       sorted(conv["reasons"].items(), key=lambda kv: -kv[1])) or "-"
    print(f"CONVERT failures={conv['total']} rows={conv['rows']} reasons {reasons}")
    print(f"LOAD rows_in={load['rows_in']} unique={load['unique']} "
          f"dup_resolved={load['dup_resolved']} loaded={load['loaded']} "
          f"rejected={load['rejected']}")
    for name, info in report["sources"].items():
        print(f"SOURCE {name} rows={info['rows']} elapsed={info['elapsed_sec']}s")
    for field, detail in report["conflicts"].items():
        print(f"CONFLICT {field} types={detail['declared_types']} "
              f"samples={detail['samples']}")
    for reason, tops in report["conversion_failures"]["top_raw_values"].items():
        print(f"TOP_RAW {reason} " +
              " ".join(f"{v!r}x{n}" for v, n in tops))
    print(f"CHECKSUM facts={report['checksum']}")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="etl", description="Multi-source fact-table alignment pipeline")
    sub = parser.add_subparsers(dest="command", required=True)

    align = sub.add_parser("align", help="run the full pipeline")
    align.add_argument("--config", required=True)
    align.add_argument("--policy", choices=["quarantine", "fail-fast"],
                       default=None, help="override failure_policy")

    verify = sub.add_parser("verify", help="validate config and mapping plan")
    verify.add_argument("--config", required=True)
    verify.add_argument("--replay", default=None,
                        help="replay a quarantine CSV through the connector layer")

    report = sub.add_parser("report", help="print the last run report")
    report.add_argument("--run-dir", default="run")
    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    if args.command == "align":
        return _cmd_align(args)
    if args.command == "verify":
        return _cmd_verify(args)
    if args.command == "report":
        return _cmd_report(args)
    return 1


if __name__ == "__main__":
    sys.exit(main())
