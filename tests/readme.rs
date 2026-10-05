//! Verification that every code block and table row in README.md holds.
//!
//! Documentation that is not executable drifts.  This test is the executable
//! spec: if the engine changes behaviour, the README claims fail here rather
//! than misleading a reader.

use jmespath_guard::{compile, compile_with_shape, Type};
use serde_json::{json, Value};

#[test]
fn readme_claims_hold() {
    let doc = json!({"people": [{"name": "ada", "age": 36}]});
    let q = compile("people[0].name").unwrap();
    assert_eq!(q.run(&doc), json!("ada"));

    let a = compile("people[0].name").unwrap();
    let b = compile("people | [0] | name").unwrap();
    assert_eq!(a.run(&doc), b.run(&doc));
    assert!(a.is_equivalent(&b));

    let shape = Type::object(vec![("people", Type::array(Type::Any))]);
    assert!(compile_with_shape("people | [0] | name", &shape).is_ok());
    let err =
        compile_with_shape("score[0]", &Type::object(vec![("score", Type::Number)])).unwrap_err();
    assert!(err
        .to_string()
        .contains("an index or slice requires an array"));

    // Documented error table.
    for (query, needle) in [
        ("sort_by(people, &age)", "unknown function `sort_by`"),
        ("contains(meta)", "takes 2 arguments"),
        ("people[?age > `30`]", "filter expressions"),
        ("people[0", "syntax error at line 1, column 9"),
    ] {
        let e = compile(query).unwrap_err().to_string();
        assert!(e.contains(needle), "{query}: {e}");
    }
    for (query, needle) in [
        ("score[0]", "an index or slice requires an array"),
        ("score.name", "requires an object"),
        ("keys(score)", "cannot call `keys`"),
        ("score[]", "flattening requires an array"),
    ] {
        let e = compile_with_shape(query, &Type::object(vec![("score", Type::Number)]))
            .unwrap_err()
            .to_string();
        assert!(e.contains(needle), "{query}: {e}");
    }

    // Documented projection semantics.
    let d = json!({"people": [
        {"address": {"city": "london"}, "tags": ["x","y"]},
        {"address": {"city": "helsinki"}, "tags": ["z"]}],
        "matrix": [[1,2],[3,4],[5,6]]});
    assert_eq!(
        compile("people[*].address.city").unwrap().run(&d),
        json!(["london", "helsinki"])
    );
    assert_eq!(
        compile("people[*].tags").unwrap().run(&d),
        json!([["x", "y"], ["z"]])
    );
    assert_eq!(
        compile("people[*].tags[]").unwrap().run(&d),
        json!(["x", "y", "z"])
    );
    assert_eq!(compile("matrix[*] | [1]").unwrap().run(&d), json!([3, 4]));

    // Documented normalisation table.
    let n = |q: &str| jmespath_guard::normal_form(&jmespath_guard::parse(q).unwrap());
    assert_eq!(n("people[0].name"), "people[0].name");
    assert_eq!(n("people | [0] | name"), "people[0].name");
    assert_eq!(n("people.*.name"), "people[*].name");
    assert_eq!(n("a.b"), "a.b");
    assert_eq!(n("a.\"b\""), "a.b");
    assert_eq!(n("{a: x, b: y}"), n("{b: y, a: x}"));

    // Documented literal-vs-field rule.
    let tags = json!({"meta": {"tags": ["auto", "manual"]}});
    assert_eq!(
        compile("contains(meta.tags, `auto`)").unwrap().run(&tags),
        json!(true)
    );
    assert_eq!(compile("keys(meta)").unwrap().run(&tags), json!(["tags"]));
    // `keys(tags)` is null because tags is an array, not an object.
    assert_eq!(compile("keys(tags)").unwrap().run(&tags), Value::Null);

    // Unicode keys, quoted.
    let u = json!({"用户": {"姓名": "张三"}});
    assert_eq!(compile("\"用户\".\"姓名\"").unwrap().run(&u), json!("张三"));

    // Minimal core example from the README.
    let err = compile_with_shape(
        "[score[0], score[*], length(score)]",
        &Type::object(vec![("score", Type::Number)]),
    )
    .unwrap_err()
    .to_string();
    assert!(
        err.contains("minimal unsatisfiable subset (3 requirements)"),
        "{err}"
    );
}
