"""Shared event dataclass used between forensic modules."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from typing import Any


SOURCE_PRIORITY = {
    "metadata": 0,
    "file_record": 1,
    "sqlite": 2,
}


@dataclass
class TimelineEvent:
    event_id: str
    utc: datetime
    entity_key: str
    event_type: str
    description: str
    sources: tuple[str, ...]
    source_priority: int
    sequence: int
    raw: dict[str, Any] = field(default_factory=dict)
    anomalous: bool = False
    recovered: bool = False

    def merged_with(self, other: "TimelineEvent") -> "TimelineEvent":
        sources = tuple(sorted(set(self.sources) | set(other.sources)))
        primary, secondary = (
            (self, other)
            if (self.source_priority, self.sequence, self.event_id)
            <= (other.source_priority, other.sequence, other.event_id)
            else (other, self)
        )
        raw = dict(primary.raw)
        raw["merged_raw"] = [self.raw, other.raw]
        return TimelineEvent(
            event_id=primary.event_id,
            utc=min(self.utc, other.utc),
            entity_key=primary.entity_key,
            event_type=primary.event_type,
            description=primary.description,
            sources=sources,
            source_priority=min(self.source_priority, other.source_priority),
            sequence=min(self.sequence, other.sequence),
            raw=raw,
            anomalous=self.anomalous or other.anomalous,
            recovered=self.recovered or other.recovered,
        )

    def sort_key(self) -> tuple[datetime, int, int, str]:
        return (self.utc, self.source_priority, self.sequence, self.event_id)
