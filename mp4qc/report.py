"""Terminal report rendering, JSON persistence, exit-code mapping."""
from __future__ import annotations

import json
import os
import sys
from dataclasses import dataclass, field

EXIT_OK = 0
EXIT_ERROR = 2
EXIT_UNPARSEABLE = 3
EXIT_RECHECK_FAILED = 4

LEVELS = ("info", "warn", "error")


@dataclass
class Report:
    command: str
    path: str = ""
    messages: list = field(default_factory=list)  # (level, text)
    data: dict = field(default_factory=dict)

    def add(self, level, text):
        assert level in LEVELS, level
        self.messages.append((level, text))

    @property
    def has_errors(self):
        return any(level == "error" for level, _ in self.messages)

    @property
    def has_warnings(self):
        return any(level == "warn" for level, _ in self.messages)

    @property
    def status(self):
        if self.has_errors:
            return "error"
        if self.has_warnings:
            return "warn"
        return "ok"


def to_dict(report):
    return {
        "command": report.command,
        "path": report.path,
        "status": report.status,
        "messages": [{"level": level, "text": text}
                     for level, text in report.messages],
        "data": report.data,
    }


def write_json(report, json_path):
    directory = os.path.dirname(os.path.abspath(json_path))
    os.makedirs(directory, exist_ok=True)
    with open(json_path, "w", encoding="utf-8") as fh:
        json.dump(to_dict(report), fh, indent=2, ensure_ascii=False)
        fh.write("\n")


def print_report(report, stream=None):
    stream = stream or sys.stdout
    for level, text in report.messages:
        print("[%s] %s" % (level.upper(), text), file=stream)
    for key, value in report.data.items():
        if isinstance(value, (list, dict)):
            print("%s: %s" % (key, json.dumps(value, ensure_ascii=False)),
                  file=stream)
        else:
            print("%s: %s" % (key, value), file=stream)
    print("status: %s" % report.status, file=stream)


def exit_code(report, strict=False):
    """0 = pass, 2 = error-level violations (warnings too when strict)."""
    if report.has_errors:
        return EXIT_ERROR
    if strict and report.has_warnings:
        return EXIT_ERROR
    return EXIT_OK

