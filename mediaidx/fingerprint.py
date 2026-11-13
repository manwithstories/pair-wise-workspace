"""Perceptual fingerprints and pairwise verification for images/audio/video."""

from __future__ import annotations

import math
import pickle
import zlib
from typing import Any

import numpy as np
from PIL import Image

from mediaidx.canonical import MediaFamily
from mediaidx.loaders import LoadedMedia
from mediaidx.loaders.audio_loader import NormalizedAudio, TARGET_SAMPLE_RATE
from mediaidx.loaders.image_loader import NormalizedImage
from mediaidx.loaders.video_loader import NormalizedVideo


FINGERPRINT_VERSION = "fp-2026.11.3"
HASH_BITS = 64
AUDIO_FRAME_MS = 50
AUDIO_BAND_COUNT = 16
AUDIO_PROFILE_BINS = 64


def _dct_matrix(size: int) -> np.ndarray:
    positions = np.arange(size, dtype=np.float64)[None, :]
    frequencies = np.arange(size, dtype=np.float64)[:, None]
    matrix = np.cos(np.pi * (2.0 * positions + 1.0) * frequencies / (2.0 * size))
    matrix[0, :] *= math.sqrt(1.0 / size)
    matrix[1:, :] *= math.sqrt(2.0 / size)
    return matrix


_DCT32 = _dct_matrix(32)


def pack_bits(bits: np.ndarray) -> int:
    flat = np.asarray(bits, dtype=bool).reshape(-1)
    if flat.size < HASH_BITS:
        flat = np.pad(flat, (0, HASH_BITS - flat.size))
    packed = np.packbits(flat[:HASH_BITS], bitorder="big")
    return int.from_bytes(packed.tobytes(), byteorder="big", signed=False)


def hash_hex(value: int) -> str:
    return f"{value & ((1 << 64) - 1):016x}"


def hex_to_hash(value: str) -> int:
    return int(value, 16) & ((1 << 64) - 1)


def hamming64(left: int | str, right: int | str) -> int:
    left_int = hex_to_hash(left) if isinstance(left, str) else left
    right_int = hex_to_hash(right) if isinstance(right, str) else right
    return int((left_int ^ right_int).bit_count())


def split_hash_bands(value: str) -> tuple[int, int, int, int]:
    """Split a 64-bit hash into the four 16-bit LSH bands."""

    integer = hex_to_hash(value)
    return tuple((integer >> shift) & 0xFFFF for shift in (48, 32, 16, 0))


def _gray32(rgb: np.ndarray) -> np.ndarray:
    luminance = (
        0.299 * rgb[:, :, 0].astype(np.float32)
        + 0.587 * rgb[:, :, 1].astype(np.float32)
        + 0.114 * rgb[:, :, 2].astype(np.float32)
    )
    gray = Image.fromarray(np.clip(luminance, 0, 255).astype(np.uint8), mode="L")
    resized = gray.resize((32, 32), Image.Resampling.LANCZOS)
    return np.asarray(resized, dtype=np.float32)


def _image_components(rgb: np.ndarray) -> tuple[int, int, np.ndarray, np.ndarray]:
    gray = _gray32(rgb)
    centered = gray - float(gray.mean())
    dct = _DCT32 @ centered @ _DCT32.T
    low = dct[:8, :8]
    phash = pack_bits(low > float(np.median(low)))

    hash_image = Image.fromarray(np.clip(gray, 0, 255).astype(np.uint8), mode="L")
    dh_image = np.asarray(
        hash_image.resize((9, 8), Image.Resampling.LANCZOS), dtype=np.float32
    )
    dhash = pack_bits(dh_image[:, 1:] > dh_image[:, :-1])

    detail = dct[:12, :12].reshape(-1).astype(np.float32)
    detail[0] = 0.0
    norm = float(np.linalg.norm(detail))
    if norm > 0:
        detail /= norm
    return phash, dhash, detail, gray.reshape(-1).astype(np.uint8)


def image_fingerprint(image: NormalizedImage | np.ndarray) -> dict[str, Any]:
    rgb = image.rgb if isinstance(image, NormalizedImage) else image
    phash, dhash, detail, thumbnail = _image_components(rgb)
    primary = hash_hex(phash)
    secondary = hash_hex(dhash)
    return {
        "type": "image",
        "version": FINGERPRINT_VERSION,
        "phash": primary,
        "dhash": secondary,
        "candidate_hashes": sorted({primary, secondary}),
        "detail": detail,
        "thumbnail": thumbnail,
    }


