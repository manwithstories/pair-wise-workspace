//! Deterministic evaluation of a type-annotated AST against `serde_json::Value`.
//!
//! # Purity and determinism
//!
//! The evaluator is a pure tree walk.  It holds no interior mutable state, opens
//! no files, consults no clock and no environment, and allocates only through
//! the supplied `&Value`.  Crucially, every place where iteration order could
//! otherwise leak a `HashMap`'s nondeterminism is funnelled through
//! [`ordered_keys`], which sorts keys lexicographically (byte order, which for
//! UTF-8 equals code-point order).  Consequently:
//!
//! * the same compiled query and the same JSON input always produce
//!   byte-identical output;
//! * two queries that are semantically equivalent produce byte-identical
//!   output, which is what makes the equivalence tests meaningful;
//! * a compiled [`crate::Query`] is `Send + Sync` and safe to share across
//!   threads, and concurrent evaluation of the same instance still agrees
//!   byte-for-byte.

use serde_json::{Map, Value};

use crate::ast::{AstKind, BracketSpec, EvalShape, TypedAst};
use crate::types::Type;

/// Keys of a JSON object in a deterministic, locale-independent order.
///
/// `serde_json::Map` is a `BTreeMap` unless the `preserve_order` feature is on,
/// in which case it is an `IndexMap`.  Sorting explicitly makes the behaviour
/// independent of that feature flag, which is essential for the byte-for-byte
/// reproducibility guarantees.
pub fn ordered_keys(map: &Map<String, Value>) -> Vec<&str> {
    let mut keys: Vec<&str> = map.keys().map(|s| s.as_str()).collect();
    keys.sort_unstable();
    keys
}

/// Evaluate `ast` against `data`, returning a freshly allocated result.
///
/// Missing paths yield `null`, matching JMESPath semantics; the type checker has
/// already rejected the shapes where that fallback would mask a real bug.
///
/// # Projections
///
/// A projection (`[*]`, `.*`, `[]`, `||`) *rebinds* the node: every accessor
/// written to its right applies to each projected element.  This is why the
/// evaluator distinguishes `lhs` from `rhs` explicitly rather than recursing
/// blindly — `people[*].name` is "the name of every person", not "the `name`
/// field of the array of people".
pub fn eval(ast: &TypedAst, data: &Value) -> Value {
    // A projection (`[*]`, `.*`, `[]`, a slice, `||`) *rebinds* the node: every
    // accessor written after it applies to each projected element, however deep
    // the chain runs.  This is why `people[*].address.city` is "the city of every
    // person" rather than "the `city` field of the projected array".
    //
    // The chain is walked down to its left-most projection, the left side is
    // evaluated and projected, and the remaining accessors are applied to each
    // element.  `chain_shape` caches that split so the walk happens once at
    // compile time rather than on every evaluation.
    match &ast.chain {
        EvalShape::Plain => eval_node(ast, data),
        EvalShape::Projected {
            source,
            rest,
            flatten,
        } => {
            let elements = project_elements(source, data, false);
            let mapped: Vec<Value> = elements.iter().map(|e| eval(rest, e)).collect();
            // A flatten splices *after* the remainder has been applied, so
            // `people[*].tags[]` yields a flat list of every tag rather than a
            // list of tag lists.
            let out = if *flatten {
                flatten_into(&Value::Array(mapped))
            } else {
                mapped
            };
            Value::Array(out)
        }
    }
}

