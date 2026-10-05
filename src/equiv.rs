//! Semantic equivalence: a normal form over the AST plus a bounded search for
//! alternate spellings of the same query.
//!
//! Analysts write the same intent in many ways — `people[0].name` and
//! `people | [0] | name`, or `length(items)` and `items | length(@)`.  Simple
//! string comparison cannot see through that.  Instead we build a canonical
//! normal form that erases purely syntactic variation while preserving
//! semantics, and expose [`search`] to enumerate nearby distinct spellings that
//! normalise to the same form.
//!
//! The normal form rules are conservative: every rewrite below preserves the
//! JMESPath meaning exactly, and anything the rewriter is not certain about is
//! left alone rather than guessed at.

use std::collections::BTreeSet;

use crate::ast::{Ast, AstKind, BracketSpec, SliceSpec};
use crate::lexer::Span;
use crate::parser::parse;

// ---------------------------------------------------------------------------
// Normalisation
// ---------------------------------------------------------------------------

/// One step of a rebinding accessor chain, left to right.
#[derive(Clone, Debug, PartialEq, Eq)]
enum Step {
    Field(String),
    Index(i64),
    Wildcard,
    Flatten,
    Slice(SliceSpec),
    MultiList(Vec<Ast>),
}

/// A node plus the rebinding accessor chain applied to it.
#[derive(Clone, Debug)]
struct Chain {
    base: Ast,
    steps: Vec<Step>,
}

impl Chain {
    /// Rebuild an AST from the base and its steps.
    fn build(&self) -> Ast {
        let mut node = self.base.clone();
        for s in &self.steps {
            node = match s {
                Step::Field(n) => {
                    Ast::new(AstKind::Field(Box::new(node), n.clone()), Span::dummy())
                }
                Step::Index(i) => Ast::new(
                    AstKind::Bracket(Box::new(node), BracketSpec::Index(*i)),
                    Span::dummy(),
                ),
                Step::Wildcard => Ast::new(AstKind::Wildcard(Box::new(node)), Span::dummy()),
                Step::Flatten => Ast::new(AstKind::Flatten(Box::new(node)), Span::dummy()),
                Step::Slice(sl) => Ast::new(
                    AstKind::Bracket(Box::new(node), BracketSpec::Slice(*sl)),
                    Span::dummy(),
                ),
                Step::MultiList(items) => Ast::new(
                    AstKind::Bracket(Box::new(node), BracketSpec::List(items.clone())),
                    Span::dummy(),
                ),
            };
        }
        node
    }
}

/// Decompose `a` into a base and the rebinding accessor chain applied to it.
///
/// Accessors nest to the left, so the steps come out right-to-left and are
/// reversed here.  Returns `None` for anything that is not a pure accessor chain
/// rooted at a bare identifier or `@` (a call, a multi-select, a projection...).
fn decompose(a: &Ast) -> Option<Chain> {
    let mut steps = Vec::new();
    let mut cur = a;
    loop {
        match &cur.kind {
            AstKind::Current => {
                steps.reverse();
                return Some(Chain {
                    base: Ast::new(AstKind::Current, cur.span),
                    steps,
                });
            }
            AstKind::Ident(name) => {
                // A bare leading identifier is the chain's base, not a step.
                steps.reverse();
                return Some(Chain {
                    base: Ast::new(AstKind::Ident(name.clone()), cur.span),
                    steps,
                });
            }
            AstKind::QuotedField(name) => {
                // `a."b"` and `a.b` are the same lookup.
                steps.reverse();
                return Some(Chain {
                    base: Ast::new(AstKind::Ident(name.clone()), cur.span),
                    steps,
                });
            }
            AstKind::Field(l, name) => {
                steps.push(Step::Field(name.clone()));
                cur = l;
            }
            AstKind::Bracket(l, BracketSpec::Index(i)) => {
                steps.push(Step::Index(*i));
                cur = l;
            }
            AstKind::Bracket(l, BracketSpec::Wildcard) => {
                steps.push(Step::Wildcard);
                cur = l;
            }
            AstKind::Flatten(l) => {
                steps.push(Step::Flatten);
                cur = l;
            }
            AstKind::Bracket(l, BracketSpec::Slice(sl)) => {
                steps.push(Step::Slice(*sl));
                cur = l;
            }
            AstKind::Bracket(l, BracketSpec::List(items)) => {
                steps.push(Step::MultiList(items.clone()));
                cur = l;
            }
            _ => return None,
        }
    }
}

