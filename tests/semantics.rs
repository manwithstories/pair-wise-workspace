//! Parsing and evaluation of the supported JMESPath subset.
//!
//! Evaluation is checked against hand-computed JMESPath semantics, including the
//! corner cases that a naive implementation gets wrong: negative indices,
//! Python-style slice bounds, flatten versus projection, and the difference
//! between a projection (which rebinds the node) and a plain accessor.

use jmespath_guard::{compile, parse};
use serde_json::{json, Value};

fn doc() -> Value {
    json!({
        "meta": {"source": "risk", "batch": 7, "tags": ["a", "b", "c", "d"]},
        "people": [
            {"name": "ada",   "age": 36, "tags": ["x", "y"], "address": {"city": "london"}},
            {"name": "linus", "age": 54, "tags": [],          "address": {"city": "helsinki"}},
            {"name": "grace", "age": 45, "tags": ["z"],       "address": {"city": "new york"}}
        ],
        "items": [10, 20, 30, 40, 50],
        "matrix": [[1, 2], [3, 4], [5, 6]],
        "name": "root-name",
        "empty_obj": {},
        "empty_arr": [],
        "nulls": [null, null],
        "n": 5
    })
}

fn run(q: &str) -> Value {
    compile(q)
        .unwrap_or_else(|e| panic!("`{q}` failed to compile:\n{e}"))
        .run(&doc())
}

// ---------------------------------------------------------------------------
// Fields, indices, slices
// ---------------------------------------------------------------------------

#[test]
fn field_access() {
    assert_eq!(run("meta.source"), json!("risk"));
    assert_eq!(run("people[0].address.city"), json!("london"));
    assert_eq!(run("name"), json!("root-name"));
    assert_eq!(run("@.meta.source"), json!("risk"));
    assert_eq!(run("meta.\"source\""), json!("risk"));
}

#[test]
fn indices_including_negative_and_out_of_range() {
    assert_eq!(run("people[0].name"), json!("ada"));
    assert_eq!(run("people[1].name"), json!("linus"));
    assert_eq!(run("people[-1].name"), json!("grace"));
    assert_eq!(run("people[-3].name"), json!("ada"));
    assert_eq!(run("people[3].name"), Value::Null);
    assert_eq!(run("people[-4].name"), Value::Null);
}

#[test]
fn slices_follow_python_semantics() {
    let i = "items";
    assert_eq!(run(&format!("{i}[0:2]")), json!([10, 20]));
    assert_eq!(run(&format!("{i}[:2]")), json!([10, 20]));
    assert_eq!(run(&format!("{i}[3:]")), json!([40, 50]));
    assert_eq!(run(&format!("{i}[::2]")), json!([10, 30, 50]));
    assert_eq!(run(&format!("{i}[1:4:2]")), json!([20, 40]));
    assert_eq!(run(&format!("{i}[-2:]")), json!([40, 50]));
    assert_eq!(run(&format!("{i}[:-3]")), json!([10, 20]));
    // A negative step counts down from `start` and stops *before* `stop`, which
    // is Python's behaviour and the reference JMESPath implements.
    assert_eq!(run(&format!("{i}[4:1:-1]")), json!([50, 40, 30]));
    assert_eq!(run(&format!("{i}[10:20]")), json!([]));
    assert_eq!(run(&format!("{i}[-100:100]")), json!([10, 20, 30, 40, 50]));
    // A step of zero yields an empty projection rather than looping forever.
    assert_eq!(run(&format!("{i}[::0]")), json!([]));
}

#[test]
fn indexing_a_non_array_yields_null() {
    assert_eq!(run("n[0]"), Value::Null);
    assert_eq!(run("meta[0]"), Value::Null);
    assert_eq!(run("meta.source[0]"), Value::Null);
}

// ---------------------------------------------------------------------------
// Projections
// ---------------------------------------------------------------------------

#[test]
fn wildcard_projects_over_arrays_and_objects() {
    assert_eq!(run("people[*].name"), json!(["ada", "linus", "grace"]));
    assert_eq!(run("people.*.name"), json!(["ada", "linus", "grace"]));
    // Object values come out in lexicographic key order: batch, source, tags.
    assert_eq!(run("meta[*]"), json!([7, "risk", ["a", "b", "c", "d"]]));
    assert_eq!(run("meta.*"), json!([7, "risk", ["a", "b", "c", "d"]]));
    assert_eq!(run("empty_arr[*]"), json!([]));
    assert_eq!(run("empty_obj[*]"), json!([]));
    assert_eq!(run("n[*]"), json!([]));
}