/// Evaluate a projection node and return the elements it yields.
///
/// `source` is a projection node (`[*]`, `.*`, a slice, `||`).  Its own shape may
/// itself be a projection — as in `people[*].tags[]`, where the outer flatten
/// projects over people and the inner projection resolves `tags` first — so the
/// split is resolved recursively.  The result is a flat list of values, with a
/// flatten's one-level splice applied when asked for.
fn project_elements(source: &TypedAst, data: &Value, do_flatten: bool) -> Vec<Value> {
    // `do_flatten` is only ever `true` for the innermost source of a `||`
    // projection; the outer chain handles its own splice in `eval`.
    // The `source` of a `Projected` shape is a projection node in its own right,
    // so resolving it may itself yield a projection.  The remainder is applied
    // per element, and only then does a flatten splice across the result — which
    // is what makes `a[*].b[]` collect every element of every `b`.
    let elements = match &source.chain {
        EvalShape::Plain => projected(&eval_node(source, data)),
        EvalShape::Projected {
            source: inner,
            rest,
            flatten,
        } => {
            let base = project_elements(inner, data, *flatten);
            base.iter().map(|e| eval(rest, e)).collect()
        }
    };
    if do_flatten {
        flatten_into(&Value::Array(elements))
    } else {
        elements
    }
}

/// Evaluate one node, with any projection in its chain already handled.
fn eval_node(ast: &TypedAst, data: &Value) -> Value {
    match &ast.kind {
        AstKind::Root => data.clone(),
        AstKind::Current => data.clone(),
        AstKind::RefExpr(inner) => eval(inner, data),
        AstKind::Ident(name) | AstKind::QuotedField(name) => lookup(data, name),
        AstKind::LiteralKey(s) | AstKind::StringLiteral(s) => Value::String(s.clone()),
        AstKind::NumberLiteral(n) => Value::from(*n),

        AstKind::Field(lhs, name) => {
            let base = eval(lhs, data);
            lookup(&base, name)
        }

        AstKind::Bracket(lhs, spec) => {
            let base = eval(lhs, data);
            apply_bracket(spec, &base, data)
        }

        AstKind::Flatten(lhs) => Value::Array(flatten_into(&eval(lhs, data))),

        AstKind::Wildcard(lhs) => Value::Array(projected(&eval(lhs, data))),

        AstKind::Projection(lhs, rhs) => {
            // `a || b`: project `a`, then evaluate `b` on each element.  The left
            // side may itself project, in which case its own remainder has to be
            // applied before the projection.
            let elements = match &lhs.chain {
                EvalShape::Plain => projected(&eval_node(lhs, data)),
                EvalShape::Projected {
                    source,
                    rest,
                    flatten,
                } => project_elements(source, data, *flatten)
                    .iter()
                    .map(|e| eval(rest, e))
                    .collect(),
            };
            Value::Array(elements.iter().map(|e| eval(rhs, e)).collect())
        }

        AstKind::Pipe(lhs, rhs) => {
            let mid = eval(lhs, data);
            eval(rhs, &mid)
        }

        AstKind::MultiSelectList(items) => {
            Value::Array(items.iter().map(|i| eval(i, data)).collect())
        }

        AstKind::MultiSelectHash(items) => {
            let mut out = Map::new();
            for (k, _, v) in items {
                out.insert(k.clone(), eval(v, data));
            }
            Value::Object(out)
        }

        AstKind::Call { builtin, args, .. } => {
            let vals: Vec<Value> = args.iter().map(|a| eval(a, data)).collect();
            match builtin {
                Some(b) => call(*b, &vals),
                // The type checker rejects unknown functions before evaluation,
                // so this branch is unreachable for compiled queries.
                None => Value::Null,
            }
        }
    }
}

/// Field access on an object; anything else yields `null`.
fn lookup(data: &Value, key: &str) -> Value {
    data.as_object()
        .and_then(|o| o.get(key))
        .cloned()
        .unwrap_or(Value::Null)
}

/// One level of flattening: nested arrays are spliced, scalars are kept.
fn flatten_into(base: &Value) -> Vec<Value> {
    let Value::Array(items) = base else {
        return Vec::new();
    };
    let mut out = Vec::with_capacity(items.len());
    for item in items {
        match item {
            Value::Array(inner) => out.extend(inner.iter().cloned()),
            other => out.push(other.clone()),
        }
    }
    out
}

