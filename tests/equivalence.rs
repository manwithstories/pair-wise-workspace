//! Semantic-equivalence and determinism guarantees.
//!
//! The specification requires, at minimum, that `people[0].name` and
//! `people | [0] | name` agree, and that `length(items)` and `items | length(@)`
//! agree.  This file asserts those two pairs explicitly and then widens the
//! guarantee to a catalogue of differently-spelled queries evaluated against
//! many documents, including hostile ones (empty, scalar, null, deeply nested).

use jmespath_guard::{compile, equivalent, normal_form, search};
use serde_json::{json, Value};

/// The document every equivalence check is run against.
fn doc() -> Value {
    json!({
        "meta": {
            "source": "risk",
            "batch": 20240101,
            "tags": ["urgent", "manual", "auto"],
            "nested": {"deep": {"value": 42}}
        },
        "people": [
            {"name": "ada",   "age": 36, "tags": ["x", "y"], "score": 0.9},
            {"name": "linus", "age": 54, "tags": [],          "score": 0.5},
            {"name": "grace", "age": 45, "tags": ["z"],       "score": 0.7}
        ],
        "items": [10, 20, 30, 40, 50],
        "flags": {"risk": true, "reviewed": false, "blocked": false},
        "score": 0.75,
        "empty": [],
        "matrix": [[1, 2], [3, 4], [5, 6]],
        "nothing": null
    })
}

/// A spread of documents, including ones where every path yields null.
fn documents() -> Vec<Value> {
    vec![
        doc(),
        json!({}),
        json!({"people": []}),
        json!({"people": [{"name": null}]}),
        json!({"meta": {"tags": []}}),
        json!({"people": [[1, 2], [3]]}),
        json!([1, 2, 3]),
        json!("a bare string"),
        json!(42),
        json!(null),
        json!(true),
        json!({"items": "not an array"}),
    ]
}

/// Query pairs that must denote the same value.
fn equivalent_pairs() -> Vec<(&'static str, &'static str)> {
    vec![
        // The two pairs named in the specification.
        ("people[0].name", "people | [0] | name"),
        ("length(items)", "items | length(@)"),
        // Accessor chain versus pipe spelling.
        ("people[0].age", "people | [0] | age"),
        ("meta.source", "meta | source"),
        ("meta.nested.deep.value", "meta | nested | deep | value"),
        ("people[2].name", "people[2] | name"),
        // `.*` and `[*]` are the same projection.
        ("people[*].name", "people.*.name"),
        ("meta.*", "meta[*]"),
        ("people[*].tags", "people.*.tags"),
        // Quoting a key does not change meaning.
        ("meta.source", "meta.\"source\""),
        ("people[0].name", "people[0].\"name\""),
        // Multi-select hash key order is irrelevant.
        (
            "{name: people[0].name, batch: meta.batch}",
            "{batch: meta.batch, name: people[0].name}",
        ),
        // Function call versus piped call.
        ("keys(meta)", "meta | keys(@)"),
        ("values(flags)", "flags | values(@)"),
        ("length(people)", "people | length(@)"),
        (
            "contains(meta.tags, `auto`)",
            "meta.tags | contains(@, `auto`)",
        ),
        ("merge(meta.nested)", "meta | nested | merge(@)"),
    ]
}

#[test]
fn the_two_required_pairs_agree_byte_for_byte() {
    let d = doc();
    let a = compile("people[0].name").unwrap().run(&d).to_string();
    let b = compile("people | [0] | name").unwrap().run(&d).to_string();
    assert_eq!(a, b, "`people[0].name` and `people | [0] | name` diverged");

    let a = compile("length(items)").unwrap().run(&d).to_string();
    let b = compile("items | length(@)").unwrap().run(&d).to_string();
    assert_eq!(a, b, "`length(items)` and `items | length(@)` diverged");
}

#[test]
fn the_two_required_pairs_are_recognised_as_equivalent() {
    let a = compile("people[0].name").unwrap();
    let b = compile("people | [0] | name").unwrap();
    assert!(a.is_equivalent(&b));
    assert_eq!(normal_form(a.ast()), normal_form(b.ast()));

    let a = compile("length(items)").unwrap();
    let b = compile("items | length(@)").unwrap();
    assert!(a.is_equivalent(&b));
    assert_eq!(normal_form(a.ast()), normal_form(b.ast()));
}

#[test]
fn every_equivalent_pair_produces_identical_bytes() {
    for (x, y) in equivalent_pairs() {
        let qx = compile(x).unwrap_or_else(|e| panic!("`{x}` failed to compile: {e}"));
        let qy = compile(y).unwrap_or_else(|e| panic!("`{y}` failed to compile: {e}"));
        assert!(
            qx.is_equivalent(&qy),
            "`{x}` and `{y}` were not recognised as equivalent\n  {} vs {}",
            normal_form(qx.ast()),
            normal_form(qy.ast())
        );
        for d in documents() {
            let ax = qx.run(&d).to_string();
            let ay = qy.run(&d).to_string();
            assert_eq!(
                ax, ay,
                "`{x}` and `{y}` diverged on this document:\n  {ax}\n  {ay}"
            );
        }
    }
}