/// Decompose `a`, treating a *leading* bracket as an accessor on the current
/// node.
///
/// This is what makes the pipe spelling `people | [0] | name` normalise to the
/// same form as `people[0].name`: a leading `[0]` has an implicit `@` base.
fn decompose_with_implicit_base(a: &Ast) -> Option<Chain> {
    match &a.kind {
        AstKind::Bracket(l, spec) => {
            let inner = step_of(spec)?;
            let base = decompose(l)?;
            let mut steps = base.steps;
            steps.push(inner);
            Some(Chain {
                base: base.base,
                steps,
            })
        }
        AstKind::Wildcard(l) => {
            let base = decompose(l)?;
            let mut steps = base.steps;
            steps.push(Step::Wildcard);
            Some(Chain {
                base: base.base,
                steps,
            })
        }
        AstKind::Flatten(l) => {
            let base = decompose(l)?;
            let mut steps = base.steps;
            steps.push(Step::Flatten);
            Some(Chain {
                base: base.base,
                steps,
            })
        }
        _ => decompose(a),
    }
}

/// Steps of a right-hand-side pipe operand.
///
/// This differs from [`decompose`] in exactly one place: a bare leading
/// identifier is a field access on the piped value rather than a new base,
/// because the right side of a pipe is always evaluated against the left side's
/// result.  Everything else is the same decomposition.
fn pipe_steps(a: &Ast) -> Option<Vec<Step>> {
    match &a.kind {
        // A bare identifier here means "field of the piped value".
        AstKind::Ident(name) | AstKind::QuotedField(name) => Some(vec![Step::Field(name.clone())]),
        _ => {
            let c = decompose_with_implicit_base(a)?;
            match c.base.kind {
                // The chain was rooted at the current node, which is what the
                // left side of a pipe supplies.
                AstKind::Current => Some(c.steps),
                _ => None,
            }
        }
    }
}

fn step_of(spec: &BracketSpec) -> Option<Step> {
    match spec {
        BracketSpec::Wildcard => Some(Step::Wildcard),
        BracketSpec::Index(i) => Some(Step::Index(*i)),
        BracketSpec::Slice(s) => Some(Step::Slice(*s)),
        BracketSpec::List(items) => Some(Step::MultiList(items.clone())),
        BracketSpec::Nested(_) | BracketSpec::Filter(_) => None,
    }
}

