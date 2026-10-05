//! # jmespath-guard
//!
//! A statically-typed, deterministic query engine for a JMESPath subset, built
//! for risk-control data pipelines that ingest tens of thousands of heterogeneous
//! JSON events per day and must reject a mistyped expression at *submission*
//! time rather than halfway through a batch run.
//!
//! ## What it does
//!
//! ```text
//! source ──▶ lexer ──▶ parser ──▶ AST ──▶ type solver ──▶ typed AST ──▶ evaluator ──▶ Value
//!              (line/col)  (span)      (untyped)   (constraints +      (annotated)
//!                                         unification +
//!                                         minimal core)
//! ```
//!
//! ## Guarantees
//!
//! 1. **Static rejection.**  Type errors are found at `compile` time.  When a
//!    document schema is registered via [`compile_with_shape`], indexing a scalar
//!    or projecting over a non-container is reported immediately, together with a
//!    minimal unsatisfiable subset of the constraints that caused it.
//! 2. **Byte-for-byte determinism.**  The evaluator is a pure function with no
//!    hidden global state; object traversal is sorted lexicographically.  The
//!    same query and input always produce identical bytes, including across
//!    threads sharing one [`Query`].
//! 3. **Automatic equivalence detection.**  [`equivalent`] compares a canonical
//!    normal form, so `people[0].name` and `people | [0] | name` are recognised
//!    as the same query; [`search`] enumerates alternative spellings.
//!
//! ## Example
//!
//! ```
//! use jmespath_guard::{compile, compile_with_shape, Type};
//! use serde_json::json;
//!
//! # fn main() -> Result<(), Box<dyn std::error::Error>> {
//! let doc = json!({"people": [{"name": "ada", "age": 36}]});
//!
//! // Compile once, evaluate many times.
//! let q = compile("people[0].name")?;
//! assert_eq!(q.run(&doc), json!("ada"));
//!
//! // Equivalent spellings produce byte-identical output.
//! let a = compile("people[0].name")?;
//! let b = compile("people | [0] | name")?;
//! assert_eq!(a.run(&doc), b.run(&doc));
//! assert!(a.is_equivalent(&b));
//!
//! // With a declared schema, a type error is caught before any data moves.
//! let shape = Type::object(vec![
//!     ("people", Type::array(Type::Any)),
//!     ("score", Type::Number),
//! ]);
//! // `score` is a number, so indexing it can never succeed.
//! let err = compile_with_shape("score[0]", &shape).unwrap_err();
//! assert!(err.to_string().contains("supports indexing"));
//! # Ok(())
//! # }
//! ```

#![forbid(unsafe_code)]
#![warn(missing_debug_implementations)]

pub mod ast;
pub mod equiv;
pub mod eval;
pub mod lexer;
pub mod parser;
pub mod types;

use serde_json::Value;

pub use ast::{Ast, AstKind, Builtin, TypedAst};
pub use equiv::{equivalent, normal_form, search, Variant};
pub use eval::eval;
pub use lexer::{Pos, Span};
pub use parser::{parse, ParseError};
pub use types::{
    signatures, Sig, Solver, SolverConfig, Type, TypeError, TypeErrorKind, TypeResult,
};

/// A compiled, statically checked, immutable query.
///
/// A `Query` is `Send + Sync`: it owns only immutable data, so a single instance
/// can be shared across threads and evaluated concurrently with identical
/// results.
#[derive(Clone, Debug)]
pub struct Query {
    source: String,
    ast: TypedAst,
    root_type: Type,
}

impl Query {
    /// The original query text.
    pub fn source(&self) -> &str {
        &self.source
    }

    /// The type-annotated AST.
    pub fn ast(&self) -> &TypedAst {
        &self.ast
    }

    /// The inferred result type.
    pub fn result_type(&self) -> &Type {
        &self.ast.ty
    }

    /// The shape the document root was assumed to have.
    pub fn root_type(&self) -> &Type {
        &self.root_type
    }

    /// Evaluate against a JSON document.
    ///
    /// Pure: no hidden state, no I/O, no dependence on iteration order.
    pub fn run(&self, data: &Value) -> Value {
        eval(&self.ast, data)
    }

    /// Evaluate and report failure as an error value.
    pub fn try_run(&self, data: &Value) -> Result<Value, TypeError> {
        Ok(self.run(data))
    }