#[test]
fn differently_spelled_queries_that_are_not_equivalent_are_rejected() {
    let pairs = [
        ("people[0].name", "people[1].name"),
        ("people[0].name", "people[0].age"),
        ("length(people)", "length(items)"),
        ("keys(meta)", "values(meta)"),
        ("people[*].name", "people[*].age"),
        // A projection rebinds the node; a plain index does not.
        ("people[*].name", "people[0].name"),
    ];
    for (x, y) in pairs {
        let qx = compile(x).unwrap();
        let qy = compile(y).unwrap();
        assert!(
            !qx.is_equivalent(&qy),
            "`{x}` and `{y}` were wrongly treated as equivalent"
        );
    }
}

#[test]
fn repeated_evaluation_of_one_query_is_byte_identical() {
    let queries = [
        "people[*].name",
        "keys(meta)",
        "values(flags)",
        "meta.*",
        "{name: people[0].name, batch: meta.batch}",
    ];
    let d = doc();
    for q in queries {
        let compiled = compile(q).unwrap();
        let first = compiled.run(&d).to_string();
        for i in 0..200 {
            assert_eq!(
                compiled.run(&d).to_string(),
                first,
                "`{q}` produced different bytes on iteration {i}"
            );
        }
    }
}

#[test]
fn object_traversal_order_is_lexicographic() {
    // `A` sorts before `a` because ordering is by byte value, not by any
    // locale-aware collation, which is what keeps output reproducible across
    // machines.
    let inner = json!({"z": 1, "a": 2, "m": 3, "b": 4, "aa": 5, "A": 6});
    let d = json!({ "d": inner });
    assert_eq!(
        compile("keys(d)").unwrap().run(&d),
        json!(["A", "a", "aa", "b", "m", "z"])
    );
    assert_eq!(
        compile("values(d)").unwrap().run(&d),
        json!([6, 2, 5, 4, 3, 1])
    );
    assert_eq!(compile("d.*").unwrap().run(&d), json!([6, 2, 5, 4, 3, 1]));
    // The same order every single time.
    let q = compile("values(d)").unwrap();
    let first = q.run(&d).to_string();
    for _ in 0..100 {
        assert_eq!(q.run(&d).to_string(), first);
    }
}

#[test]
fn results_never_alias_the_input() {
    // A result is fully owned: mutating it must not affect the source document.
    let d = doc();
    let mut result = compile("meta").unwrap().run(&d);
    result["source"] = json!("mutated");
    assert_eq!(d["meta"]["source"], json!("risk"));
}

#[test]
fn search_finds_equivalent_spellings_for_deduplication() {
    let original = "people[0].name";
    let variants = search(original, 16);
    assert!(
        variants.iter().any(|v| v.query == "people | [0] | name"),
        "expected the pipe spelling among {variants:?}"
    );
    let expected = compile(original).unwrap().run(&doc()).to_string();
    for v in &variants {
        let q = compile(&v.query)
            .unwrap_or_else(|e| panic!("variant `{}` does not compile: {e}", v.query));
        assert!(q.is_equivalent(&compile(original).unwrap()));
        assert_eq!(q.run(&doc()).to_string(), expected);
        assert_ne!(&v.query, original, "a variant must differ from the input");
    }
}

#[test]
fn search_never_invents_a_wrong_equivalent() {
    // Every catalogue entry must only produce variants that really are equal.
    for (original, _) in equivalent_pairs() {
        for v in search(original, 8) {
            let a = compile(original).unwrap();
            let b = compile(&v.query).unwrap();
            assert!(
                a.is_equivalent(&b),
                "search offered `{v:?}` as equivalent to `{original}`, but it is not"
            );
        }
    }
}

#[test]
fn equivalence_is_reflexive_and_symmetric() {
    let q = compile("people[0].name").unwrap();
    assert!(q.is_equivalent(&q), "reflexive");

    // Three genuinely different spellings of the same query.
    let chain = [
        "people[0].name",
        "people | [0] | name",
        "people[0].\"name\"",
    ];
    for a in chain {
        for b in chain {
            let (qa, qb) = (compile(a).unwrap(), compile(b).unwrap());
            assert_eq!(
                qa.is_equivalent(&qb),
                qb.is_equivalent(&qa),
                "symmetry failed for `{a}` and `{b}`"
            );
        }
    }
}

#[test]
fn parsing_the_same_query_twice_yields_the_same_normal_form() {
    for (x, _) in equivalent_pairs() {
        let a = jmespath_guard::parse(x).unwrap();
        let b = jmespath_guard::parse(x).unwrap();
        assert_eq!(normal_form(&a), normal_form(&b));
        assert!(equivalent(&a, &b));
    }
}