/// Apply a bracket specifier to an already-evaluated `base`.
///
/// `doc` is the enclosing node, which bracket forms carrying their own
/// expression (`a[[i]]`) evaluate against.
fn apply_bracket(spec: &BracketSpec, base: &Value, doc: &Value) -> Value {
    match spec {
        BracketSpec::Wildcard => Value::Array(projected(base)),
        BracketSpec::Index(i) => match &base {
            Value::Array(items) => {
                let idx = if *i < 0 { items.len() as i64 + *i } else { *i };
                if idx < 0 || idx as usize >= items.len() {
                    Value::Null
                } else {
                    items[idx as usize].clone()
                }
            }
            // JSON has no object indexing by position.
            _ => Value::Null,
        },
        BracketSpec::Slice(spec) => match &base {
            Value::Array(items) => {
                let picked = slice(items, spec.start, spec.stop, spec.step);
                Value::Array(picked)
            }
            // Slicing a string is not supported by this subset.
            _ => Value::Null,
        },
        BracketSpec::Nested(items) => {
            // `a[[b]]` — flatten then index, with `b` evaluated against `a`.
            let Value::Array(list) = &base else {
                return Value::Null;
            };
            let idx_val = match items.first() {
                Some(e) => eval(e, doc),
                None => return Value::Null,
            };
            let Some(i) = idx_val.as_i64() else {
                return Value::Null;
            };
            let idx = if i < 0 { list.len() as i64 + i } else { i };
            let Value::Array(inner) = list.get(idx as usize).cloned().unwrap_or(Value::Null) else {
                return Value::Null;
            };
            Value::Array(inner)
        }
        BracketSpec::List(items) => {
            // Multi-select list: each element is evaluated against the base.
            Value::Array(items.iter().map(|i| eval(i, base)).collect())
        }
        BracketSpec::Filter(_) => Value::Null,
    }
}

/// The elements a projection produces from `base`.
fn projected(base: &Value) -> Vec<Value> {
    match base {
        Value::Array(items) => items.clone(),
        Value::Object(map) => ordered_keys(map)
            .into_iter()
            .map(|k| map.get(k).cloned().unwrap_or(Value::Null))
            .collect(),
        _ => Vec::new(),
    }
}

/// Python-compatible slice semantics: negative bounds count from the end,
/// out-of-range bounds clamp, and an absent step defaults to 1.
fn slice<T: Clone>(
    items: &[T],
    start: Option<i64>,
    stop: Option<i64>,
    step: Option<i64>,
) -> Vec<T> {
    let n = items.len() as i64;
    let step = step.unwrap_or(1);
    if step == 0 {
        // Python rejects a zero step; JMESPath defines it as an error.  We
        // return an empty projection rather than panicking.
        return vec![];
    }
    let norm = |v: i64| -> i64 {
        if v < 0 {
            (n + v).max(0)
        } else {
            v.min(n)
        }
    };
    let (lo, hi) = if step > 0 {
        let lo = match start {
            Some(v) if v < 0 => norm(v),
            Some(v) => v.min(n),
            None => 0,
        };
        let hi = match stop {
            Some(v) if v < 0 => norm(v),
            Some(v) => v.min(n),
            None => n,
        };
        (lo, hi)
    } else {
        let lo = match start {
            Some(v) if v < 0 => norm(v),
            Some(v) => v.min(n),
            None => n - 1,
        };
        let hi = match stop {
            Some(v) if v < 0 => norm(v),
            Some(v) => v.min(n),
            None => -1,
        };
        (lo, hi)
    };
    let mut out = Vec::new();
    if step > 0 {
        let mut i = lo;
        while i < hi {
            if i >= 0 && (i as usize) < items.len() {
                out.push(items[i as usize].clone());
            }
            i += step;
        }
    } else {
        let mut i = lo;
        while i > hi {
            if i >= 0 && (i as usize) < items.len() {
                out.push(items[i as usize].clone());
            }
            i += step;
        }
    }
    out
}

