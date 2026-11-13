"""Timezone resolution and NTP-based clock correction."""

from __future__ import annotations

import io
import json
import struct
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo

from .format import ASSET_MAINTENANCE, ASSET_NTP_LOG, ASSET_TZ_RULES
from .imageio import ImageIO


UTC_MIN = datetime.min.replace(tzinfo=timezone.utc)
UTC_MAX = datetime.max.replace(tzinfo=timezone.utc)


class TimebaseError(ValueError):
    """Raised when timezone or NTP assets are malformed."""


@dataclass(frozen=True)
class Transition:
    utc: datetime
    offset_before: int
    offset_after: int
    abbreviation_before: str
    abbreviation_after: str
    is_dst_after: bool = True


@dataclass(frozen=True)
class TimeZoneRules:
    zone_name: str
    base_offset: int
    base_abbreviation: str
    transitions: tuple[Transition, ...]


@dataclass(frozen=True)
class NtpSample:
    sequence: int
    reference_utc: datetime
    observed_local: str
    mode: str
    offset_seconds: float


@dataclass(frozen=True)
class NormalizedTime:
    utc: datetime
    wall: datetime
    offset_seconds: int | None
    correction_seconds: float
    dst_kind: str
    source_kind: str

    @property
    def anomalous(self) -> bool:
        return abs(self.correction_seconds) > 300.0


