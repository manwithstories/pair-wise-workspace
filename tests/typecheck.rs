//! Static type inference: what must be caught at submission time, and what
//! must be let through.
//!
//! The dividing line is deliberate.  A query is rejected when it is
//! *structurally* impossible (unknown function, wrong arity, unsupported
//! filter) or when a declared document shape makes it *concretely* impossible
//! (indexing a scalar, projecting over a non-container, reading a field of a
//! number).  Everything else is left to evaluation, because JMESPath's
//! "absent data yields null" rule makes almost every shape question
//! unanswerable without real data — and a checker that guessed would be worse
//! than one that stays quiet.

use jmespath_guard::{compile, compile_with_shape, CompileError, Type, TypeError};

/// The registered envelope schema for one upstream system.
fn risk_schema() -> Type {
    Type::object(vec![
        (
            "meta",
            Type::object(vec![
                ("source", Type::String),
                ("batch", Type::Number),
                ("tags", Type::array(Type::String)),
            ]),
        ),
        (
            "people",
            Type::array(Type::object(vec![
                ("name", Type::String),
                ("age", Type::Number),
            ])),
        ),
        ("score", Type::Number),
        ("flagged", Type::Bool),
    ])
}

fn expect_type_error(q: &str, shape: &Type) -> TypeError {
    match compile_with_shape(q, shape) {
        Ok(_) => panic!("`{q}` should have been rejected, but compiled successfully"),
        Err(CompileError::Type(e)) => e,
        Err(CompileError::Syntax(e)) => panic!("`{q}` produced a syntax error instead: {e}"),
    }
}

// ---------------------------------------------------------------------------
// Structural errors: caught with no schema at all
// ---------------------------------------------------------------------------

#[test]
fn unknown_functions_are_rejected_before_any_data_exists() {
    // Note: `people.map(&name)` is a *syntax* error, not an unknown function —
    // `.map(` parses as a field access followed by an unopened bracket, which
    // the parser rejects before the type checker ever runs.
    for q in [
        "sort_by(people, &age)",
        "max(people)",
        "to_number(score)",
        "not_a_function(people)",
    ] {
        let err = compile(q).unwrap_err();
        assert_eq!(err.kind_name(), "type error", "`{q}`");
        assert!(err.to_string().contains("unknown function"), "`{q}`: {err}");
    }
}

#[test]
fn arity_is_checked() {
    for (q, expected) in [
        ("contains(meta)", "takes 2 arguments"),
        ("length(people, meta)", "takes 1 argument"),
        ("keys()", "takes 1 argument"),
        ("merge(a, b)", "takes 1 argument"),
    ] {
        let err = compile(q).unwrap_err();
        assert!(err.to_string().contains(expected), "`{q}`: {err}");
    }
}

#[test]
fn filters_are_rejected_with_a_precise_location() {
    for q in ["people[?age > `30`]", "people[?name == 'ada']"] {
        let err = compile(q).unwrap_err();
        assert_eq!(err.kind_name(), "type error", "`{q}`");
        let text = err.to_string();
        assert!(text.contains("filter expressions"), "`{q}`: {text}");
        // The reported position must point into the query, not at byte zero.
        let CompileError::Type(te) = &err else {
            unreachable!()
        };
        let col = te.diagnostics[0].span.start.col;
        assert!(
            col > 1,
            "`{q}` reported column {col}, expected the `[?` position"
        );
    }
}

#[test]
fn syntax_errors_report_line_and_column_with_a_caret() {
    let err = compile("people[0").unwrap_err();
    let text = err.to_string();
    assert!(text.contains("syntax error at line 1"), "{text}");
    assert!(text.contains("column 9"), "{text}");
    assert!(text.contains('^'), "expected a caret marker:\n{text}");

    let err = compile("people\n  .[?bad]").unwrap_err();
    assert!(err.to_string().contains("line 2"), "{err}");
}

// ---------------------------------------------------------------------------
// Concrete errors: caught once a schema is registered
// ---------------------------------------------------------------------------

#[test]
fn indexing_a_scalar_is_caught() {
    for q in ["score[0]", "score[0:2]", "flagged[0]", "score | [0]"] {
        let te = expect_type_error(q, &risk_schema());
        assert!(
            te.diagnostics[0]
                .message
                .contains("an index or slice requires an array"),
            "`{q}`: {}",
            te.diagnostics[0].message
        );
    }
}

