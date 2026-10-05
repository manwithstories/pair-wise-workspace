# jmespath-guard

A statically-typed, deterministic query engine for a JMESPath subset, built for
risk-control data pipelines that ingest tens of thousands of heterogeneous JSON
events per day and must reject a mistyped expression at **submission** time
rather than discovering the mistake halfway through a batch run.

```text
source ──▶ lexer ──▶ parser ──▶ AST ──▶ type solver ──▶ typed AST ──▶ evaluator ──▶ Value
             line/col   spans     untyped   constraints +      annotated     pure walk
                                    unification
                                    + minimal core
```

## Quick start

```rust
use jmespath_guard::{compile, compile_with_shape, Type};
use serde_json::json;

let doc = json!({"people": [{"name": "ada", "age": 36}]});

// Compile once, evaluate many times.
let q = compile("people[0].name")?;
assert_eq!(q.run(&doc), json!("ada"));

// Equivalent spellings produce byte-identical output and are recognised as such.
let a = compile("people[0].name")?;
let b = compile("people | [0] | name")?;
assert_eq!(a.run(&doc), b.run(&doc));
assert!(a.is_equivalent(&b));

// With a registered schema, type errors surface before any data moves.
let shape = Type::object(vec![("people", Type::array(Type::Any))]);
assert!(compile_with_shape("people | [0] | name", &shape).is_ok());
let err = compile_with_shape("score[0]", &Type::object(vec![("score", Type::Number)])).unwrap_err();
assert!(err.to_string().contains("an index or slice requires an array"));
# Ok::<(), Box<dyn std::error::Error>>(())
```

## The three guarantees

### 1. Errors are caught before evaluation

`compile` rejects a query for two distinct reasons.

**Structurally impossible** — rejected with no document at all:

| Query | Error |
|---|---|
| `sort_by(people, &age)` | unknown function `sort_by` |
| `contains(meta)` | `contains` takes 2 arguments but got 1 |
| `people[?age > \`30\`]` | filter expressions are not part of this subset |
| `people[0` | syntax error at line 1, column 9 |

**Concretely impossible** — rejected once a document schema is registered via
`compile_with_shape`:

| Query against `{"score": number}` | Error |
|---|---|
| `score[0]` | cannot index it: an index or slice requires an array |
| `score.name` | cannot read field `name` from it: requires an object |
| `keys(score)` | cannot call `keys` with (number) |
| `score[]` | cannot flatten it: flattening requires an array |

A missing field is deliberately *not* an error: JMESPath turns absent data into
`null`, so reading a key the schema does not list is legitimate. A checker that
rejected those would be unusable against heterogeneous upstream data.

### 2. Errors carry a minimal unsatisfiable subset

When several constraints interact, the solver reports a **minimal core**: an
irreducible set of requirements that cannot all hold at once. Removing any member
restores satisfiability.

```
static type check failed with 3 diagnostics:
  - type error: cannot index it: an index or slice requires an array — this node has type number at line 1, column 10
  - type error: cannot project over it: a projection requires an array or an object — this node has type number at line 1, column 10
  - type error: cannot call `length` with (number) — this node has type number at line 1, column 1
  minimal unsatisfiable subset (3 requirements):
    * number supports indexing (must be an array)
    * number supports wildcard projection (must be an object or an array)
    * `length` accepts (number)
```

### 3. Evaluation is deterministic and equivalence is automatic

The evaluator is a pure function: no hidden global state, no I/O, no clock, no
dependence on iteration order. Every object traversal goes through
[`ordered_keys`](src/eval.rs), which sorts by UTF-8 byte order — locale
independent, and independent of whether `serde_json`'s `preserve_order` feature is
enabled. A compiled `Query` is `Send + Sync`, so one instance shared across
threads yields identical bytes.

Equivalence is decided by a **normal form** ([`equiv::normalize`](src/equiv.rs)),
which rewrites a query into one left-nested accessor chain:

| Written as | Normalised to |
|---|---|
| `people[0].name` | `people[0].name` |
| `people \| [0] \| name` | `people[0].name` |
| `people.*.name` | `people[*].name` |
| `a.b` | `a.b` |
| `a."b"` | `a.b` |
| `{a: x, b: y}` | `{a:x,b:y}` (keys sorted) |
| `{b: y, a: x}` | `{a:x,b:y}` |

`search` enumerates alternative spellings of a query — every returned variant is
guaranteed to normalise to the same form, so it is safe to use for deduplicating
an analyst's query catalogue.

## Supported subset

Field access, quoted keys, integer indices (including negative), slices
`[start:stop:step]` with Python semantics, wildcards `[*]` / `.*`, flatten `a[]`,
flatten projection `a || b`, pipes, multi-select lists and hashes, `@`, `&expr`,
and the built-ins `length`, `keys`, `values`, `contains`, `merge`.

Slices and wildcards are **projections**: they rebind the node, so accessors to
their right apply per element.

```text
people[*].address.city   →  the city of every person
people[*].tags           →  the tag list of every person
people[*].tags[]         →  every tag of every person, spliced flat
matrix[*] | [1]          →  the second sub-array (a pipe rebinds only the right side)
```

## Performance

Budgets are asserted as p99 tests in `tests/perf.rs` and reported by
`cargo bench` on a ~10 KB document.

| Stage | Budget | Measured |
|---|---|---|
| Parse | < 100 µs | ~3 µs (compile incl. type check) |
| Type check | < 50 µs | ~3 µs |
| Evaluate | < 1 ms | 27–131 µs |

The evaluator's projection split is computed **once at compile time** and cached
in `Ast::chain`, which is why evaluation is a straight tree walk with no
repeated chain analysis.

## Layout

| File | Role |
|---|---|
| `src/lexer.rs` | tokeniser with 1-based line/column and byte spans |
| `src/parser.rs` | recursive descent; every error carries a span |
| `src/ast.rs` | node definitions, rendering, cached evaluation shape |
| `src/types.rs` | shape lattice, unification, requirement generation, minimal core |
| `src/eval.rs` | deterministic tree walk over `serde_json::Value` |
| `src/equiv.rs` | normal form, equivalence, variant search |
| `src/lib.rs` | `compile`, `compile_with_shape`, `Query`, integration tests |

## Commands

```sh
cargo test --release   # 145 tests: units, semantics, type checking, equivalence, perf
cargo bench            # criterion report
cargo clippy --all-targets
```

## Notes and limitations

- Bare identifiers are ASCII by the JMESPath grammar. Other scripts must be
  quoted (`"用户"."姓名"`); the lexer says so explicitly.
- String *literals* use backticks (`` `auto` ``). A quoted token in expression
  position names a *field*, matching JMESPath, so `contains(meta.tags, \`auto\`)`
  compares against the text "auto" while `meta."tags"` looks up a field. There is
  no ambiguity because backticks are the only literal syntax.
- Filter expressions are recognised by the lexer and rejected by the type checker
  with a precise location, rather than failing obscurely.
- `compile_with_shape` needs the schema; without one, only structural errors are
  caught, because almost every shape question is unanswerable without data.