/// Built-in function semantics.
fn call(b: crate::ast::Builtin, args: &[Value]) -> Value {
    use crate::ast::Builtin;
    match b {
        Builtin::Length => match &args[0] {
            Value::String(s) => Value::from(s.chars().count()),
            Value::Array(a) => Value::from(a.len()),
            Value::Object(o) => Value::from(o.len()),
            _ => Value::Null,
        },
        Builtin::Values => match &args[0] {
            Value::Object(o) => Value::Array(
                ordered_keys(o)
                    .into_iter()
                    .map(|k| o.get(k).cloned().unwrap_or(Value::Null))
                    .collect(),
            ),
            _ => Value::Null,
        },
        Builtin::Keys => match &args[0] {
            Value::Object(o) => Value::Array(
                ordered_keys(o)
                    .into_iter()
                    .map(|k| Value::String(k.into()))
                    .collect(),
            ),
            _ => Value::Null,
        },
        Builtin::Merge => match &args[0] {
            Value::Object(o) => {
                // Deep merge per JMESPath: objects merge recursively, other
                // values are overwritten by the right-hand side.  With a single
                // argument this is a deep copy, which we implement by rebuilding
                // the map so that the result never aliases the input.
                Value::Object(deep_copy_map(o))
            }
            _ => Value::Null,
        },
        Builtin::Contains => {
            let needle = &args[1];
            match &args[0] {
                Value::String(hay) => {
                    let n = needle.as_str().unwrap_or("");
                    Value::Bool(hay.contains(n))
                }
                Value::Array(items) => Value::Bool(items.iter().any(|v| v == needle)),
                _ => Value::Null,
            }
        }
    }
}

/// Rebuild an object map, deep-copying nested containers so results are fully
/// owned and independent of the input.
fn deep_copy_map(o: &Map<String, Value>) -> Map<String, Value> {
    let mut out = Map::new();
    for k in ordered_keys(o) {
        out.insert(k.to_string(), o.get(k).cloned().unwrap_or(Value::Null));
    }
    out
}

