"""Per-source mapping plan and cross-source type arbitration."""
from __future__ import annotations

from dataclasses import dataclass, field

# Convergence ladder: decimal > float > int > datetime > string.
# Numeric rungs converge to the strongest numeric type; mixing numeric
# with datetime/string (or datetime with string) is not convergent.
NUMERIC_TYPES = ("decimal", "float", "int")
ALL_TYPES = ("decimal", "float", "int", "datetime", "string")


def resolve_types(declared) -> tuple:
    """Return (resolved_type, conflict) for a set of declared types."""
    kinds = set(declared)
    if len(kinds) == 1:
        return kinds.pop(), False
    if kinds.issubset(set(NUMERIC_TYPES)):
        for candidate in NUMERIC_TYPES:
            if candidate in kinds:
                return candidate, False
    return None, True


@dataclass
class BindingPlan:
    source: str
    column: str
    declared_type: str
    unit_factor: float | None
    renamed: bool


@dataclass
class FieldPlan:
    target: str
    target_type: str
    default: object
    enum: str | None
    bindings: dict = field(default_factory=dict)  # source name -> BindingPlan
    resolved_type: str | None = None
    conflict: bool = False


@dataclass
class Plan:
    fields: dict          # target field -> FieldPlan
    pk_field: str
    dropped: dict         # source name -> [unused source columns]
    defaulted: list       # target fields with no binding at all
    bindings_total: int
    renamed_total: int

    @property
    def mapped(self):
        return [name for name, fp in self.fields.items() if fp.bindings]

    @property
    def conflicts(self):
        return [name for name, fp in self.fields.items() if fp.conflict]

    @property
    def dropped_total(self):
        return sum(len(cols) for cols in self.dropped.values())


def build_plan(config, schemas: dict) -> Plan:
    fields = {}
    for name, spec in config.target_schema.items():
        fields[name] = FieldPlan(
            target=name,
            target_type=spec["type"],
            default=spec.get("default"),
            enum=spec.get("enum"),
        )
    bindings_total = 0
    renamed_total = 0
    used = {src.name: set() for src in config.sources}
    for src in config.sources:
        schema = schemas[src.name]
        for target, binding in src.bindings.items():
            column = binding.column
            used[src.name].add(column)
            renamed = column != target
            renamed_total += 1 if renamed else 0
            bindings_total += 1
            fields[target].bindings[src.name] = BindingPlan(
                source=src.name,
                column=column,
                declared_type=schema.get(column, "string"),
                unit_factor=binding.unit_factor,
                renamed=renamed,
            )
    for fp in fields.values():
        if fp.bindings:
            declared = [b.declared_type for b in fp.bindings.values()]
            fp.resolved_type, fp.conflict = resolve_types(declared)
        else:
            fp.resolved_type, fp.conflict = fp.target_type, False
    dropped = {}
    for src in config.sources:
        unused = sorted(set(schemas[src.name]) - used[src.name])
        if unused:
            dropped[src.name] = unused
    defaulted = [name for name, fp in fields.items() if not fp.bindings]
    pk_field = next(
        name for name, spec in config.target_schema.items() if spec.get("pk")
    )
    return Plan(fields, pk_field, dropped, defaulted, bindings_total, renamed_total)
