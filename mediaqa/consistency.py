"""Rule engine: zero alignment, duration match, whitelist, language, rotation."""

from dataclasses import dataclass

AUDIO_TOLERANCE_US = 40_000
DEFAULT_FRAME_US = 33_333
WHITELIST = {"sample_rate": 48000, "channels": 2, "bits_per_sample": 16,
             "codec": "pcm"}


@dataclass
class Finding:
    rule_id: str
    severity: str
    expected: str
    actual: str
    track_index: int = None
    file_path: str = None
    message: str = ""

    def to_dict(self):
        return dict(self.__dict__)


def _fmt_us(value):
    return "%dus" % value


def evaluate(tracks, file_results=()):
    """Run all rules over the selected tracks; returns findings and metrics."""
    findings = []
    for result in file_results:
        for warn in result.warnings:
            findings.append(Finding(
                warn["rule_id"], warn["severity"], warn["expected"],
                warn["actual"], None, result.path, warn.get("message", "")))

    starts = [t.start_us for t in tracks]
    durations = [t.duration_us for t in tracks]
    start_delta = (max(starts) - min(starts)) if starts else 0
    duration_delta = (max(durations) - min(durations)) if durations else 0

    if start_delta > 0:
        worst = max(tracks, key=lambda t: t.start_us)
        findings.append(Finding(
            "ZERO_ALIGN", "error", _fmt_us(0), _fmt_us(start_delta),
            worst.track_index, worst.path,
            "all tracks must share one zero point"))

    frame_us = max((t.sample_duration_us for t in tracks if t.kind == "video"),
                   default=DEFAULT_FRAME_US) or DEFAULT_FRAME_US
    tolerances = []
    if any(t.kind == "audio" for t in tracks):
        tolerances.append((AUDIO_TOLERANCE_US, "audio 40000us"))
    if any(t.kind == "video" for t in tracks):
        tolerances.append((frame_us, "video one frame"))
    for limit, label in tolerances:
        if duration_delta > limit:
            longest = max(tracks, key=lambda t: t.duration_us)
            findings.append(Finding(
                "DURATION_MATCH", "error", "<= %s" % label,
                _fmt_us(duration_delta), longest.track_index, longest.path,
                "track duration drift exceeds tolerance"))

    for t in tracks:
        if t.kind != "audio":
            continue
        actual = {"sample_rate": t.sample_rate, "channels": t.channels,
                  "bits_per_sample": t.bits_per_sample, "codec": t.codec}
        if actual != WHITELIST:
            findings.append(Finding(
                "AUDIO_WHITELIST", "error",
                "48000Hz/2ch/16bit/pcm",
                "%dHz/%dch/%dbit/%s" % (t.sample_rate, t.channels,
                                        t.bits_per_sample, t.codec),
                t.track_index, t.path,
                "audio track outside delivery whitelist"))

    for t in tracks:
        if t.language is not None and t.language in ("", "und"):
            findings.append(Finding(
                "LANGUAGE_REQUIRED", "warning", "ISO-639 language tag",
                t.language or "(none)", t.track_index, t.path,
                "track is missing a language tag"))

    for t in tracks:
        if t.kind != "video":
            continue
        swapped = (t.display_width, t.display_height) != (t.width, t.height)
        if t.rotation in (90, 270) and not swapped:
            findings.append(Finding(
                "ROTATION_DISPLAY", "error", "display dims swapped for rotation",
                "%dx%d rot%d" % (t.display_width, t.display_height, t.rotation),
                t.track_index, t.path,
                "rotation 90/270 must swap display width/height"))
        elif t.rotation in (0, 180) and swapped:
            findings.append(Finding(
                "ROTATION_DISPLAY", "warning", "display dims match coded dims",
                "%dx%d rot%d" % (t.display_width, t.display_height, t.rotation),
                t.track_index, t.path,
                "display dims swapped without rotation"))

    metrics = {
        "track_count": len(tracks),
        "max_duration_delta_us": duration_delta,
        "start_delta_us": start_delta,
    }
    return findings, metrics
