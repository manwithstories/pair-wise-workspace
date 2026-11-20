"""Per-packet corruption detection used by the chunk planner.

Two strategies, chosen so that two runs always agree:

* Audio (AAC) frames are independently decodable, so a *fresh* decoder per
  packet makes detection stateless - the same packet is judged the same way
  no matter where the walk starts.
* Video (H.264) needs decoder state, so the context is reused within a chunk
  and reset at every keyframe boundary (chunk boundaries are keyframes).  A
  resumed run starting at a committed boundary therefore sees exactly the
  decoder state an uninterrupted run would.
"""

from __future__ import annotations

import av


class AudioValidator:
    """Stateless AAC validation: a fresh decoder for every packet."""

    def __init__(self, stream: av.stream.Stream):
        self.stream = stream
        self.codec = stream.codec_context

    def _fresh(self) -> av.CodecContext:
        context = av.CodecContext.create(self.codec.name, "r")
        if self.codec.extradata:
            context.extradata = bytes(self.codec.extradata)
        context.sample_rate = self.codec.sample_rate
        context.channels = self.codec.channels
        context.open()
        return context

    def check(self, packet: av.Packet) -> str | None:
        """Return an error description, or ``None`` when the packet is fine."""
        context = self._fresh()
        try:
            frames = context.decode(packet)
        except Exception as exc:
            return f"{type(exc).__name__}: {str(exc)[:120]}"
        if not frames and packet.dts and packet.dts > 0:
            return "decoder produced no frame for a non-priming packet"
        return None


class VideoValidator:
    """Chunk-scoped H.264 validation with a reset at every chunk boundary."""

    def __init__(self, stream: av.stream.Stream):
        self.stream = stream
        self.codec = stream.codec_context
        self.context: av.CodecContext | None = None

    def reset(self) -> None:
        self.context = None

    def _fresh(self) -> av.CodecContext:
        context = av.CodecContext.create(self.codec.name, "r")
        if self.codec.extradata:
            context.extradata = bytes(self.codec.extradata)
        context.width = self.codec.width
        context.height = self.codec.height
        context.pix_fmt = self.codec.pix_fmt
        context.open()
        return context

    def check(self, packet: av.Packet) -> str | None:
        if self.context is None:
            self.context = self._fresh()
        try:
            self.context.decode(packet)
            return None
        except Exception as exc:
            self.context = None
            return f"{type(exc).__name__}: {str(exc)[:120]}"