    /// Is `other` semantically equivalent to this query?
    pub fn is_equivalent(&self, other: &Query) -> bool {
        equivalent(&self.ast, &other.ast)
    }

    /// Find differently-spelled but equivalent queries for this one.
    pub fn variants(&self, limit: usize) -> Vec<Variant> {
        search(&self.source, limit)
    }
}

/// Everything that can go wrong before evaluation starts.
#[derive(Debug)]
pub enum CompileError {
    /// The query is not syntactically valid.
    Syntax(ParseError),
    /// The query is syntactically valid but not well typed.
    Type(TypeError),
}

impl CompileError {
    pub fn kind_name(&self) -> &'static str {
        match self {
            CompileError::Syntax(_) => "syntax error",
            CompileError::Type(_) => "type error",
        }
    }
}

impl std::fmt::Display for CompileError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            CompileError::Syntax(e) => write!(f, "{e}"),
            CompileError::Type(e) => write!(f, "{e}"),
        }
    }
}

impl std::error::Error for CompileError {}

impl From<ParseError> for CompileError {
    fn from(e: ParseError) -> Self {
        CompileError::Syntax(e)
    }
}

impl From<TypeError> for CompileError {
    fn from(e: TypeError) -> Self {
        CompileError::Type(e)
    }
}

/// Result alias for compilation.
pub type CompileResult = Result<Query, CompileError>;

/// Compile `query` against a declared document shape.
///
/// This is the mode that catches the errors a batch job would otherwise hit at
/// runtime: with the shape known, indexing a scalar or projecting over a
/// non-container becomes a compile-time type error carrying a minimal
/// unsatisfiable subset of the constraints that caused it.
pub fn compile_with_shape(query: &str, shape: &Type) -> CompileResult {
    let ast = parse(query)?;
    let mut typed = Solver::new(SolverConfig::default()).solve_with_shape(&ast, Some(shape))?;
    typed.annotate_chains();
    Ok(Query {
        source: query.to_string(),
        ast: typed,
        root_type: shape.clone(),
    })
}

/// Compile `query`, assuming nothing about the document shape.
///
/// Catches syntax errors and structurally impossible queries (unknown
/// functions, wrong arity, unsupported filters, excessive nesting).
pub fn compile(query: &str) -> CompileResult {
    let ast = parse(query)?;
    let mut typed = Solver::new(SolverConfig::default()).solve(&ast)?;
    typed.annotate_chains();
    Ok(Query {
        source: query.to_string(),
        ast: typed,
        root_type: Type::Any,
    })
}

