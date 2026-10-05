"""Session state machine: ordered frames -> switch/measurement timeline.

Each reassembled frame body is a flat TLV sequence.  Two tag families matter:

* **Registers** (tag high byte ``0x01``) carry raw measurement values; the low
  byte is the point id.  ``0x01xx`` is a signed 16-bit scaled reading, which is
  how breaker/transformer analogs arrive.
* **Commands** (tag ``0x0201``) carry breaker open/close state; the low 8 bits
  of the value are the breaker index and the next byte the state.

The machine keeps a bounded per-point history so a long-running session cannot
grow without limit, and it exposes ``snapshot``/``restore`` so a resumed run
continues the same timeline instead of restarting it.
"""

from __future__ import annotations

import struct
from typing import Any, Dict, List, Optional, Tuple

TAG_REGISTER = 0x0100
TAG_CMD = 0x0201

#: 0 = open, 1 = closed; anything else is reported as an unknown state.
STATE_OPEN = 0
STATE_CLOSED = 1
STATE_NAMES = {STATE_OPEN: "open", STATE_CLOSED: "closed"}

_I16 = struct.Struct("<h")
_U8 = struct.Struct("<B")

DEFAULT_HISTORY = 64


class StateError(ValueError):
    """Malformed measurement payload.  Routed to the DLQ like any other fault."""


def _decode_register(point: int, value: bytes) -> Tuple[str, Any]:
    if len(value) == 2:
        return "i16", _I16.unpack(value)[0]
    if len(value) == 4:
        return "i32", _I16.unpack(value[:2])[0]
    raise StateError(f"register 0x{point:04x}: unsupported width {len(value)}")


class SessionState:
    """Mutable timeline state for one substation session."""

    __slots__ = ("session_id", "points", "breakers", "history", "frames_seen",
                 "last_seq", "max_history", "transitions")

    def __init__(self, session_id: str, max_history: int = DEFAULT_HISTORY) -> None:
        self.session_id = session_id
        self.points: Dict[int, Dict[str, Any]] = {}
        self.breakers: Dict[int, Dict[str, Any]] = {}
        self.history: List[Dict[str, Any]] = []
        self.frames_seen = 0
        self.last_seq = -1
        self.max_history = max_history
        self.transitions = 0

    def apply(self, seq: int, body: bytes) -> Dict[str, Any]:
        """Fold one ordered frame into the timeline and return a change record."""
        from .protocol import iter_tlvs

        changed_points: Dict[int, Any] = {}
        changed_breakers: Dict[int, int] = {}
        n_tlv = 0

        for tag, value in iter_tlvs(body):
            n_tlv += 1
            if (tag & 0xFF00) == TAG_REGISTER:
                point = tag & 0xFF
                kind, reading = _decode_register(point, value)
                prev = self.points.get(point)
                if prev is None or prev.get("value") != reading:
                    changed_points[point] = reading
                self.points[point] = {
                    "value": reading,
                    "type": kind,
                    "seq": seq,
                }
            elif tag == TAG_CMD:
                if len(value) < 2:
                    raise StateError(f"cmd TLV too short: {len(value)} bytes")
                breaker = value[0]
                state = value[1]
                prev = self.breakers.get(breaker)
                if prev is None or prev.get("state") != state:
                    changed_breakers[breaker] = state
                    self.transitions += 1
                self.breakers[breaker] = {"state": state, "seq": seq}
            # Unknown tags are ignored on purpose: gateways add vendor tags
            # frequently and an unknown tag must not fail the replay.

        self.frames_seen += 1
        self.last_seq = seq

        record: Dict[str, Any] = {"seq": seq, "session_id": self.session_id}
        if changed_points:
            record["points"] = {str(k): v for k, v in changed_points.items()}
        if changed_breakers:
            record["breakers"] = {
                str(k): STATE_NAMES.get(v, f"unknown:{v}")
                for k, v in changed_breakers.items()
            }
        if record.get("points") or record.get("breakers"):
            self.history.append(record)
            if len(self.history) > self.max_history:
                del self.history[: len(self.history) - self.max_history]
        return record

    # -- persistence -------------------------------------------------------

    def snapshot(self) -> Dict[str, Any]:
        return {
            "points": {str(k): v for k, v in self.points.items()},
            "breakers": {str(k): v for k, v in self.breakers.items()},
            "frames_seen": self.frames_seen,
            "last_seq": self.last_seq,
            "transitions": self.transitions,
        }

    def restore(self, snap: Dict[str, Any]) -> None:
        self.points = {int(k): v for k, v in snap.get("points", {}).items()}
        self.breakers = {int(k): v for k, v in snap.get("breakers", {}).items()}
        self.frames_seen = int(snap.get("frames_seen", 0))
        self.last_seq = int(snap.get("last_seq", -1))
        self.transitions = int(snap.get("transitions", 0))

    def current(self) -> Dict[str, Any]:
        """Latest known state of every tracked point, for a final summary."""
        return {
            "session_id": self.session_id,
            "frames_seen": self.frames_seen,
            "last_seq": self.last_seq,
            "breakers": {
                str(k): STATE_NAMES.get(v["state"], f"unknown:{v['state']}")
                for k, v in sorted(self.breakers.items())
            },
            "points": {
                str(k): v["value"] for k, v in sorted(self.points.items())
            },
        }


class StateMachine:
    """Owns one :class:`SessionState` per session, bounded by ``max_sessions``."""

    __slots__ = ("max_sessions", "max_history", "_states", "_order", "ejected")

    def __init__(self, max_sessions: int = 64, max_history: int = DEFAULT_HISTORY) -> None:
        self.max_sessions = max_sessions
        self.max_history = max_history
        self._states: Dict[str, SessionState] = {}
        self._order: List[str] = []
        self.ejected = 0

    def get(self, session_id: str) -> SessionState:
        st = self._states.get(session_id)
        if st is None:
            if len(self._order) >= self.max_sessions:
                # Evict the least-recently-used session's live view; its
                # snapshot is already durable in SQLite, so a later resume can
                # restore it.  Keeping memory flat matters more than convenience.
                old = self._order.pop(0)
                self._states.pop(old, None)
                self.ejected += 1
            st = SessionState(session_id, self.max_history)
            self._states[session_id] = st
            self._order.append(session_id)
        else:
            self._order.remove(session_id)
            self._order.append(session_id)
        return st

    def apply(self, session_id: str, seq: int, body: bytes) -> Optional[Dict[str, Any]]:
        st = self.get(session_id)
        rec = st.apply(seq, body)
        return rec if ("points" in rec or "breakers" in rec) else None

    def restore(self, session_id: str, snap: Dict[str, Any]) -> None:
        self.get(session_id).restore(snap)

    def states(self) -> Dict[str, SessionState]:
        return self._states