#[test]
fn a_projection_rebinds_the_node() {
    // This is the property that distinguishes a projection from an accessor:
    // `.name` applies to every element, not to the projected array.
    assert_eq!(run("people[*].name"), json!(["ada", "linus", "grace"]));
    assert_eq!(
        run("people[*].address.city"),
        json!(["london", "helsinki", "new york"])
    );
    assert_eq!(run("people[*].tags"), json!([["x", "y"], [], ["z"]]));
    assert_eq!(run("meta.tags[*]"), json!(["a", "b", "c", "d"]));
}

#[test]
fn slice_is_a_projection_too() {
    assert_eq!(run("people[0:2].name"), json!(["ada", "linus"]));
    assert_eq!(run("people[::2].name"), json!(["ada", "grace"]));
    assert_eq!(run("items[1:3]"), json!([20, 30]));
}

#[test]
fn flatten_splices_one_level() {
    assert_eq!(run("matrix[]"), json!([1, 2, 3, 4, 5, 6]));
    assert_eq!(run("matrix[*]"), json!([[1, 2], [3, 4], [5, 6]]));
    // Scalars in the array are kept as-is by a flatten.
    assert_eq!(run("items[]"), json!([10, 20, 30, 40, 50]));
    assert_eq!(run("n[]"), json!([]));
}

#[test]
fn flatten_projection_combines_with_accessors() {
    // `people[*].tags[]` splices every tag of every person into one list.  The
    // person with an empty tag list contributes nothing, so there are exactly
    // three tags in the fixture.
    assert_eq!(run("people[*].tags[]"), json!(["x", "y", "z"]));
    assert_eq!(run("matrix[]"), json!([1, 2, 3, 4, 5, 6]));
    assert_eq!(run("items[]"), json!([10, 20, 30, 40, 50]));
    // `matrix[*][1]` indexes into each projected element and so rebinds again.
    assert_eq!(run("matrix[*][1]"), json!([2, 4, 6]));
    // `matrix[*] | [1]` pipes the whole array into `[1]`, selecting the second
    // sub-array — a different query with a different answer.
    assert_eq!(run("matrix[*] | [1]"), json!([3, 4]));
    // A slice rebinds too, so a further accessor applies per element.
    assert_eq!(run("items[1:3]"), json!([20, 30]));
    // Indexing per element: ada's first tag is "x", linus has none (null), and
    // grace's is "z".
    assert_eq!(run("people[*].tags[0]"), json!(["x", null, "z"]));
}

// ---------------------------------------------------------------------------
// Multi-select
// ---------------------------------------------------------------------------

#[test]
fn multi_select_list() {
    assert_eq!(run("[meta.source, meta.batch]"), json!(["risk", 7]));
    assert_eq!(
        run("[people[0].name, people[1].name]"),
        json!(["ada", "linus"])
    );
    // `[]` is a syntax error (there is nothing to select), not a query that
    // evaluates to null.
    assert!(compile("[]").is_err());
}

#[test]
fn multi_select_hash() {
    assert_eq!(
        run("{name: people[0].name, batch: meta.batch}"),
        json!({"name": "ada", "batch": 7})
    );
    // The key order in the query does not change the result object.
    let a = run("{name: people[0].name, batch: meta.batch}").to_string();
    let b = run("{batch: meta.batch, name: people[0].name}").to_string();
    assert_eq!(a, b);
}

// ---------------------------------------------------------------------------
// Built-ins
// ---------------------------------------------------------------------------

#[test]
fn length_of_each_shape() {
    assert_eq!(run("length(meta)"), json!(3));
    assert_eq!(run("length(people)"), json!(3));
    assert_eq!(run("length(meta.tags)"), json!(4));
    assert_eq!(run("length(meta.source)"), json!(4));
    // Numbers have no length.
    assert_eq!(run("length(n)"), Value::Null);
}

#[test]
fn keys_and_values_are_sorted() {
    assert_eq!(run("keys(meta)"), json!(["batch", "source", "tags"]));
    assert_eq!(
        run("values(meta)"),
        json!([7, "risk", ["a", "b", "c", "d"]])
    );
    assert_eq!(run("keys(empty_obj)"), json!([]));
    assert_eq!(run("keys(meta) | [0]"), json!("batch"));
}

#[test]
fn contains() {
    assert_eq!(run("contains(meta.source, `sk`)"), json!(true));
    assert_eq!(run("contains(meta.source, `zz`)"), json!(false));
    assert_eq!(run("contains(meta.tags, `c`)"), json!(true));
    assert_eq!(run("contains(meta.tags, `z`)"), json!(false));
    assert_eq!(run("contains(people, `ada`)"), json!(false));
    // Membership is by value, not by substring.
    assert_eq!(run("contains(items, 30)"), json!(true));
    assert_eq!(run("contains(items, 31)"), json!(false));
}