def _parse_utc(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def load_timezone_rules(image: ImageIO) -> TimeZoneRules:
    raw = image.read_asset(ASSET_TZ_RULES, zero_fill=True)
    text = raw.split(b"\0", 1)[0].decode("utf-8")
    try:
        value = json.loads(text)
        transitions = tuple(
            Transition(
                utc=_parse_utc(item["utc"]),
                offset_before=int(item["offset_before"]),
                offset_after=int(item["offset_after"]),
                abbreviation_before=str(item["abbreviation_before"]),
                abbreviation_after=str(item["abbreviation_after"]),
                is_dst_after=bool(item.get("is_dst_after", True)),
            )
            for item in value["transitions"]
        )
        if any(
            transitions[index].utc >= transitions[index + 1].utc
            for index in range(len(transitions) - 1)
        ):
            raise TimebaseError("timezone transitions are not strictly ordered")
        return TimeZoneRules(
            zone_name=str(value["zone"]),
            base_offset=int(value["base_offset"]),
            base_abbreviation=str(value.get("base_abbreviation", "LST")),
            transitions=transitions,
        )
    except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
        raise TimebaseError(f"invalid timezone rules: {exc}") from exc


def load_ntp_samples(image: ImageIO) -> list[NtpSample]:
    raw = image.read_asset(ASSET_NTP_LOG, zero_fill=True)
    text = raw.split(b"\0", 1)[0].decode("utf-8")
    samples = []
    for line_number, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            continue
        try:
            value = json.loads(line)
            samples.append(
                NtpSample(
                    sequence=int(value["seq"]),
                    reference_utc=_parse_utc(value["reference_utc"]),
                    observed_local=str(value["observed_local"]),
                    mode=str(value["mode"]),
                    offset_seconds=float(value["offset_s"]),
                )
            )
        except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
            raise TimebaseError(f"invalid NTP log line {line_number}: {exc}") from exc
    if not samples:
        raise TimebaseError("NTP log is empty")
    return sorted(samples, key=lambda item: (item.reference_utc, item.sequence))


def load_maintenance_windows(image: ImageIO) -> list[tuple[datetime, datetime]]:
    raw = image.read_asset(ASSET_MAINTENANCE, zero_fill=True)
    text = raw.split(b"\0", 1)[0].decode("utf-8")
    try:
        value = json.loads(text)
        windows = []
        for item in value["windows"]:
            start = _parse_utc(item["start"])
            end = _parse_utc(item["end"])
            if end <= start:
                raise TimebaseError("maintenance window end must follow its start")
            windows.append((start, end))
        return sorted(windows)
    except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
        raise TimebaseError(f"invalid maintenance windows: {exc}") from exc


def _build_tzif(rules: TimeZoneRules) -> bytes:
    types: list[tuple[int, bool, str]] = [
        (rules.base_offset, False, rules.base_abbreviation)
    ]
    transition_types = []
    for transition in rules.transitions:
        type_tuple = (
            transition.offset_after,
            transition.is_dst_after,
            transition.abbreviation_after,
        )
        if type_tuple not in types:
            types.append(type_tuple)
        transition_types.append(types.index(type_tuple))

    abbreviations = b""
    abbreviation_index = {}
    for _, _, abbreviation in types:
        abbreviation_index[abbreviation] = len(abbreviations)
        abbreviations += abbreviation.encode("ascii") + b"\0"
    if len(abbreviations) > 255:
        raise TimebaseError("timezone abbreviations are too large for TZif")

    transition_times = [
        int((item.utc - datetime(1970, 1, 1, tzinfo=timezone.utc)).total_seconds())
        for item in rules.transitions
    ]

    def encode(version: bytes, wide: bool) -> bytes:
        fmt = ">q" if wide else ">i"
        header = (
            b"TZif"
            + version
            + b"\0" * 15
            + struct.pack(
                ">IIIIII",
                0,
                0,
                0,
                len(transition_times),
                len(types),
                len(abbreviations),
            )
        )
        transitions = b"".join(struct.pack(fmt, value) for value in transition_times)
        indexes = bytes(transition_types)
        ttinfo = b"".join(
            struct.pack(
                ">iBB",
                offset,
                1 if is_dst else 0,
                abbreviation_index[abbreviation],
            )
            for offset, is_dst, abbreviation in types
        )
        return header + transitions + indexes + ttinfo + abbreviations

    return encode(b"2", False) + encode(b"2", True) + b"\n\n"


def build_zone(rules: TimeZoneRules) -> ZoneInfo:
    try:
        return ZoneInfo.from_file(io.BytesIO(_build_tzif(rules)), key=rules.zone_name)
    except Exception as exc:
        raise TimebaseError(f"could not construct image timezone: {exc}") from exc


class TimeBase:
    def __init__(self, rules: TimeZoneRules, samples: list[NtpSample]) -> None:
        self.rules = rules
        self.zone = build_zone(rules)
        self.samples = samples
        self.step_samples = [item for item in samples if item.mode == "step"]
        if not self.step_samples:
            self.step_samples = [samples[0]]
        self.drift_samples = [
            item for item in samples if item.mode in ("drift_start", "drift_end")
        ]

    @classmethod
    def from_image(cls, image: ImageIO) -> "TimeBase":
        return cls(load_timezone_rules(image), load_ntp_samples(image))

    def _is_nonexistent(self, wall: datetime) -> bool:
        fold0 = wall.replace(tzinfo=self.zone, fold=0)
        fold1 = wall.replace(tzinfo=self.zone, fold=1)
        if fold0.utcoffset() == fold1.utcoffset():
            return False
        first = fold0.astimezone(timezone.utc).astimezone(self.zone).replace(tzinfo=None)
        second = fold1.astimezone(timezone.utc).astimezone(self.zone).replace(tzinfo=None)
        return first != wall and second != wall

    def _is_ambiguous(self, wall: datetime) -> bool:
        fold0 = wall.replace(tzinfo=self.zone, fold=0)
        fold1 = wall.replace(tzinfo=self.zone, fold=1)
        if fold0.utcoffset() == fold1.utcoffset():
            return False
        first = fold0.astimezone(timezone.utc).astimezone(self.zone).replace(tzinfo=None)
        second = fold1.astimezone(timezone.utc).astimezone(self.zone).replace(tzinfo=None)
        return first == wall and second == wall

    def wall_to_provisional_utc(self, wall: datetime) -> tuple[datetime, int, str]:
        if wall.tzinfo is not None:
            wall = wall.replace(tzinfo=None)
        aware = wall.replace(tzinfo=self.zone, fold=0)
        offset = int(aware.utcoffset().total_seconds())
        if self._is_nonexistent(wall):
            kind = "nonexistent"
            candidate = wall.replace(tzinfo=self.zone, fold=0)
            offset = int(candidate.utcoffset().total_seconds())
            return candidate.astimezone(timezone.utc), offset, kind
        if self._is_ambiguous(wall):
            kind = "ambiguous"
            candidate = wall.replace(tzinfo=self.zone, fold=0)
            return candidate.astimezone(timezone.utc), offset, kind
        return aware.astimezone(timezone.utc), offset, "normal"

    def correction_seconds(self, provisional_utc: datetime) -> float:
        steps = sorted(self.step_samples, key=lambda item: item.reference_utc)
        active = steps[0]
        for sample in steps:
            if sample.reference_utc <= provisional_utc:
                active = sample
            else:
                break
        next_step_time = UTC_MAX
        for sample in steps:
            if sample.reference_utc > active.reference_utc:
                next_step_time = sample.reference_utc
                break
        error = active.offset_seconds
        drifts = [
            sample
            for sample in self.drift_samples
            if active.reference_utc <= sample.reference_utc < next_step_time
        ]
        start = next((sample for sample in drifts if sample.mode == "drift_start"), None)
        end = next(
            (
                sample
                for sample in drifts
                if sample.mode == "drift_end"
                and start is not None
                and sample.reference_utc >= start.reference_utc
            ),
            None,
        )
        if start is not None and end is not None:
            if provisional_utc < start.reference_utc:
                error = active.offset_seconds
            elif provisional_utc >= end.reference_utc:
                error = end.offset_seconds
            else:
                span = (end.reference_utc - start.reference_utc).total_seconds()
                ratio = (provisional_utc - start.reference_utc).total_seconds() / span
                error = start.offset_seconds + ratio * (
                    end.offset_seconds - start.offset_seconds
                )
        return error

    def normalize_local(
        self,
        value: str | datetime,
        *,
        apply_clock: bool = True,
    ) -> NormalizedTime:
        if isinstance(value, str):
            try:
                parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
            except ValueError as exc:
                raise TimebaseError(f"invalid timestamp: {value}") from exc
        else:
            parsed = value
        if parsed.tzinfo is not None:
            utc = parsed.astimezone(timezone.utc)
            return NormalizedTime(
                utc=utc,
                wall=parsed.replace(tzinfo=None),
                offset_seconds=int(parsed.utcoffset().total_seconds()),
                correction_seconds=0.0,
                dst_kind="explicit",
                source_kind="offset",
            )
        provisional, offset, dst_kind = self.wall_to_provisional_utc(parsed)
        correction = self.correction_seconds(provisional) if apply_clock else 0.0
        utc = provisional - timedelta(seconds=correction)
        return NormalizedTime(
            utc=utc,
            wall=parsed,
            offset_seconds=offset,
            correction_seconds=correction,
            dst_kind=dst_kind,
            source_kind="naive" if apply_clock else "utc",
        )

    def normalize_utc(self, value: str | datetime) -> NormalizedTime:
        if isinstance(value, str):
            parsed = _parse_utc(value)
        elif value.tzinfo is None:
            parsed = value.replace(tzinfo=timezone.utc)
        else:
            parsed = value.astimezone(timezone.utc)
        wall = parsed.astimezone(self.zone).replace(tzinfo=None)
        return NormalizedTime(
            utc=parsed,
            wall=wall,
            offset_seconds=int(parsed.astimezone(self.zone).utcoffset().total_seconds()),
            correction_seconds=0.0,
            dst_kind="normal",
            source_kind="utc",
        )

    def timezone_gap_windows(self, padding: timedelta = timedelta(minutes=10)) -> list[tuple[datetime, datetime]]:
        windows = []
        for transition in self.rules.transitions:
            if transition.offset_after > transition.offset_before:
                windows.append(
                    (transition.utc - padding, transition.utc + padding)
                )
        return windows