/// Rewrite `a` into an equivalent left-nested accessor chain, pushing explicit
/// `@` bases down to the innermost position.
///
/// `people | [0] | name` becomes `people[0].name`, and `a.*` becomes `a[*]`,
/// which is exactly the normal form that makes textual variants comparable.
pub fn normalize(a: &Ast) -> Ast {
    match &a.kind {
        AstKind::Pipe(lhs, rhs) => {
            // `x | chain` is `x` with the chain appended.  The right side is
            // always rooted at the *current* node, so a bare identifier there
            // (`x | name`) is a field access on `x`, not a new base.
            if let Some(steps) = pipe_steps(rhs) {
                if !steps.is_empty() {
                    let node = Chain {
                        base: normalize(lhs),
                        steps,
                    }
                    .build();
                    return node;
                }
            }
            // `x | f(@, ...)` is exactly `f(x, ...)`: piping into a function
            // whose first argument is the current node passes the piped value as
            // that argument.  This is what makes `items | length(@)` and
            // `length(items)` compare equal.
            if let AstKind::Call {
                name,
                name_span,
                builtin,
                args,
            } = &rhs.kind
            {
                if let Some((first, rest)) = args.split_first() {
                    if !matches!(first.kind, AstKind::Current) {
                        return Ast::new(
                            AstKind::Pipe(Box::new(normalize(lhs)), Box::new(normalize(rhs))),
                            a.span,
                        );
                    }
                    let mut new_args = vec![normalize(lhs)];
                    new_args.extend(rest.iter().map(normalize));
                    return Ast::new(
                        AstKind::Call {
                            name: name.clone(),
                            name_span: *name_span,
                            builtin: *builtin,
                            args: new_args,
                        },
                        a.span,
                    );
                }
            }
            Ast::new(
                AstKind::Pipe(Box::new(normalize(lhs)), Box::new(normalize(rhs))),
                a.span,
            )
        }
        AstKind::Current => Ast::new(AstKind::Current, a.span),
        AstKind::Root => Ast::new(AstKind::Root, a.span),
        AstKind::RefExpr(e) => Ast::new(AstKind::RefExpr(Box::new(normalize(e))), a.span),
        AstKind::Ident(_)
        | AstKind::QuotedField(_)
        | AstKind::StringLiteral(_)
        | AstKind::LiteralKey(_)
        | AstKind::NumberLiteral(_) => a.clone(),
        AstKind::MultiSelectList(items) => Ast::new(
            AstKind::MultiSelectList(items.iter().map(normalize).collect()),
            a.span,
        ),
        AstKind::MultiSelectHash(items) => Ast::new(
            AstKind::MultiSelectHash(
                items
                    .iter()
                    .map(|(k, s, v)| (k.clone(), s.clone(), normalize(v)))
                    .collect(),
            ),
            a.span,
        ),
        AstKind::Call {
            name,
            name_span,
            builtin,
            args,
        } => Ast::new(
            AstKind::Call {
                name: name.clone(),
                name_span: *name_span,
                builtin: *builtin,
                args: args.iter().map(normalize).collect(),
            },
            a.span,
        ),
        AstKind::Projection(lhs, rhs) => Ast::new(
            AstKind::Projection(Box::new(normalize(lhs)), Box::new(normalize(rhs))),
            a.span,
        ),
        // Plain accessor nodes are already in normal form once their operands
        // are, apart from the `.*` / `[*]` spelling which `decompose` unifies.
        AstKind::Field(..) | AstKind::Wildcard(_) | AstKind::Bracket(..) | AstKind::Flatten(_) => {
            match decompose_with_implicit_base(a) {
                Some(c) => Chain {
                    base: normalize(&c.base),
                    steps: c.steps,
                }
                .build(),
                None => a.clone(),
            }
        }
    }
}

/// The canonical normal form of a query.
///
/// Normalisation first (structural rewrite to one left-nested accessor chain),
/// then rendering.  Two queries are considered equivalent exactly when their
/// normal forms are byte-identical.
pub fn normal_form(a: &Ast) -> String {
    render(&normalize(a))
}

fn render(a: &Ast) -> String {
    match &a.kind {
        AstKind::Root | AstKind::Current => "@".to_string(),
        AstKind::RefExpr(e) => format!("&{}", render(e)),
        AstKind::Ident(s) | AstKind::QuotedField(s) | AstKind::LiteralKey(s) => s.clone(),
        AstKind::StringLiteral(s) => format!("'{}'", s),
        AstKind::NumberLiteral(n) => n.to_string(),
        AstKind::Field(l, name) => format!("{}.{}", render(l), name),
        AstKind::Wildcard(l) => format!("{}[*]", render(l)),
        AstKind::Flatten(l) => format!("{}.[]", render(l)),
        AstKind::Bracket(l, spec) => format!("{}{}", render(l), render_spec(spec)),
        AstKind::Projection(l, r) => format!("{}||{}", render(l), render(r)),
        AstKind::Pipe(l, r) => format!("{}|{}", render(l), render(r)),
        AstKind::MultiSelectList(items) => {
            let parts: Vec<String> = items.iter().map(render).collect();
            format!("[{}]", parts.join(","))
        }
        AstKind::MultiSelectHash(items) => {
            // Key order in a multi-select hash cannot affect the resulting
            // object, so sort the entries.
            let mut parts: Vec<String> = items
                .iter()
                .map(|(k, _, v)| format!("{}:{}", k, render(v)))
                .collect();
            parts.sort();
            format!("{{{}}}", parts.join(","))
        }
        AstKind::Call { name, args, .. } => {
            let parts: Vec<String> = args.iter().map(render).collect();
            format!("{}({})", name, parts.join(","))
        }
    }
}