#[test]
fn merge_rebuilds_an_object() {
    assert_eq!(
        run("merge(meta)"),
        json!({
            "source": "risk", "batch": 7, "tags": ["a", "b", "c", "d"]
        })
    );
    assert_eq!(run("merge(empty_obj)"), json!({}));
    // Merging a nested object yields the same object, fully owned.
    let merged = run("merge(meta)");
    assert_eq!(merged, doc()["meta"]);
}

// ---------------------------------------------------------------------------
// Pipes
// ---------------------------------------------------------------------------

#[test]
fn pipes_compose() {
    assert_eq!(run("people | [0] | name"), json!("ada"));
    assert_eq!(run("people | [0] | address | city"), json!("london"));
    assert_eq!(run("meta | tags | [1]"), json!("b"));
    assert_eq!(run("items | length(@)"), json!(5));
    assert_eq!(run("meta | keys(@)"), json!(["batch", "source", "tags"]));
}

#[test]
fn reference_expressions_address_the_current_node() {
    assert_eq!(run("&meta.source"), json!("risk"));
    assert_eq!(run("&name"), json!("root-name"));
}

// ---------------------------------------------------------------------------
// Missing data
// ---------------------------------------------------------------------------

#[test]
fn absent_paths_yield_null() {
    assert_eq!(run("meta.missing"), Value::Null);
    assert_eq!(run("people[0].nickname"), Value::Null);
    assert_eq!(run("people[0].address.zip"), Value::Null);
    assert_eq!(run("missing.deeper.deepest"), Value::Null);
    assert_eq!(run("meta.tags[99]"), Value::Null);
}

#[test]
fn nulls_in_the_data_are_preserved() {
    assert_eq!(run("nulls"), json!([null, null]));
    assert_eq!(run("nulls[0]"), Value::Null);
}

// ---------------------------------------------------------------------------
// Parser surface
// ---------------------------------------------------------------------------

#[test]
fn every_supported_construct_parses() {
    for q in [
        "a",
        "a.b",
        "a.b.c",
        "\"quoted\".field",
        "a[0]",
        "a[-1]",
        "a[0:1]",
        "a[:]",
        "a[::2]",
        "a[*]",
        "a.*",
        "a[]",
        "a || b",
        "a | b",
        "@",
        "@.a",
        "&a",
        "[a, b]",
        "{k: a}",
        "length(a)",
        "contains(a, b)",
        "a[0].b[*].c | [0] | d",
    ] {
        parse(q).unwrap_or_else(|e| panic!("`{q}` should parse, got:\n{e}"));
    }
}

#[test]
fn malformed_queries_are_rejected_with_a_location() {
    for (q, needle) in [
        ("a[0", "`]`"),
        ("a.", "field name"),
        ("[", "index"),
        ("{a: }", "expression"),
        ("{a: b, a: c}", "duplicate key"),
        ("[]", "empty index"),
        ("f(a,)", "trailing comma"),
        ("\"unterminated", "unterminated"),
        ("a @ b", "end of query"),
        ("a..b", "field name"),
        ("a[::", "index"),
    ] {
        let e = parse(q).unwrap_err();
        assert!(
            e.msg.contains(needle),
            "`{q}` reported `{}`, expected it to mention `{needle}`",
            e.msg
        );
        assert!(e.span.start.line >= 1 && e.span.start.col >= 1);
    }
}

#[test]
fn deeply_nested_queries_are_bounded_not_crashed() {
    assert!(parse("a").is_ok());
    // A pathological nest must be refused with a diagnostic, not overflow the
    // stack.  The parser has its own depth budget precisely so that a hostile
    // or generated query cannot take the process down.
    for pathological in ["[".repeat(5_000), format!("{}a", "(".repeat(5_000))] {
        let err = parse(&pathological).expect_err("expected a depth-limit error");
        assert!(
            err.msg.contains("deeply") || err.msg.contains("depth"),
            "unexpected message: {}",
            err.msg
        );
    }
}

#[test]
fn unicode_keys_are_supported() {
    // JMESPath bare identifiers are ASCII; other scripts must be quoted, and
    // the lexer says so explicitly rather than failing obscurely.
    let d = json!({"用户": {"姓名": "张三"}});
    let q = compile("\"用户\".\"姓名\"").unwrap();
    assert_eq!(q.run(&d), json!("张三"));
    assert_eq!(compile("keys(\"用户\")").unwrap().run(&d), json!(["姓名"]));

    // Sorting is by UTF-8 byte order, so it is locale-independent.
    let u = json!({"用户": {"b": 1, "a": 2, "张": 3}});
    assert_eq!(
        compile("keys(\"用户\")").unwrap().run(&u),
        json!(["a", "b", "张"])
    );

    let err = compile("用户").unwrap_err();
    assert!(
        err.to_string().contains("quote it"),
        "expected a helpful message, got: {err}"
    );
}