def image_dct_hash(image: NormalizedImage | np.ndarray) -> int:
    """Return the 64-bit DCT perceptual hash used for candidate bands."""

    rgb = image.rgb if isinstance(image, NormalizedImage) else image
    phash, _dhash, _detail, _thumbnail = _image_components(rgb)
    return phash


def audio_band_energy(
    audio: NormalizedAudio,
) -> tuple[np.ndarray, np.ndarray]:
    """Return short-time band power and the full-track spectral profile."""

    return _audio_band_matrix(audio.samples, audio.sample_rate)


def _vector_cosine(left: np.ndarray, right: np.ndarray) -> float:
    left = np.asarray(left, dtype=np.float64).reshape(-1)
    right = np.asarray(right, dtype=np.float64).reshape(-1)
    denominator = float(np.linalg.norm(left) * np.linalg.norm(right))
    if denominator <= 1e-12:
        return 0.0
    return float(np.dot(left, right) / denominator)


def _image_similarity(left: dict[str, Any], right: dict[str, Any]) -> dict[str, Any]:
    ph_distance = hamming64(left["phash"], right["phash"])
    dh_distance = hamming64(left["dhash"], right["dhash"])
    detail_cosine = _vector_cosine(left["detail"], right["detail"])
    detail_left = np.asarray(left["detail"], dtype=np.float64)
    detail_right = np.asarray(right["detail"], dtype=np.float64)
    if np.linalg.norm(detail_left) <= 1e-12 and np.linalg.norm(detail_right) <= 1e-12:
        detail_cosine = 1.0
    thumbnail_left = np.asarray(left["thumbnail"], dtype=np.float64)
    thumbnail_right = np.asarray(right["thumbnail"], dtype=np.float64)
    left_centered = thumbnail_left - thumbnail_left.mean()
    right_centered = thumbnail_right - thumbnail_right.mean()
    if np.linalg.norm(left_centered) <= 1e-9 and np.linalg.norm(right_centered) <= 1e-9:
        # Correlation is undefined for flat images.  Compare the level instead
        # so identical solid-color transcodes still verify while black/white do not.
        mean_delta = abs(float(thumbnail_left.mean() - thumbnail_right.mean()))
        thumbnail_corr = max(0.0, 1.0 - mean_delta / 16.0)
    else:
        thumbnail_corr = _vector_cosine(left_centered, right_centered)
    score = (
        0.42 * (1.0 - ph_distance / HASH_BITS)
        + 0.23 * (1.0 - dh_distance / HASH_BITS)
        + 0.20 * max(0.0, detail_cosine)
        + 0.15 * max(0.0, thumbnail_corr)
    )
    return {
        "phash_distance": ph_distance,
        "dhash_distance": dh_distance,
        "detail_cosine": detail_cosine,
        "thumbnail_correlation": thumbnail_corr,
        "score": score,
    }


def verify_image_fingerprints(left: dict[str, Any], right: dict[str, Any]) -> tuple[bool, dict[str, Any]]:
    similarity = _image_similarity(left, right)
    same = (
        similarity["phash_distance"] <= 12
        and similarity["dhash_distance"] <= 18
        and similarity["detail_cosine"] >= 0.70
        and similarity["thumbnail_correlation"] >= 0.70
    ) or (
        similarity["phash_distance"] <= 5
        and similarity["detail_cosine"] >= 0.60
        and similarity["thumbnail_correlation"] >= 0.60
    )
    similarity["decision"] = "same" if same else "different"
    return same, similarity