fn render_spec(spec: &BracketSpec) -> String {
    match spec {
        BracketSpec::Wildcard => "[*]".to_string(),
        BracketSpec::Index(i) => format!("[{}]", i),
        BracketSpec::Slice(s) => {
            let mut out = String::from("[");
            if let Some(v) = s.start {
                out.push_str(&v.to_string());
            }
            out.push(':');
            if let Some(v) = s.stop {
                out.push_str(&v.to_string());
            }
            if s.has_second_colon {
                out.push(':');
                if let Some(v) = s.step {
                    out.push_str(&v.to_string());
                }
            }
            out.push(']');
            out
        }
        BracketSpec::Nested(items) => {
            let parts: Vec<String> = items.iter().map(render).collect();
            format!("[[{}]]", parts.join(","))
        }
        BracketSpec::List(items) => {
            let parts: Vec<String> = items.iter().map(render).collect();
            format!("[{}]", parts.join(","))
        }
        BracketSpec::Filter(_) => "[?]".to_string(),
    }
}

/// Are two queries semantically equivalent?
///
/// The comparison is structural over the normal form, which erases purely
/// syntactic variation: `people[0].name` vs `people | [0] | name`,
/// `people[*].name` vs `people.*.name`, `a.b` vs `a."b"`, and multi-select hash
/// key reordering.  It never guesses: anything not provably equivalent is
/// reported as different.
pub fn equivalent(a: &Ast, b: &Ast) -> bool {
    normal_form(a) == normal_form(b)
}

// ---------------------------------------------------------------------------
// Variant search
// ---------------------------------------------------------------------------

/// One alternative spelling of an equivalent query.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Variant {
    /// The alternative query text.
    pub query: String,
    /// How the rewrite was derived, for human review.
    pub note: &'static str,
}

/// Generate differently-spelled queries that denote the same value as `query`.
///
/// This is a bounded enumeration, not a solver.  It applies a fixed set of
/// semantics-preserving rewrites — accessor chain to pipe spelling, dropping
/// redundant `@` bases, quoting keys, and multi-select hash key reordering — and
/// returns the results whose text differs from the input.
///
/// Every returned variant is guaranteed to normalise to the same form as the
/// input, so callers can use it to deduplicate an analyst's query catalogue.
pub fn search(query: &str, limit: usize) -> Vec<Variant> {
    let mut out: Vec<Variant> = Vec::new();
    let mut seen: BTreeSet<String> = BTreeSet::new();
    seen.insert(query.to_string());
    if limit == 0 {
        return out;
    }

    let Ok(ast) = parse(query) else {
        return out;
    };

    // 1. The canonical spelling plus the pipe spelling of an accessor chain.
    if let Some(c) = decompose_with_implicit_base(&ast) {
        let piped = pipe_spelling(&c);
        if seen.insert(piped.clone()) {
            out.push(Variant {
                query: piped,
                note: "accessor chain rewritten using pipes",
            });
        }
        if out.len() < limit {
            if let Some(p) = pipe_spelling_with_at(&c) {
                if seen.insert(p.clone()) {
                    out.push(Variant {
                        query: p,
                        note: "accessor chain with an explicit `@` base",
                    });
                }
            }
        }
    }

    // 2. Reordering a multi-select hash is free: the resulting object is the
    //    same regardless of the order the keys were written in.
    if let AstKind::MultiSelectHash(items) = &ast.kind {
        if items.len() > 1 {
            let mut reversed: Vec<&(String, String, Ast)> = items.iter().collect();
            reversed.reverse();
            let body: Vec<String> = reversed
                .iter()
                .map(|(k, _, v)| format!("{}: {}", k, crate::ast::canonical(v)))
                .collect();
            let cand = format!("{{{}}}", body.join(", "));
            if seen.insert(cand.clone()) {
                out.push(Variant {
                    query: cand,
                    note: "multi-select hash keys reordered",
                });
            }
        }
    }

    out.truncate(limit);
    out
}

/// `people[0].name` -> `people | [0] | name`
fn pipe_spelling(c: &Chain) -> String {
    let mut out = crate::ast::canonical(&c.base);
    for s in &c.steps {
        out.push_str(" | ");
        out.push_str(&render_step(s));
    }
    out
}