#[test]
fn reading_a_field_of_a_scalar_is_caught() {
    for q in ["score.name", "flagged.name", "meta.batch.value"] {
        let te = expect_type_error(q, &risk_schema());
        assert!(
            te.diagnostics[0].message.contains("read field"),
            "`{q}`: {}",
            te.diagnostics[0].message
        );
    }
}

#[test]
fn projecting_over_a_scalar_is_caught() {
    for q in ["score[*]", "score.*", "flagged[*]"] {
        expect_type_error(q, &risk_schema());
    }
}

#[test]
fn flattening_a_scalar_is_caught() {
    expect_type_error("score[]", &risk_schema());
    expect_type_error("flagged[]", &risk_schema());
}

#[test]
fn object_only_builtins_reject_scalars() {
    for q in [
        "keys(score)",
        "values(score)",
        "merge(score)",
        "keys(people)",
    ] {
        let te = expect_type_error(q, &risk_schema());
        assert!(
            te.diagnostics[0].message.contains("call `"),
            "`{q}`: {}",
            te.diagnostics[0].message
        );
    }
}

#[test]
fn length_rejects_numbers_and_booleans_but_accepts_the_rest() {
    // Scalars have no length.
    expect_type_error("length(score)", &risk_schema());
    expect_type_error("length(flagged)", &risk_schema());
    // Strings, arrays and objects do.
    for q in ["length(meta)", "length(people)", "length(meta.tags)"] {
        compile_with_shape(q, &risk_schema())
            .unwrap_or_else(|e| panic!("`{q}` should be accepted: {e}"));
    }
}

#[test]
fn contains_requires_a_string_or_array_haystack() {
    expect_type_error("contains(score, `x`)", &risk_schema());
    expect_type_error("contains(flagged, `x`)", &risk_schema());
    // An array *is* a valid haystack, even of objects: membership is by value.
    compile_with_shape("contains(people, `x`)", &risk_schema()).unwrap();
    for q in ["contains(meta.source, `r`)", "contains(meta.tags, `auto`)"] {
        compile_with_shape(q, &risk_schema())
            .unwrap_or_else(|e| panic!("`{q}` should be accepted: {e}"));
    }
}

// ---------------------------------------------------------------------------
// What must be accepted
// ---------------------------------------------------------------------------

#[test]
fn valid_queries_compile_against_a_known_schema() {
    for q in [
        "meta.source",
        "meta.tags[0]",
        "meta.tags[1:]",
        "people[0].name",
        "people[-1].name",
        "people[*].name",
        "people[0:2].age",
        "people.*.name",
        "people[0].name | [0]",
        "people | [0] | name",
        "length(people)",
        "keys(meta)",
        "values(meta)",
        "merge(meta)",
        "contains(meta.tags, `auto`)",
        "[meta.source, meta.batch]",
        "{name: people[0].name, batch: meta.batch}",
        "{n: people[0].name, a: people[0].age}",
        "people[] | [0]",
    ] {
        compile_with_shape(q, &risk_schema())
            .unwrap_or_else(|e| panic!("`{q}` should compile, got:\n{e}"));
    }
}

#[test]
fn missing_fields_are_not_errors_because_they_yield_null() {
    // JMESPath turns an absent key into null, so reading one is legitimate even
    // when the schema does not list it.  A checker that rejected these would be
    // unusable against heterogeneous upstream data.
    for q in [
        "meta.missing",
        "people[0].nickname",
        "people[0].missing.deep",
    ] {
        compile_with_shape(q, &risk_schema())
            .unwrap_or_else(|e| panic!("`{q}` should compile: {e}"));
    }
    let d = serde_json::json!({"meta": {"source": "risk"}});
    assert_eq!(
        compile("meta.missing").unwrap().run(&d),
        serde_json::Value::Null
    );
}

#[test]
fn indexing_a_scalar_object_is_caught() {
    let shape = Type::object(vec![("meta", Type::object(vec![("source", Type::String)]))]);
    expect_type_error("meta[0]", &shape);
}

// ---------------------------------------------------------------------------
// The minimal unsatisfiable subset
// ---------------------------------------------------------------------------

