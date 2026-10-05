//! Performance and concurrency guarantees.
//!
//! These are *budget* assertions rather than micro-benchmarks: criterion
//! (`cargo bench`) reports distributions, while these tests fail when a
//! regression pushes a stage past its stated ceiling.  They use p99 over many
//! samples so a single slow run — GC, scheduler noise, a cold page — cannot
//! cause a spurious failure, while a genuine regression still trips them.

use std::sync::Arc;
use std::time::{Duration, Instant};

use jmespath_guard::{compile, compile_with_shape, Type};
use serde_json::{json, Value};

/// Ceilings from the specification.
const EVAL_BUDGET: Duration = Duration::from_millis(1);
const TYPECHECK_BUDGET: Duration = Duration::from_micros(50);
const PARSE_BUDGET: Duration = Duration::from_micros(100);

/// ~10 KB of realistic risk-control event data.
fn big_doc() -> Value {
    let people: Vec<Value> = (0..72)
        .map(|i| {
            json!({
                "id": i,
                "name": format!("user-{}", i),
                "age": 20 + (i % 40),
                "risk": i % 3 == 0,
                "tags": [format!("t{}", i % 7), format!("u{}", i % 5)],
                "score": (i as f64) / 7.0,
                "meta": {"region": "cn", "verified": i % 2 == 0}
            })
        })
        .collect();
    let padding: Vec<Value> = (0..10)
        .map(|i| json!({"k": i, "note": format!("padding-note-{:03}", i)}))
        .collect();
    json!({
        "meta": {"source": "risk", "batch": 20240101, "tags": ["a", "b", "c"]},
        "people": people,
        "flags": {"risk": true, "reviewed": false},
        "score": 0.75,
        "padding": padding
    })
}

/// Duration of the 99th percentile of `n` timed runs of `f`.
fn p99_of<F: FnMut()>(n: usize, mut f: F) -> Duration {
    let mut samples = Vec::with_capacity(n);
    for _ in 0..n {
        let t0 = Instant::now();
        f();
        samples.push(t0.elapsed());
    }
    samples.sort_unstable();
    samples[n * 99 / 100]
}

#[test]
fn fixture_is_about_ten_kilobytes() {
    let size = serde_json::to_string(&big_doc()).unwrap().len();
    assert!(
        (9_000..=13_000).contains(&size),
        "fixture is {size} bytes, expected roughly 10 KB"
    );
}

#[test]
fn single_evaluation_p99_is_under_one_millisecond() {
    let doc = big_doc();
    for q in [
        "people[0].name",
        "people[*].name",
        "people[0:50].name",
        "meta.source",
        "length(people)",
        "keys(meta)",
        "[people[0].name, people[1].name]",
        "{n: people[0].name, b: meta.batch}",
    ] {
        let compiled = compile(q).unwrap();
        // Warm up so first-touch page faults are not counted as steady state.
        for _ in 0..50 {
            std::hint::black_box(compiled.run(std::hint::black_box(&doc)));
        }
        let p99 = p99_of(2_000, || {
            std::hint::black_box(compiled.run(std::hint::black_box(&doc)));
        });
        assert!(
            p99 < EVAL_BUDGET,
            "`{q}` p99 was {p99:?}, budget is {EVAL_BUDGET:?}"
        );
    }
}

#[test]
fn type_check_p99_is_under_fifty_microseconds() {
    let shape = Type::object(vec![
        (
            "people",
            Type::array(Type::object(vec![("name", Type::String)])),
        ),
        (
            "meta",
            Type::object(vec![("source", Type::String), ("batch", Type::Number)]),
        ),
        ("score", Type::Number),
    ]);
    for q in [
        "people[0].name",
        "people[*].name",
        "people[0:50].name",
        "length(people)",
        "keys(meta)",
        "people | [0] | name",
    ] {
        for _ in 0..50 {
            std::hint::black_box(compile_with_shape(std::hint::black_box(q), &shape).unwrap());
        }
        let p99 = p99_of(1_000, || {
            std::hint::black_box(compile_with_shape(std::hint::black_box(q), &shape).unwrap());
        });
        assert!(
            p99 < TYPECHECK_BUDGET,
            "type check of `{q}` p99 was {p99:?}, budget is {TYPECHECK_BUDGET:?}"
        );
    }
}

#[test]
fn parse_p99_is_under_one_hundred_microseconds() {
    for q in [
        "people[0].name",
        "people[*].name",
        "people[0:50].name",
        "people | [0] | name",
        "{n: people[0].name, b: meta.batch}",
    ] {
        for _ in 0..50 {
            std::hint::black_box(jmespath_guard::parse(std::hint::black_box(q)).unwrap());
        }
        let p99 = p99_of(1_000, || {
            std::hint::black_box(jmespath_guard::parse(std::hint::black_box(q)).unwrap());
        });
        assert!(
            p99 < PARSE_BUDGET,
            "parse of `{q}` p99 was {p99:?}, budget is {PARSE_BUDGET:?}"
        );
    }
}

#[test]
fn one_engine_instance_is_safe_to_share_across_threads() {
    let query = Arc::new(compile("people[*].name").unwrap());
    let doc = Arc::new(big_doc());
    let expected = query.run(&doc).to_string();

    let handles: Vec<_> = (0..8)
        .map(|_| {
            let q = Arc::clone(&query);
            let d = Arc::clone(&doc);
            std::thread::spawn(move || {
                let mut last = String::new();
                for _ in 0..300 {
                    last = q.run(&d).to_string();
                }
                last
            })
        })
        .collect();

    for h in handles {
        assert_eq!(
            h.join().unwrap(),
            expected,
            "a concurrent evaluation disagreed with the single-threaded result"
        );
    }
}

#[test]
fn concurrent_compilation_is_also_deterministic() {
    let queries = [
        "people[0].name",
        "people | [0] | name",
        "length(people)",
        "keys(meta)",
    ];
    let doc = Arc::new(big_doc());
    let expected: Vec<String> = queries
        .iter()
        .map(|q| compile(q).unwrap().run(&doc).to_string())
        .collect();

    let handles: Vec<_> = (0..8)
        .map(|_| {
            let doc = Arc::clone(&doc);
            std::thread::spawn(move || {
                queries
                    .iter()
                    .map(|q| compile(q).unwrap().run(&doc).to_string())
                    .collect::<Vec<_>>()
            })
        })
        .collect();

    for h in handles {
        assert_eq!(h.join().unwrap(), expected);
    }
}
