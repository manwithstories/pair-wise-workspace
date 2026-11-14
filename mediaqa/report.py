"""Terminal summaries, JSON reports, exit-code computation."""

import json
import os

_SEVERITY_ORDER = {"error": 0, "warning": 1}

EXIT_OK = 0
EXIT_USAGE = 1
EXIT_WARNING = 2
EXIT_ERROR = 3
EXIT_PARSE = 4
EXIT_PACK_REJECTED = 5


def sort_findings(findings):
    return sorted(findings, key=lambda f: (
        _SEVERITY_ORDER.get(f.severity, 9), f.rule_id,
        f.track_index if f.track_index is not None else -1))


def exit_code_for(findings, parse_failed=False):
    if parse_failed:
        return EXIT_PARSE
    if any(f.severity == "error" for f in findings):
        return EXIT_ERROR
    if findings:
        return EXIT_WARNING
    return EXIT_OK


def counts(findings):
    return {sev: sum(1 for f in findings if f.severity == sev)
            for sev in ("error", "warning")}


def write_json(report, path):
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    with open(path, "w") as fh:
        json.dump(report, fh, indent=2, ensure_ascii=False)
        fh.write("\n")


def print_findings(findings):
    for f in sort_findings(findings):
        where = f.file_path or "-"
        track = "-" if f.track_index is None else str(f.track_index)
        print("  [%s] %s track=%s file=%s expected=%s actual=%s"
              % (f.severity, f.rule_id, track, where, f.expected, f.actual))