#[test]
fn every_rejection_carries_a_minimal_core() {
    for q in [
        "score[0]",
        "score.name",
        "keys(score)",
        "length(score)",
        "people.name",
        "score[]",
    ] {
        let te = expect_type_error(q, &risk_schema());
        assert!(
            !te.minimal_core.is_empty(),
            "`{q}` was rejected without a minimal core"
        );
        assert_eq!(
            te.minimal_core.len(),
            te.core_constraints.len(),
            "`{q}`: core indices and rendered constraints disagree"
        );
        for c in &te.core_constraints {
            assert!(!c.is_empty(), "`{q}` produced an empty core entry");
        }
    }
}

#[test]
fn a_single_fault_yields_a_single_element_core() {
    let te = expect_type_error("score[0]", &risk_schema());
    assert_eq!(te.minimal_core.len(), 1);
    assert_eq!(te.diagnostics.len(), 1);
}

#[test]
fn several_independent_faults_are_all_reported() {
    // Three independent scalar violations on one node: the core is irreducible
    // because every member is unsatisfiable on its own.
    let shape = Type::object(vec![("a", Type::Number)]);
    let te = expect_type_error("[a[0], a[*], length(a)]", &shape);
    assert!(te.minimal_core.len() >= 2, "expected several core members");
    assert!(te.core_constraints.iter().any(|c| c.contains("indexing")));
    assert!(te.core_constraints.iter().any(|c| c.contains("length")));
}

#[test]
fn the_core_explains_the_diagnostic() {
    let te = expect_type_error("score[0]", &risk_schema());
    let report = te.to_string();
    assert!(report.contains("minimal unsatisfiable subset"), "{report}");
    assert!(report.contains("number"), "{report}");
}

// ---------------------------------------------------------------------------
// Inference
// ---------------------------------------------------------------------------

#[test]
fn result_types_are_inferred_through_the_pipeline() {
    let shape = risk_schema();
    let cases = [
        ("meta.source", Type::String),
        ("meta.batch", Type::Number),
        ("flagged", Type::Bool),
        ("meta.tags", Type::array(Type::String)),
        ("meta.tags[0]", Type::String),
        ("length(meta)", Type::Number),
        ("contains(meta.tags, `a`)", Type::Bool),
        ("keys(meta)", Type::array(Type::String)),
    ];
    for (q, want) in cases {
        let got = compile_with_shape(q, &shape)
            .unwrap_or_else(|e| panic!("`{q}` should compile: {e}"))
            .result_type()
            .clone();
        assert_eq!(got, want, "`{q}` inferred {got}, expected {want}");
    }
}

#[test]
fn the_root_shape_is_recorded_on_the_query() {
    let shape = risk_schema();
    let q = compile_with_shape("meta.source", &shape).unwrap();
    assert_eq!(q.root_type(), &shape);
    let q = compile("meta.source").unwrap();
    assert_eq!(q.root_type(), &Type::Any);
}

// ---------------------------------------------------------------------------
// Statelessness
// ---------------------------------------------------------------------------

#[test]
fn compilation_has_no_hidden_state_between_calls() {
    let shape = risk_schema();
    // Interleaving accepting and rejecting queries must not let one leak into
    // the next: a checker that cached "the last query was fine" would fail here.
    for _ in 0..50 {
        compile_with_shape("people[0].name", &shape).unwrap();
        assert!(compile_with_shape("score[0]", &shape).is_err());
        compile_with_shape("keys(meta)", &shape).unwrap();
        assert!(compile_with_shape("score.name", &shape).is_err());
        compile("meta.source").unwrap();
        assert!(compile("sort_by(x, &y)").is_err());
    }
}

#[test]
fn the_same_query_always_produces_the_same_verdict() {
    let shape = risk_schema();
    let good = "people[0].name";
    let bad = "score[0]";
    for _ in 0..50 {
        assert!(compile_with_shape(good, &shape).is_ok());
        let e1 = compile_with_shape(bad, &shape).unwrap_err().to_string();
        let e2 = compile_with_shape(bad, &shape).unwrap_err().to_string();
        assert_eq!(e1, e2, "two identical compilations disagreed");
    }
}

#[test]
fn queries_are_send_and_sync() {
    fn assert_send_sync<T: Send + Sync>() {}
    assert_send_sync::<jmespath_guard::Query>();
    assert_send_sync::<jmespath_guard::TypedAst>();
    assert_send_sync::<Type>();
}