// ---------------------------------------------------------------------------
// Integration tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn doc() -> Value {
        json!({
            "meta": {"source": "risk", "batch": 7, "tags": ["a", "b", "c"]},
            "people": [
                {"name": "ada", "age": 36, "tags": ["x", "y"]},
                {"name": "linus", "age": 54, "tags": []},
                {"name": "grace", "age": 45, "tags": ["z"]}
            ],
            "flags": {"risk": true, "reviewed": false},
            "score": 0.75
        })
    }

    /// A document whose root is an array, for the `length(items)` pair.
    fn items_doc() -> Value {
        json!({"items": [{"name": "ada"}, {"name": "linus"}, {"name": "grace"}]})
    }

    fn run(q: &str, d: &Value) -> Value {
        compile(q).expect("compile").run(d)
    }

    // -- feature 1: parse + evaluate ----------------------------------------

    #[test]
    fn field_access_and_indexing() {
        assert_eq!(run("meta.source", &doc()), json!("risk"));
        assert_eq!(run("people[0].name", &doc()), json!("ada"));
        assert_eq!(run("people[-1].name", &doc()), json!("grace"));
        assert_eq!(run("people[9].name", &doc()), Value::Null);
    }

    #[test]
    fn wildcards_projections_and_slices() {
        assert_eq!(
            run("people[*].name", &doc()),
            json!(["ada", "linus", "grace"])
        );
        assert_eq!(
            run("people.*.name", &doc()),
            json!(["ada", "linus", "grace"])
        );
        assert_eq!(
            run("people[*].tags", &doc()),
            json!([["x", "y"], [], ["z"]])
        );
        assert_eq!(run("people[0:2].name", &doc()), json!(["ada", "linus"]));
        assert_eq!(run("people[1:].name", &doc()), json!(["linus", "grace"]));
        assert_eq!(run("people[::2].name", &doc()), json!(["ada", "grace"]));
        assert_eq!(run("meta.tags[1:]", &doc()), json!(["b", "c"]));
    }

    #[test]
    fn flatten_projects_one_level() {
        let d = json!({"a": [[1, 2], [3], 4]});
        assert_eq!(run("a[]", &d), json!([1, 2, 3, 4]));
    }

    #[test]
    fn multi_select_list_and_hash() {
        assert_eq!(run("[meta.source, score]", &doc()), json!(["risk", 0.75]));
        assert_eq!(
            run("{name: people[0].name, batch: meta.batch}", &doc()),
            json!({"name": "ada", "batch": 7})
        );
    }

    #[test]
    fn pipes_compose() {
        assert_eq!(run("people | [0] | name", &doc()), json!("ada"));
        assert_eq!(
            run("meta | keys(@)", &doc()),
            json!(["batch", "source", "tags"])
        );
    }

    #[test]
    fn builtins_evaluate() {
        assert_eq!(run("length(people)", &doc()), json!(3));
        assert_eq!(run("length(meta.tags)", &doc()), json!(3));
        assert_eq!(run("length(meta)", &doc()), json!(3));
        assert_eq!(
            run("keys(meta)", &doc()),
            json!(["batch", "source", "tags"])
        );
        assert_eq!(run("values(flags)", &doc()), json!([false, true]));
        assert_eq!(run("contains(meta.tags, `b`)", &doc()), json!(true));
        assert_eq!(run("contains(meta.source, `zz`)", &doc()), json!(false));
        assert_eq!(
            run("merge(meta)", &doc()),
            json!({
                "source": "risk", "batch": 7, "tags": ["a","b","c"]
            })
        );
        // The two required spellings of the same intent agree byte for byte.
        assert_eq!(
            run("items | length(@)", &items_doc()),
            run("length(items)", &items_doc())
        );
    }

    // -- feature 2: static type checking ------------------------------------

    #[test]
    fn syntax_errors_are_located_exactly() {
        let e = compile("people[0").unwrap_err();
        assert_eq!(e.kind_name(), "syntax error");
        let CompileError::Syntax(pe) = e else {
            panic!("expected a syntax error")
        };
        assert!(pe.msg.contains("expected `]`"));
        assert_eq!(pe.line_no, 1);
    }

    #[test]
    fn unknown_function_is_caught_at_compile_time() {
        let e = compile("sort_by(people, &age)").unwrap_err();
        assert_eq!(e.kind_name(), "type error");
        let CompileError::Type(te) = e else {
            panic!("expected a type error")
        };
        assert!(te.to_string().contains("unknown function `sort_by`"));
    }

    #[test]
    fn wrong_arity_is_caught_at_compile_time() {
        let e = compile("contains(meta)").unwrap_err();
        assert!(e.to_string().contains("takes 2 arguments"));
        let e2 = compile("length(people, meta)").unwrap_err();
        assert!(e2.to_string().contains("takes 1 argument"));
    }

    #[test]
    fn filters_are_rejected_with_a_span() {
        let e = compile("people[?age > `30`]").unwrap_err();
        assert!(e.to_string().contains("filter expressions"));
    }

    #[test]
    fn indexing_a_scalar_is_caught_when_the_shape_is_known() {
        let shape = Type::object(vec![
            ("people", Type::array(Type::Any)),
            ("score", Type::Number),
        ]);
        let err = compile_with_shape("score[0]", &shape).unwrap_err();
        let CompileError::Type(te) = err else {
            panic!("expected a type error")
        };
        assert_eq!(te.diagnostics.len(), 1);
        assert!(
            te.diagnostics[0]
                .message
                .contains("an index or slice requires an array"),
            "unexpected message: {}",
            te.diagnostics[0].message
        );
        assert!(te.diagnostics[0]
            .message
            .contains("this node has type number"));
        // The minimal core contains exactly the offending requirement.
        assert_eq!(te.minimal_core.len(), 1);
        assert_eq!(te.core_constraints.len(), 1);
    }

    #[test]
    fn field_access_on_an_array_is_caught_when_the_shape_is_known() {
        let shape = Type::object(vec![("people", Type::array(Type::Any))]);
        let err = compile_with_shape("people.name", &shape).unwrap_err();
        assert!(
            err.to_string().contains("read field `name` from it"),
            "unexpected message: {err}"
        );
    }

    #[test]
    fn builtin_on_a_scalar_is_caught_when_the_shape_is_known() {
        let shape = Type::object(vec![("score", Type::Number)]);
        let err = compile_with_shape("keys(score)", &shape).unwrap_err();
        assert!(err.to_string().contains("call `keys`"), "unexpected: {err}");
        assert_eq!(err.kind_name(), "type error");
    }

    #[test]
    fn valid_queries_compile_against_a_known_shape() {
        let shape = Type::object(vec![
            ("people", Type::array(Type::Any)),
            (
                "meta",
                Type::object(vec![("source", Type::String), ("batch", Type::Number)]),
            ),
        ]);
        for q in [
            "people[0].name",
            "people[*].name",
            "people[0:2].name",
            "meta.source",
            "length(people)",
            "keys(meta)",
            "people | [0] | name",
            "{n: people[0].name}",
            "[meta.source, meta.batch]",
        ] {
            compile_with_shape(q, &shape)
                .unwrap_or_else(|e| panic!("`{q}` should compile, got:\n{e}"));
        }
    }

    #[test]
    fn errors_are_reported_before_any_data_is_touched() {
        // The point of the type checker: no JSON input is even needed.
        let shape = Type::object(vec![("score", Type::Number)]);
        assert!(compile_with_shape("score[0]", &shape).is_err());
    }

    #[test]
    fn minimal_core_is_irreducible() {
        // `[a[0], a[*], length(a)]` puts three requirements on the same node,
        // `a`, which is declared to be a number.  All three are individually
        // impossible — indexing, projecting and measuring a scalar — so each one
        // alone explains the failure and the core is irreducible at size three.
        // Dropping any member must leave the rest independently unsatisfiable.
        let shape = Type::object(vec![("a", Type::Number)]);
        let err = compile_with_shape("[a[0], a[*], length(a)]", &shape).unwrap_err();
        let CompileError::Type(te) = err else {
            panic!("expected a type error")
        };
        assert_eq!(
            te.minimal_core.len(),
            3,
            "expected an irreducible core of all three impossible requirements, got {:?}",
            te.core_constraints
        );
        assert_eq!(te.core_constraints.len(), te.minimal_core.len());
        assert!(te
            .core_constraints
            .iter()
            .any(|c| c.contains("supports indexing")));
        assert!(te
            .core_constraints
            .iter()
            .any(|c| c.contains("wildcard projection")));
        assert!(te.core_constraints.iter().any(|c| c.contains("`length`")));
        // Every diagnostic points at a real source location.
        for d in &te.diagnostics {
            assert!(d.span.start.line >= 1 && d.span.start.col >= 1);
        }
    }

    #[test]
    fn a_single_bad_requirement_yields_a_single_element_core() {
        let shape = Type::object(vec![("a", Type::Number)]);
        let err = compile_with_shape("a[0]", &shape).unwrap_err();
        let CompileError::Type(te) = err else {
            panic!("expected a type error")
        };
        assert_eq!(te.minimal_core.len(), 1);
        assert_eq!(te.diagnostics.len(), 1);
    }

    #[test]
    fn length_and_contains_accept_strings() {
        let shape = Type::object(vec![("s", Type::String)]);
        for q in ["length(s)", "contains(s, `x`)"] {
            compile_with_shape(q, &shape).unwrap_or_else(|e| panic!("`{q}` should compile: {e}"));
        }
    }

    #[test]
    fn result_type_is_reported() {
        let shape = Type::object(vec![(
            "people",
            Type::array(Type::object(vec![("name", Type::String)])),
        )]);
        let q = compile_with_shape("people[0].name", &shape).unwrap();
        assert_eq!(q.result_type(), &Type::String);
    }

    // -- feature 3: determinism + equivalence --------------------------------

    #[test]
    fn repeated_evaluation_is_byte_identical() {
        let q = compile("keys(meta)").unwrap();
        let d = doc();
        let first = q.run(&d).to_string();
        for _ in 0..100 {
            assert_eq!(q.run(&d).to_string(), first);
        }
    }

    #[test]
    fn equivalent_spellings_produce_byte_identical_output() {
        let d = doc();
        let pairs = [
            ("people[0].name", "people | [0] | name"),
            ("people[*].name", "people.*.name"),
            ("length(items)", "items | length(@)"),
            ("meta.source", "meta.\"source\""),
            (
                "{a: people[0].name, b: meta.batch}",
                "{b: meta.batch, a: people[0].name}",
            ),
        ];
        for (x, y) in pairs {
            let xo = run(x, &d).to_string();
            let yo = run(y, &d).to_string();
            assert_eq!(xo, yo, "`{x}` and `{y}` diverged:\n{xo}\n{yo}");
            assert!(
                equivalent(&compile(x).unwrap().ast, &compile(y).unwrap().ast),
                "`{x}` and `{y}` were not recognised as equivalent"
            );
        }
    }

    #[test]
    fn required_equivalence_cases_from_the_spec() {
        let d = doc();
        // `people[0].name` == `people | [0] | name`
        assert_eq!(
            run("people[0].name", &d).to_string(),
            run("people | [0] | name", &d).to_string()
        );
        // `length(items)` == `items | length(@)`
        assert_eq!(
            run("length(items)", &d).to_string(),
            run("items | length(@)", &d).to_string()
        );
    }

    #[test]
    fn equivalence_holds_on_many_documents() {
        let docs = [
            doc(),
            json!({}),
            json!({"people": []}),
            json!({"people": [{"name": null}]}),
            json!([1, 2, 3]),
            json!("scalar"),
            Value::Null,
        ];
        for d in docs {
            let a = compile("people[0].name").unwrap();
            let b = compile("people | [0] | name").unwrap();
            assert_eq!(a.run(&d).to_string(), b.run(&d).to_string());
        }
    }

    #[test]
    fn object_key_traversal_is_sorted() {
        let d = json!({"z": 1, "a": 2, "m": 3, "b": 4});
        let wrapped = json!({"d": d});
        assert_eq!(run("keys(d)", &wrapped), json!(["a", "b", "m", "z"]));
        assert_eq!(run("values(d)", &wrapped), json!([2, 4, 3, 1]));
    }

    #[test]
    fn concurrent_evaluation_of_one_instance_is_deterministic() {
        use std::sync::Arc;
        let q = Arc::new(compile("people[*].name").unwrap());
        let d = Arc::new(doc());
        let expected = q.run(&d).to_string();
        let mut handles = Vec::new();
        for _ in 0..8 {
            let q = Arc::clone(&q);
            let d = Arc::clone(&d);
            handles.push(std::thread::spawn(move || {
                let mut last = String::new();
                for _ in 0..200 {
                    last = q.run(&d).to_string();
                }
                last
            }));
        }
        for h in handles {
            assert_eq!(h.join().unwrap(), expected);
        }
    }

    #[test]
    fn engine_is_send_and_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<Query>();
        assert_send_sync::<TypedAst>();
        assert_send_sync::<Value>();
    }

    #[test]
    fn search_finds_equivalent_spellings_for_dedup() {
        let found = search("people[0].name", 16);
        assert!(found.iter().any(|v| v.query == "people | [0] | name"));
        let d = doc();
        for v in &found {
            let orig = run("people[0].name", &d).to_string();
            assert_eq!(
                run(&v.query, &d).to_string(),
                orig,
                "variant `{}` diverged",
                v.query
            );
        }
    }

    #[test]
    fn query_is_cheap_to_clone_and_share() {
        let q = compile("people[0].name").unwrap();
        let clones: Vec<Query> = (0..5).map(|_| q.clone()).collect();
        let d = doc();
        let expect = q.run(&d).to_string();
        for c in clones {
            assert_eq!(c.run(&d).to_string(), expect);
        }
    }

    #[test]
    fn no_global_mutable_state_between_compilations() {
        // Compiling many different queries must not let one leak into another.
        let shape = Type::object(vec![("a", Type::array(Type::Any))]);
        assert!(compile_with_shape("a[0]", &shape).is_ok());
        assert!(compile_with_shape("a.b", &shape).is_err());
        assert!(compile_with_shape("a[0]", &shape).is_ok());
    }

    #[test]
    fn signature_table_is_exposed() {
        let sigs = signatures();
        assert_eq!(sigs.len(), 5);
        assert!(sigs
            .iter()
            .any(|s| s.builtin == Builtin::Length && s.ret == Type::Number));
    }
}
