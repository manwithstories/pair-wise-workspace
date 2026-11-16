"""Export the repaired timeline as a CSV sidecar for the retrieval side."""

from __future__ import annotations

import csv
import os

from .repair import RepairResult

FIELDNAMES = ("frame_index", "pts", "dts", "repaired")


def export_csv(repair: RepairResult, path: str) -> str:
    """Write frame_index,pts,dts,repaired rows in display order."""
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(FIELDNAMES)
        for row in repair.rows:
            writer.writerow(
                (
                    row.frame_index,
                    row.pts if row.pts is not None else "",
                    row.dts if row.dts is not None else "",
                    1 if row.repaired else 0,
                )
            )
    return path