def _audio_band_matrix(samples: np.ndarray, sample_rate: int = TARGET_SAMPLE_RATE) -> tuple[np.ndarray, np.ndarray]:
    frame_length = max(256, int(round(sample_rate * 0.128)))
    hop_length = max(1, int(round(sample_rate * AUDIO_FRAME_MS / 1000.0)))
    if samples.size < frame_length:
        padded = np.pad(samples, (0, frame_length - samples.size))
    else:
        padded = samples
    frame_count = 1 + max(0, (padded.size - frame_length) // hop_length)
    indices = np.arange(frame_length)[None, :] + np.arange(frame_count)[:, None] * hop_length
    frames = padded[indices]
    window = np.hanning(frame_length).astype(np.float32)
    spectrum = np.fft.rfft(frames * window, axis=1)
    power = np.square(np.abs(spectrum), dtype=np.float32)
    frequencies = np.fft.rfftfreq(frame_length, d=1.0 / sample_rate)
    usable_high = min(7000.0, sample_rate * 0.46)
    edges = np.geomspace(55.0, usable_high, AUDIO_BAND_COUNT + 1)
    bands = np.zeros((frame_count, AUDIO_BAND_COUNT), dtype=np.float64)
    for band in range(AUDIO_BAND_COUNT):
        low, high = edges[band], edges[band + 1]
        mask = (frequencies >= low) & (frequencies < high)
        if not np.any(mask):
            nearest = int(np.argmin(np.abs(frequencies - (low + high) / 2.0)))
            bands[:, band] = power[:, nearest]
        else:
            bands[:, band] = power[:, mask].mean(axis=1)
    profile_power = bands.mean(axis=0)
    profile_freqs = np.geomspace(55.0, usable_high, AUDIO_PROFILE_BINS)
    sampled_profile = np.interp(profile_freqs, edges[:-1], np.maximum(profile_power, 0.0))
    return bands, sampled_profile


def _quantize_audio_features(log_bands: np.ndarray) -> np.ndarray:
    centered = log_bands - log_bands.mean(axis=1, keepdims=True)
    maxima = np.max(np.abs(centered), axis=1, keepdims=True)
    normalized = np.divide(centered, maxima, out=np.zeros_like(centered), where=maxima > 1e-9)
    return np.clip(np.rint(normalized * 127.0), -127, 127).astype(np.int8)


def _quantize_envelope(values: np.ndarray) -> np.ndarray:
    values = np.asarray(values, dtype=np.float64)
    centered = values - values.mean()
    scale = float(np.max(np.abs(centered))) if centered.size else 0.0
    if scale <= 1e-9:
        return np.zeros(values.size, dtype=np.int8)
    return np.clip(np.rint(centered / scale * 127.0), -127, 127).astype(np.int8)


def audio_fingerprint(audio: NormalizedAudio) -> dict[str, Any]:
    bands, profile = _audio_band_matrix(audio.samples, audio.sample_rate)
    log_bands = np.log1p(np.maximum(bands, 0.0) * 1e14)
    profile_log = np.log1p(np.maximum(profile, 0.0) * 1e14)
    centered_profile = profile_log - float(profile_log.mean())
    profile_hash = pack_bits(centered_profile > float(np.median(centered_profile)))

    temporal = _quantize_audio_features(log_bands) / 127.0
    sample_count = temporal.shape[0]
    positions = np.linspace(0, max(0, sample_count - 1), 8).round().astype(int)
    grouped = temporal[positions].reshape(8, 8, 2).mean(axis=2)
    temporal_hash = pack_bits(grouped > np.median(grouped, axis=1, keepdims=True))

    envelope_raw = np.mean(log_bands, axis=1)
    candidate_hashes = sorted({hash_hex(profile_hash), hash_hex(temporal_hash)})
    return {
        "type": "audio",
        "version": FINGERPRINT_VERSION,
        "profile_hash": hash_hex(profile_hash),
        "temporal_hash": hash_hex(temporal_hash),
        "candidate_hashes": candidate_hashes,
        "profile": profile_log.astype(np.float32),
        "features": _quantize_audio_features(log_bands),
        "envelope": _quantize_envelope(envelope_raw),
        "duration": float(audio.duration),
        "hop_ms": AUDIO_FRAME_MS,
    }


def _normalized_envelope(envelope: np.ndarray) -> np.ndarray:
    values = np.asarray(envelope, dtype=np.float64)
    if values.size == 0:
        return values
    values = values - values.mean()
    norm = float(np.linalg.norm(values))
    if norm <= 1e-12:
        return np.zeros_like(values)
    return values / norm


def _best_offset(left: np.ndarray, right: np.ndarray, max_shift: int) -> tuple[int, float]:
    left_norm = _normalized_envelope(left)
    right_norm = _normalized_envelope(right)
    best_shift = 0
    best_score = -1.0
    for shift in range(-max_shift, max_shift + 1):
        if shift >= 0:
            left_part = left_norm[shift:]
            right_part = right_norm[: max(0, right_norm.size - shift)]
        else:
            left_part = left_norm[: max(0, left_norm.size + shift)]
            right_part = right_norm[-shift:]
        overlap = min(left_part.size, right_part.size)
        if overlap < 4:
            continue
        left_part = left_part[:overlap]
        right_part = right_part[:overlap]
        score = float(np.dot(left_part, right_part))
        if score > best_score:
            best_score = score
            best_shift = shift
    return best_shift, max(0.0, best_score)


def verify_audio_fingerprints(left: dict[str, Any], right: dict[str, Any]) -> tuple[bool, dict[str, Any]]:
    duration_a = float(left["duration"])
    duration_b = float(right["duration"])
    maximum = max(duration_a, duration_b)
    ratio = min(duration_a, duration_b) / maximum if maximum > 0 else 0.0
    duration_delta = abs(duration_a - duration_b)
    # A 50 ms feature hop can quantize a 250 ms crop to 260 ms in a WAV file;
    # 280 ms still accepts the documented 250 ms allowance while rejecting
    # the next 50 ms hop and larger trims.
    duration_ok = ratio >= 0.70 and duration_delta <= 0.28

    hop_ms = int(left.get("hop_ms", AUDIO_FRAME_MS))
    max_shift = int(math.ceil(250.0 / hop_ms))
    left_env = np.asarray(left["envelope"], dtype=np.float64)
    right_env = np.asarray(right["envelope"], dtype=np.float64)
    shift, envelope_correlation = _best_offset(left_env, right_env, max_shift)
    offset_ms = abs(shift * hop_ms)
    offset_ok = offset_ms <= 250

    left_features = np.asarray(left["features"], dtype=np.float64) / 127.0
    right_features = np.asarray(right["features"], dtype=np.float64) / 127.0
    if shift >= 0:
        left_part = left_features[shift:]
        right_part = right_features[: max(0, right_features.shape[0] - shift)]
    else:
        left_part = left_features[: max(0, left_features.shape[0] + shift)]
        right_part = right_features[-shift:]
    overlap = min(left_part.shape[0], right_part.shape[0])
    left_part = left_part[:overlap]
    right_part = right_part[:overlap]
    if overlap:
        dots = np.sum(left_part * right_part, axis=1)
        norms = np.linalg.norm(left_part, axis=1) * np.linalg.norm(right_part, axis=1)
        frame_cosines = np.divide(dots, norms, out=np.zeros_like(dots), where=norms > 1e-12)
        median_frame_cosine = float(np.median(frame_cosines))
    else:
        median_frame_cosine = 0.0
    profile_correlation = _vector_cosine(left["profile"], right["profile"])
    score = 0.62 * median_frame_cosine + 0.38 * max(0.0, profile_correlation)
    same = (
        duration_ok
        and offset_ok
        and overlap >= max(6, int(0.30 * max(left_features.shape[0], right_features.shape[0])))
        and median_frame_cosine >= 0.70
        and profile_correlation >= 0.78
    )
    return same, {
        "duration_ratio": ratio,
        "duration_delta": duration_delta,
        "offset_ms": offset_ms,
        "envelope_correlation": envelope_correlation,
        "frame_cosine": median_frame_cosine,
        "profile_correlation": profile_correlation,
        "score": score,
        "decision": "same" if same else "different",
    }


def _sequence_signature(frames: list[dict[str, Any]]) -> int:
    if not frames:
        return 0
    indices = np.linspace(0, len(frames) - 1, min(8, len(frames))).round().astype(int)
    anchors = [frames[int(index)] for index in indices]
    values = [hex_to_hash(frame["phash"]) for frame in anchors]
    bits = np.zeros(HASH_BITS, dtype=np.int32)
    for value in values:
        for bit in range(HASH_BITS):
            bits[bit] += (value >> (HASH_BITS - 1 - bit)) & 1
    return pack_bits(bits * 2 >= len(values))


def video_fingerprint(video: NormalizedVideo) -> dict[str, Any]:
    frame_fingerprints = [image_fingerprint(frame) for frame in video.frames]
    signature = _sequence_signature(frame_fingerprints)
    candidate_hashes: set[str] = set()
    for frame in frame_fingerprints:
        candidate_hashes.update(frame["candidate_hashes"])
    candidate_hashes.add(hash_hex(signature))
    anchors = np.linspace(0, max(0, len(frame_fingerprints) - 1), min(8, len(frame_fingerprints)))
    anchor_indices = sorted(set(int(round(index)) for index in anchors))
    return {
        "type": "video",
        "version": FINGERPRINT_VERSION,
        "sequence_hash": hash_hex(signature),
        "candidate_hashes": sorted(candidate_hashes),
        "frame_fingerprints": frame_fingerprints,
        "timestamps": [float(value) for value in video.timestamps],
        "duration": float(video.duration),
        "anchor_indices": anchor_indices,
    }


def verify_video_fingerprints(left: dict[str, Any], right: dict[str, Any]) -> tuple[bool, dict[str, Any]]:
    duration_a = float(left["duration"])
    duration_b = float(right["duration"])
    maximum = max(duration_a, duration_b)
    ratio = min(duration_a, duration_b) / maximum if maximum > 0 else 0.0
    duration_delta = abs(duration_a - duration_b)
    # Up to one second may be removed from each end, so the absolute duration
    # difference is the meaningful bound here; the frame alignment decides
    # whether the surviving content actually overlaps.
    duration_ok = duration_delta <= 2.05

    left_frames = left["frame_fingerprints"]
    right_frames = right["frame_fingerprints"]
    left_times = np.asarray(left["timestamps"], dtype=np.float64)
    right_times = np.asarray(right["timestamps"], dtype=np.float64)
    # The persisted anchor set is eight frames for the sequence signature.  The
    # verifier may use every extracted frame to tolerate up to one second of
    # trimming at either end without losing all pair opportunities.
    comparison_indices = list(range(len(left_frames)))
    interval_left = float(np.median(np.diff(left_times))) if left_times.size > 1 else 1.0
    interval_right = float(np.median(np.diff(right_times))) if right_times.size > 1 else 1.0
    tolerance = max(0.10, 1.10 * max(interval_left, interval_right))

    offsets = {0.0}
    for anchor in comparison_indices:
        for right_time in right_times:
            delta = float(left_times[anchor] - right_time)
            if abs(delta) <= 1.05:
                offsets.add(round(delta, 4))

    best: dict[str, Any] | None = None
    for offset in sorted(offsets):
        pairs: list[dict[str, Any]] = []
        for index in comparison_indices:
            desired = float(left_times[index] - offset)
            nearest = int(np.argmin(np.abs(right_times - desired)))
            if abs(float(right_times[nearest]) - desired) > tolerance:
                continue
            similarity = _image_similarity(left_frames[index], right_frames[nearest])
            similarity["left_index"] = index
            similarity["right_index"] = nearest
            pairs.append(similarity)
        if len(pairs) < 5:
            continue
        median_score = float(np.median([pair["score"] for pair in pairs]))
        median_phash = float(np.median([pair["phash_distance"] for pair in pairs]))
        candidate = {
            "offset_seconds": float(offset),
            "paired_frames": len(pairs),
            "median_score": median_score,
            "median_phash_distance": median_phash,
            "median_detail_cosine": float(np.median([pair["detail_cosine"] for pair in pairs])),
            "median_thumbnail_correlation": float(np.median([pair["thumbnail_correlation"] for pair in pairs])),
            "pairs": pairs,
        }
        if best is None or candidate["median_score"] > best["median_score"]:
            best = candidate

    if best is None:
        return False, {
            "duration_ratio": ratio,
            "duration_delta": duration_delta,
            "offset_seconds": None,
            "paired_frames": 0,
            "median_score": 0.0,
            "decision": "different",
        }
    same = (
        duration_ok
        and best["median_score"] >= 0.84
        and best["median_phash_distance"] <= 8
        and best["median_detail_cosine"] >= 0.74
        and best["median_thumbnail_correlation"] >= 0.72
        and best["paired_frames"] >= 6
    )
    best.update({"duration_ratio": ratio, "duration_delta": duration_delta, "decision": "same" if same else "different"})
    return same, best


def fingerprint_loaded(media: LoadedMedia) -> dict[str, Any]:
    if media.kind.family == MediaFamily.IMAGE:
        return image_fingerprint(media.normalized)
    if media.kind.family == MediaFamily.AUDIO:
        return audio_fingerprint(media.normalized)
    if media.kind.family == MediaFamily.VIDEO:
        return video_fingerprint(media.normalized)
    raise ValueError(f"unsupported family {media.kind.family}")


def verify_fingerprints(family: str, left: dict[str, Any], right: dict[str, Any]) -> tuple[bool, dict[str, Any]]:
    if family == MediaFamily.IMAGE.value:
        return verify_image_fingerprints(left, right)
    if family == MediaFamily.AUDIO.value:
        return verify_audio_fingerprints(left, right)
    if family == MediaFamily.VIDEO.value:
        return verify_video_fingerprints(left, right)
    return False, {"decision": "different", "reason": "unsupported_family"}


def pack_fingerprint(fingerprint: dict[str, Any]) -> bytes:
    return zlib.compress(pickle.dumps(fingerprint, protocol=5), level=6)


def unpack_fingerprint(payload: bytes) -> dict[str, Any]:
    return pickle.loads(zlib.decompress(payload))


def candidate_hashes(fingerprint: dict[str, Any]) -> list[str]:
    return list(fingerprint.get("candidate_hashes", []))


phash = image_dct_hash
image_phash = image_dct_hash
