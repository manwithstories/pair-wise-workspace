use criterion::{black_box, criterion_group, criterion_main, Criterion};
use jmespath_guard::{compile, compile_with_shape, Type};
use serde_json::{json, Value};

/// ~10 KB of realistic risk-control event data.
fn big_doc() -> Value {
    let mut people = Vec::new();
    for i in 0..72 {
        people.push(json!({
            "id": i,
            "name": format!("user-{}", i),
            "age": 20 + (i % 40),
            "risk": i % 3 == 0,
            "tags": [format!("t{}", i % 7), format!("u{}", i % 5)],
            "score": (i as f64) / 7.0,
            "meta": {"region": "cn", "verified": i % 2 == 0}
        }));
    }
    let mut d = json!({
        "meta": {"source": "risk", "batch": 20240101, "tags": ["a","b","c"]},
        "people": people,
        "flags": {"risk": true, "reviewed": false},
        "score": 0.75
    });
    // Pad to roughly 10 KB.
    let mut pad = Vec::new();
    for i in 0..10 {
        pad.push(json!({"k": i, "note": format!("padding-note-{:03}", i)}));
    }
    d["padding"] = json!(pad);
    d
}

fn bench_compile(c: &mut Criterion) {
    let q = "people[*].tags | [0]";
    c.bench_function("compile/typical", |b| {
        b.iter(|| compile(black_box(q)).unwrap())
    });

    let shape = Type::object(vec![
        ("people", Type::array(Type::Any)),
        ("meta", Type::object(vec![("source", Type::String)])),
    ]);
    c.bench_function("compile_with_shape", |b| {
        b.iter(|| compile_with_shape(black_box(q), &shape).unwrap())
    });
}

fn bench_eval(c: &mut Criterion) {
    let d = big_doc();
    let bytes = serde_json::to_string(&d).unwrap().len();
    assert!(
        (9_000..=13_000).contains(&bytes),
        "fixture is {bytes} bytes"
    );
    println!("benchmark fixture: {bytes} bytes");
    let queries = [
        "people[0].name",
        "people[*].name",
        "people[0:50].name",
        "meta.source",
        "length(people)",
        "keys(meta)",
    ];
    for q in queries {
        let compiled = compile(q).unwrap();
        c.bench_function(&format!("eval/{q}"), |b| {
            b.iter(|| compiled.run(black_box(&d)))
        });
    }
}

fn bench_end_to_end(c: &mut Criterion) {
    let d = big_doc();
    let q = "people[*].tags | [0]";
    c.bench_function("compile+eval", |b| {
        b.iter(|| {
            let compiled = compile(black_box(q)).unwrap();
            compiled.run(black_box(&d))
        })
    });
}

criterion_group!(benches, bench_compile, bench_eval, bench_end_to_end);
criterion_main!(benches);
