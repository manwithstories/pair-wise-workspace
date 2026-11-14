from __future__ import annotations

import os
from pathlib import Path

import click

from .checks import check_paths
from .errors import EXIT_BLOCKING, EXIT_IO, MediaError
from .index import IndexDB
from .packager import pack as pack_assets
from .probe import probe_file
from .report import print_check, print_issues, print_pack, print_probe, write_json
from .samplegen import generate_samples


def _fail(ctx: click.Context, exc: MediaError) -> None:
    click.echo(str(exc), err=True)
    ctx.exit(exc.exit_code)


@click.group(context_settings={"help_option_names": ["-h", "--help"]})
@click.version_option("0.1.0", prog_name="mediaprobe")
def cli() -> None:
    """Bounded, offline media delivery probe."""


@cli.command()
@click.argument("path", type=click.Path(exists=True, dir_okay=False, path_type=Path))
@click.option("--json", "json_out", type=click.Path(dir_okay=False, path_type=Path))
@click.pass_context
def probe(ctx: click.Context, path: Path, json_out: Path | None) -> None:
    """Inspect one media file and print its tracks."""

    try:
        asset = probe_file(path)
        payload = asset.to_dict()
        print_probe(asset)
        if json_out is not None:
            write_json(json_out, payload)
    except MediaError as exc:
        _fail(ctx, exc)
        return
    ctx.exit(0)


@cli.command()
@click.argument("path", type=click.Path(exists=True, path_type=Path))
@click.option(
    "--profile",
    "profile_path",
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    default=Path("profiles/delivery.json"),
    show_default=True,
)
@click.option("--tolerance-ms", type=click.IntRange(min=0), default=40, show_default=True)
@click.option("--json", "json_out", type=click.Path(dir_okay=False, path_type=Path))
@click.pass_context
def check(
    ctx: click.Context,
    path: Path,
    profile_path: Path,
    tolerance_ms: int,
    json_out: Path | None,
) -> None:
    """Check a file or directory against a delivery profile."""

    try:
        report = check_paths(path, profile_path, tolerance_ms)
        payload = report.to_dict()
        print_check(report)
        if json_out is not None:
            write_json(json_out, payload)
    except MediaError as exc:
        _fail(ctx, exc)
        return
    ctx.exit(EXIT_BLOCKING if report.blocking_issues else 0)


@cli.command()
@click.argument("source", type=click.Path(exists=True, path_type=Path))
@click.option(
    "--profile",
    "profile_path",
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    default=Path("profiles/delivery.json"),
    show_default=True,
)
@click.option("--bundle", type=click.Path(file_okay=False, path_type=Path), required=True)
@click.option("--db", "db_path", type=click.Path(dir_okay=False, path_type=Path), required=True)
@click.option("--tolerance-ms", type=click.IntRange(min=0), default=40, show_default=True)
@click.option("--json", "json_out", type=click.Path(dir_okay=False, path_type=Path))
@click.pass_context
def pack(
    ctx: click.Context,
    source: Path,
    profile_path: Path,
    bundle: Path,
    db_path: Path,
    tolerance_ms: int,
    json_out: Path | None,
) -> None:
    """Copy accepted assets, slice requested audio, and write an audit manifest."""

    try:
        result = pack_assets(source, profile_path, bundle, db_path, tolerance_ms)
        if json_out is not None:
            write_json(json_out, result)
        print_pack(result)
    except MediaError as exc:
        _fail(ctx, exc)
        return
    ctx.exit(EXIT_BLOCKING if result["rejected"] else 0)


@cli.group()
def index() -> None:
    """Query the SQLite audit index."""


@index.command("issues")
@click.option("--db", "db_path", type=click.Path(exists=True, dir_okay=False, path_type=Path), required=True)
@click.option("--severity", type=click.Choice(["blocking", "info", "all"]), default="all", show_default=True)
@click.option("--json", "json_out", type=click.Path(dir_okay=False, path_type=Path))
@click.pass_context
def index_issues(ctx: click.Context, db_path: Path, severity: str, json_out: Path | None) -> None:
    """List indexed issues ordered by asset path and rule."""

    query_severity = None if severity == "all" else severity
    try:
        with IndexDB(db_path) as database:
            rows = database.query_issues(query_severity)
        print_issues(rows)
        if json_out is not None:
            write_json(json_out, {"severity": severity, "issue_count": len(rows), "issues": rows})
    except MediaError as exc:
        _fail(ctx, exc)
        return
    has_blocking = any(row["severity"] == "blocking" for row in rows)
    ctx.exit(EXIT_BLOCKING if has_blocking else 0)


@cli.command()
@click.option("--out", type=click.Path(file_okay=False, path_type=Path), default=Path("fixtures"), show_default=True)
@click.option("--count", type=click.IntRange(min=0), default=14, show_default=True)
@click.option("--seed", type=int, default=7, show_default=True)
@click.pass_context
def gen(ctx: click.Context, out: Path, count: int, seed: int) -> None:
    """Generate deterministic sample media."""

    try:
        paths = generate_samples(out, count, seed)
    except (OSError, ValueError) as exc:
        _fail(ctx, MediaError("IO_ERROR", str(exc), path=os.fspath(out), exit_code=EXIT_IO))
        return
    click.echo(f"generated={len(paths)} out={out} seed={seed}")
    ctx.exit(0)


def main() -> None:
    try:
        cli()
    except MediaError as exc:
        click.echo(str(exc), err=True)
        raise SystemExit(exc.exit_code) from exc


if __name__ == "__main__":
    main()
