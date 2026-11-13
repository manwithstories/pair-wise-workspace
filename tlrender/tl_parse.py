"""Parsers for the two in-house plain-text timeline formats.

v1 (legacy): milliseconds, cues nested inside shot nodes.
v2 (current):  flat tracks, frame numbers with a rational fps.

Structural errors reject the whole file (ParseError, carrying file + line).
Single-cue semantic errors are recorded as warnings and the cue is skipped.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

from . import MAX_CUE_MS
from .timebase import TimeBase, validate_ms_interval

VALID_TRANSITIONS = ("cut", "dissolve", "wipe")


class TimelineError(Exception):
    """Base class for file-level timeline failures."""


class TimelineMissing(TimelineError):
    exit_code = 1


class TimelineEncoding(TimelineError):
    exit_code = 1


class TimelineTooLarge(TimelineError):
    exit_code = 1


class CueTooLong(TimelineError):
    exit_code = 2


class ParseError(TimelineError):
    exit_code = 1

    def __init__(self, source: str, line: int, message: str):
        self.source = source
        self.line = line
        self.message = message
        super().__init__(f"{source}:{line}: {message}")


@dataclass
class Diagnostic:
    level: str
    source: str
    line: int
    code: str
    message: str


@dataclass
class Cue:
    id: str
    track: int
    start: int
    end: int
    text: str
    source: str
    line: int
    priority: int = 0
    start_frame: int | None = None
    end_frame: int | None = None
    fps: str | None = None
    resolved: int = 0
    moved: int = 0
    dropped: bool = False
    final_start: int | None = None
    final_end: int | None = None

    @property
    def duration(self) -> int:
        return self.end - self.start


@dataclass
class Transition:
    kind: str
    start: int
    end: int
    source: str
    line: int


@dataclass
class ParseResult:
    source: str
    version: str
    cues: list[Cue] = field(default_factory=list)
    transitions: list[Transition] = field(default_factory=list)
    diagnostics: list[Diagnostic] = field(default_factory=list)
    fps: str | None = None

    @property
    def warnings(self) -> list[Diagnostic]:
        return [d for d in self.diagnostics if d.level == "warning"]


def load_timeline(path: str | Path) -> ParseResult:
    path = Path(path)
    if not path.exists():
        raise TimelineMissing(f"timeline not found: {path}")
    size = path.stat().st_size
    from . import MAX_FILE_BYTES

    if size > MAX_FILE_BYTES:
        raise TimelineTooLarge(
            f"timeline {path} is {size} bytes, limit is {MAX_FILE_BYTES}"
        )
    raw = path.read_bytes()
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise TimelineEncoding(f"timeline {path} is not valid UTF-8: {exc}") from exc
    return parse_text(text, str(path))


def parse_text(text: str, source: str = "<memory>") -> ParseResult:
    lines = text.splitlines()
    header_line, header = _first_content(lines)
    if header is None:
        raise ParseError(source, 1, "empty timeline, expected '@timeline' header")
    if len(header) < 2 or header[0] != "@timeline":
        raise ParseError(
            source, header_line, "first statement must be '@timeline v1' or v2"
        )
    version = header[1]
    if version == "v1":
        return _parse_v1(lines, source)
    if version == "v2":
        return _parse_v2(lines, source)
    raise ParseError(source, header_line, f"unknown timeline version {version!r}")


def _first_content(lines: list[str]) -> tuple[int, list[str] | None]:
    for idx, raw in enumerate(lines, start=1):
        tokens = _tokenize(raw)
        if tokens is not None:
            return idx, tokens
    return 1, None


def _tokenize(raw_line: str) -> list[str] | None:
    """Split on whitespace honoring double quotes; None for blank/comment."""
    line = raw_line.strip()
    if not line or line.startswith("#"):
        return None
    tokens: list[str] = []
    buf: list[str] = []
    quoted = False
    for ch in line:
        if ch == '"':
            quoted = not quoted
        elif ch.isspace() and not quoted:
            if buf:
                tokens.append("".join(buf))
                buf = []
        else:
            buf.append(ch)
    if quoted:
        raise ValueError("unterminated quoted string")
    if buf:
        tokens.append("".join(buf))
    return tokens


def _parse_int(source: str, line: int, token: str, what: str) -> int:
    try:
        return int(token)
    except ValueError as exc:
        raise ParseError(source, line, f"{what} must be an integer, got {token!r}") from exc


def _check_cue_interval(
    result: ParseResult,
    cue: Cue,
) -> bool:
    """Return True when the cue is kept; otherwise record a warning."""
    try:
        validate_ms_interval(cue.start, cue.end)
    except ValueError as exc:
        result.diagnostics.append(
            Diagnostic("warning", cue.source, cue.line, "invalid_interval", str(exc))
        )
        return False
    if cue.end - cue.start > MAX_CUE_MS:
        result.diagnostics.append(
            Diagnostic(
                "warning",
                cue.source,
                cue.line,
                "cue_too_long",
                f"cue {cue.id} lasts {cue.end - cue.start} ms, limit is {MAX_CUE_MS}",
            )
        )
        return False
    return True


def _parse_cue_tail(
    source: str, line: int, tokens: list[str]
) -> tuple[int, str]:
    priority = 0
    rest = tokens
    if rest and rest[0] == "pri":
        if len(rest) < 3:
            raise ParseError(source, line, "cue 'pri' needs an integer and text")
        priority = _parse_int(source, line, rest[1], "priority")
        rest = rest[2:]
    if len(rest) != 1:
        raise ParseError(source, line, "cue requires exactly one quoted text token")
    return priority, rest[0]


def _parse_transition(
    source: str, line: int, kind: str, start: int, end: int
) -> Transition:
    if kind not in VALID_TRANSITIONS:
        raise ParseError(
            source, line, f"unknown transition kind {kind!r}, want one of {VALID_TRANSITIONS}"
        )
    if start < 0 or end < 0:
        raise ParseError(source, line, "transition times must be non-negative")
    if kind == "cut":
        if end != start:
            raise ParseError(source, line, "cut transition must be a zero-width point")
    elif end <= start:
        raise ParseError(source, line, f"{kind} transition window must have positive width")
    return Transition(kind, start, end, source, line)


def _parse_v1(lines: list[str], source: str) -> ParseResult:
    result = ParseResult(source=source, version="v1")
    seen_ids: set[str] = set()
    current_shot: str | None = None
    for lineno, raw in enumerate(lines, start=1):
        try:
            tokens = _tokenize(raw)
        except ValueError as exc:
            raise ParseError(source, lineno, str(exc)) from exc
        if tokens is None:
            continue
        head, args = tokens[0], tokens[1:]
        if head == "@timeline":
            if args != ["v1"]:
                raise ParseError(source, lineno, "malformed v1 header")
            continue
        if head in ("head",):
            if len(args) != 1:
                raise ParseError(source, lineno, "head requires one quoted title")
            continue
        if head == "shot":
            if len(args) != 3:
                raise ParseError(source, lineno, "usage: shot ID START_MS END_MS")
            current_shot = args[0]
            shot_start = _parse_int(source, lineno, args[1], "shot start")
            shot_end = _parse_int(source, lineno, args[2], "shot end")
            if shot_end <= shot_start:
                raise ParseError(source, lineno, "shot must have positive duration")
            continue
        if head == "cue":
            if current_shot is None:
                raise ParseError(source, lineno, "cue outside of a shot node")
            if len(args) < 4:
                raise ParseError(
                    source, lineno, "usage: cue ID START_MS END_MS [pri N] \"text\""
                )
            cue_id, start_s, end_s = args[0], args[1], args[2]
            priority, text = _parse_cue_tail(source, lineno, args[3:])
            start = _parse_int(source, lineno, start_s, "cue start")
            end = _parse_int(source, lineno, end_s, "cue end")
            if cue_id in seen_ids:
                raise ParseError(source, lineno, f"duplicate cue id {cue_id!r}")
            seen_ids.add(cue_id)
            cue = Cue(
                id=cue_id,
                track=3,
                start=start,
                end=end,
                text=text,
                source=source,
                line=lineno,
                priority=priority,
            )
            if _check_cue_interval(result, cue):
                result.cues.append(cue)
            continue
        if head == "transition":
            if len(args) != 3:
                raise ParseError(
                    source, lineno, "usage: transition KIND START_MS END_MS"
                )
            kind, start_s, end_s = args
            start = _parse_int(source, lineno, start_s, "transition start")
            end = _parse_int(source, lineno, end_s, "transition end")
            result.transitions.append(
                _parse_transition(source, lineno, kind, start, end)
            )
            continue
        if head in ("@end",):
            continue
        raise ParseError(source, lineno, f"unknown v1 statement {head!r}")
    if not any(_tokenize(raw) and _tokenize(raw)[0] == "shot" for raw in lines):
        raise ParseError(source, 1, "v1 timeline contains no shot nodes")
    return result


def _parse_v2(lines: list[str], source: str) -> ParseResult:
    result = ParseResult(source=source, version="v2")
    timebase: TimeBase | None = None
    current_track: int | None = None
    seen_ids: set[str] = set()
    for lineno, raw in enumerate(lines, start=1):
        try:
            tokens = _tokenize(raw)
        except ValueError as exc:
            raise ParseError(source, lineno, str(exc)) from exc
        if tokens is None:
            continue
        head, args = tokens[0], tokens[1:]
        if head == "@timeline":
            if args != ["v2"]:
                raise ParseError(source, lineno, "malformed v2 header")
            continue
        if head == "fps":
            if len(args) != 1 or timebase is not None:
                raise ParseError(source, lineno, "fps must be declared exactly once")
            try:
                timebase = TimeBase.parse(args[0])
            except ValueError as exc:
                raise ParseError(source, lineno, str(exc)) from exc
            result.fps = timebase.fps_text
            continue
        if head == "track":
            if len(args) != 1:
                raise ParseError(source, lineno, "usage: track NUMBER")
            current_track = _parse_int(source, lineno, args[0], "track number")
            if current_track < 1:
                raise ParseError(source, lineno, "track number must be >= 1")
            continue
        if head == "cue":
            if timebase is None:
                raise ParseError(source, lineno, "cue before fps declaration")
            if current_track is None:
                raise ParseError(source, lineno, "cue before any track declaration")
            if len(args) < 4:
                raise ParseError(
                    source, lineno, "usage: cue ID START_FRAME END_FRAME [pri N] \"text\""
                )
            cue_id, start_s, end_s = args[0], args[1], args[2]
            priority, text = _parse_cue_tail(source, lineno, args[3:])
            start_frame = _parse_int(source, lineno, start_s, "start frame")
            end_frame = _parse_int(source, lineno, end_s, "end frame")
            if cue_id in seen_ids:
                raise ParseError(source, lineno, f"duplicate cue id {cue_id!r}")
            seen_ids.add(cue_id)
            try:
                start_ms, end_ms = timebase.frame_span_to_ms(start_frame, end_frame)
            except ValueError as exc:
                result.diagnostics.append(
                    Diagnostic("warning", source, lineno, "invalid_interval", str(exc))
                )
                continue
            cue = Cue(
                id=cue_id,
                track=current_track,
                start=start_ms,
                end=end_ms,
                text=text,
                source=source,
                line=lineno,
                priority=priority,
                start_frame=start_frame,
                end_frame=end_frame,
                fps=timebase.fps_text,
            )
            if _check_cue_interval(result, cue):
                result.cues.append(cue)
            continue
        if head == "transition":
            if timebase is None:
                raise ParseError(source, lineno, "transition before fps declaration")
            if len(args) != 3:
                raise ParseError(
                    source, lineno, "usage: transition KIND START_FRAME END_FRAME"
                )
            kind, start_s, end_s = args
            start_frame = _parse_int(source, lineno, start_s, "transition start frame")
            end_frame = _parse_int(source, lineno, end_s, "transition end frame")
            if start_frame < 0 or end_frame < 0:
                raise ParseError(source, lineno, "transition frames must be non-negative")
            start_ms = timebase.frame_to_ms(start_frame)
            end_ms = timebase.frame_to_ms(end_frame) if kind != "cut" else start_ms
            result.transitions.append(
                _parse_transition(source, lineno, kind, start_ms, end_ms)
            )
            continue
        if head == "@end":
            continue
        raise ParseError(source, lineno, f"unknown v2 statement {head!r}")
    if timebase is None:
        raise ParseError(source, 1, "v2 timeline is missing an fps statement")
    return result