/// The statically inferred result type of an already-annotated node, for
/// callers that want to report it alongside the value.
pub fn static_type(ast: &TypedAst) -> &Type {
    &ast.ty
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ast::Ast;
    use crate::lexer::Span;
    use serde_json::json;

    #[test]
    fn slice_semantics_match_python() {
        let a: Vec<i64> = vec![0, 1, 2, 3, 4, 5];
        assert_eq!(slice(&a, Some(1), Some(3), None), vec![1, 2]);
        assert_eq!(slice(&a, None, Some(2), None), vec![0, 1]);
        assert_eq!(slice(&a, Some(-2), None, None), vec![4, 5]);
        assert_eq!(slice(&a, None, None, Some(2)), vec![0, 2, 4]);
        assert_eq!(slice(&a, None, None, Some(-1)), vec![5, 4, 3, 2, 1, 0]);
        assert_eq!(slice(&a, Some(4), Some(1), Some(-1)), vec![4, 3, 2]);
        assert_eq!(slice(&a, Some(10), Some(20), None), Vec::<i64>::new());
        assert_eq!(slice(&a, None, None, Some(0)), Vec::<i64>::new());
    }

    #[test]
    fn object_traversal_is_sorted_by_key() {
        let o = json!({"z": 1, "a": 2, "m": 3});
        assert_eq!(ordered_keys(o.as_object().unwrap()), vec!["a", "m", "z"]);
    }

    #[test]
    fn builtins_behave() {
        assert_eq!(call(crate::ast::Builtin::Length, &[json!("abc")]), json!(3));
        assert_eq!(
            call(crate::ast::Builtin::Length, &[json!([1, 2])]),
            json!(2)
        );
        assert_eq!(
            call(crate::ast::Builtin::Keys, &[json!({"b": 1, "a": 2})]),
            json!(["a", "b"])
        );
        assert_eq!(
            call(crate::ast::Builtin::Values, &[json!({"b": 1, "a": 2})]),
            json!([2, 1])
        );
        assert_eq!(
            call(
                crate::ast::Builtin::Contains,
                &[json!("hello"), json!("ell")]
            ),
            json!(true)
        );
        assert_eq!(
            call(crate::ast::Builtin::Contains, &[json!([1, 2]), json!(2)]),
            json!(true)
        );
        assert_eq!(
            call(crate::ast::Builtin::Contains, &[json!([1, 2]), json!(3)]),
            json!(false)
        );
        assert_eq!(
            call(crate::ast::Builtin::Merge, &[json!({"a": 1})]),
            json!({"a": 1})
        );
    }

    #[test]
    fn missing_paths_yield_null() {
        let d = json!({"a": {"b": 1}});
        let root = Ast::new(AstKind::Root, Span::dummy());
        let node = Ast::new(AstKind::Field(Box::new(root), "zzz".into()), Span::dummy());
        assert_eq!(eval(&node, &d), Value::Null);
        assert_eq!(lookup(&d, "a"), json!({"b": 1}));
    }

    #[test]
    fn flatten_and_project() {
        let d = json!({"a": [[1, 2], [3], 4]});
        let root = Ast::new(AstKind::Root, Span::dummy());
        let field = Ast::new(
            AstKind::Field(Box::new(root.clone()), "a".into()),
            Span::dummy(),
        );
        let flat = Ast::new(AstKind::Flatten(Box::new(field.clone())), Span::dummy());
        assert_eq!(eval(&flat, &d), json!([1, 2, 3, 4]));
        let proj = Ast::new(AstKind::Wildcard(Box::new(field.clone())), Span::dummy());
        assert_eq!(eval(&proj, &d), json!([[1, 2], [3], 4]));
        // `a || b` projects `a` and applies `b` to every element.  Here `b`
        // does not exist, so each lookup yields null.
        let flatten_proj = Ast::new(
            AstKind::Projection(
                Box::new(field.clone()),
                Box::new(Ast::new(AstKind::Ident("b".into()), Span::dummy())),
            ),
            Span::dummy(),
        );
        assert_eq!(
            eval(&flatten_proj, &d),
            json!([Value::Null, Value::Null, Value::Null])
        );
        // `a || [0]` takes the first element of each projected element.
        let first_elem = Ast::new(
            AstKind::Projection(
                Box::new(field),
                Box::new(Ast::new(
                    AstKind::Bracket(
                        Box::new(Ast::new(AstKind::Current, Span::dummy())),
                        BracketSpec::Index(0),
                    ),
                    Span::dummy(),
                )),
            ),
            Span::dummy(),
        );
        assert_eq!(eval(&first_elem, &d), json!([1, 3, Value::Null]));
    }

    #[test]
    fn negative_index_counts_from_end() {
        let d = json!([10, 20, 30]);
        let root = Ast::new(AstKind::Root, Span::dummy());
        let node = Ast::new(
            AstKind::Bracket(Box::new(root), BracketSpec::Index(-1)),
            Span::dummy(),
        );
        assert_eq!(eval(&node, &d), json!(30));
        let oob = Ast::new(
            AstKind::Bracket(
                Box::new(Ast::new(AstKind::Root, Span::dummy())),
                BracketSpec::Index(9),
            ),
            Span::dummy(),
        );
        assert_eq!(eval(&oob, &d), Value::Null);
    }

    #[test]
    fn non_array_index_is_null() {
        let d = json!({"a": 5});
        let node = Ast::new(
            AstKind::Bracket(
                Box::new(Ast::new(AstKind::Root, Span::dummy())),
                BracketSpec::Index(0),
            ),
            Span::dummy(),
        );
        assert_eq!(eval(&node, &d), Value::Null);
    }
}