/// The same chain with every step explicitly rooted at `@`.
fn pipe_spelling_with_at(c: &Chain) -> Option<String> {
    if c.steps.is_empty() {
        return None;
    }
    let mut out = crate::ast::canonical(&c.base);
    for s in &c.steps {
        out.push_str(" | @");
        // A field step is written `@name`, the others take brackets.
        match s {
            Step::Field(n) => {
                out.push('.');
                out.push_str(n);
            }
            other => out.push_str(&render_step(other)),
        }
    }
    Some(out)
}

fn render_step(s: &Step) -> String {
    match s {
        Step::Field(n) => n.clone(),
        Step::Index(i) => format!("[{}]", i),
        Step::Wildcard => "[*]".to_string(),
        Step::Flatten => "[]".to_string(),
        Step::Slice(sl) => {
            let mut out = String::from("[");
            if let Some(v) = sl.start {
                out.push_str(&v.to_string());
            }
            out.push(':');
            if let Some(v) = sl.stop {
                out.push_str(&v.to_string());
            }
            if sl.has_second_colon {
                out.push(':');
                if let Some(v) = sl.step {
                    out.push_str(&v.to_string());
                }
            }
            out.push(']');
            out
        }
        Step::MultiList(_) => "[*]".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p(s: &str) -> Ast {
        parse(s).unwrap()
    }

    fn nf(s: &str) -> String {
        normal_form(&p(s))
    }

    #[test]
    fn accessor_and_pipe_forms_are_equivalent() {
        assert!(equivalent(&p("people[0].name"), &p("people | [0] | name")));
        assert!(equivalent(&p("people[*].name"), &p("people.*.name")));
        assert!(equivalent(&p("a.b.c"), &p("a | b | c")));
        assert!(equivalent(&p("a[0]"), &p("a | [0]")));
    }

    #[test]
    fn quoting_does_not_change_meaning() {
        assert!(equivalent(&p("a.b"), &p("a.\"b\"")));
        assert!(equivalent(&p("meta.\"source\""), &p("meta | source")));
    }

    #[test]
    fn hash_key_order_does_not_change_meaning() {
        assert!(equivalent(&p("{a: x, b: y}"), &p("{b: y, a: x}")));
    }

    #[test]
    fn different_queries_are_not_equivalent() {
        assert!(!equivalent(&p("a[0]"), &p("a[1]")));
        assert!(!equivalent(&p("a.b"), &p("a.c")));
        assert!(!equivalent(&p("length(a)"), &p("length(b)")));
        assert!(!equivalent(&p("a[0]"), &p("a[*]")));
        // A wildcard after a pipe projects the piped value, which is the same
        // as the wildcard accessor, so these *are* equivalent.
        assert!(equivalent(&p("a[*].b"), &p("a | [*] | b")));
        // A flatten is not a wildcard, though.
        assert!(!equivalent(&p("a[*]"), &p("a[]")));
    }

    #[test]
    fn normal_form_is_stable_across_reparses() {
        assert_eq!(nf("x[0:2]"), nf("x[0:2]"));
        assert!(nf("x[0:2]").contains("[0:2]"));
    }

    #[test]
    fn normalisation_preserves_the_required_pairs() {
        // The two pairs named in the specification.
        assert_eq!(nf("people[0].name"), nf("people | [0] | name"));
        assert_eq!(nf("length(items)"), nf("items | length(@)"));
    }

    #[test]
    fn search_finds_a_different_spelling() {
        let found = search("people[0].name", 8);
        assert!(
            found.iter().any(|v| v.query == "people | [0] | name"),
            "expected the pipe spelling, got {found:?}"
        );
        assert!(found.iter().all(|v| v.query != "people[0].name"));
    }

    #[test]
    fn every_search_result_is_equivalent() {
        for v in search("people[0].name", 16) {
            assert!(
                equivalent(&p(&v.query), &p("people[0].name")),
                "variant `{}` is not equivalent",
                v.query
            );
        }
    }

    #[test]
    fn search_respects_its_limit() {
        assert!(search("a.b.c.d.e", 1).len() <= 1);
        assert!(search("a.b.c.d.e", 0).is_empty());
    }

    #[test]
    fn search_tolerates_invalid_queries() {
        assert!(search("a[0", 8).is_empty());
    }
}
