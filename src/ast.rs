//! AST node definitions.
//!
//! The AST produced by the parser is deliberately *untyped*: parsing never
//! consults the type system, so `parse` reports only genuine syntax errors.
//! [`crate::types::solve`] then produces a second tree by cloning the parsed
//! nodes and filling in [`Ast::ty`].  Keeping the two phases apart is what lets
//! the parser and the type checker emit separate, precise diagnostics.

use crate::lexer::Span;
use crate::types::Type;

/// Built-in function families understood by this subset.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum Builtin {
    Length,
    Keys,
    Values,
    Contains,
    Merge,
}

impl Builtin {
    pub const ALL: [Builtin; 5] = [
        Builtin::Length,
        Builtin::Keys,
        Builtin::Values,
        Builtin::Contains,
        Builtin::Merge,
    ];

    pub fn name(self) -> &'static str {
        match self {
            Builtin::Length => "length",
            Builtin::Keys => "keys",
            Builtin::Values => "values",
            Builtin::Contains => "contains",
            Builtin::Merge => "merge",
        }
    }

    pub fn arity(self) -> usize {
        match self {
            Builtin::Length | Builtin::Keys | Builtin::Values | Builtin::Merge => 1,
            Builtin::Contains => 2,
        }
    }

    pub fn from_name(name: &str) -> Option<Builtin> {
        Builtin::ALL.iter().copied().find(|b| b.name() == name)
    }
}

/// Slice component of an index expression.  `None` means "unbounded on this
/// side", exactly like the JMESPath slice syntax `[start:stop:step]`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct SliceSpec {
    pub start: Option<i64>,
    pub stop: Option<i64>,
    pub step: Option<i64>,
    pub has_second_colon: bool,
}

/// `bracket` specifier contents.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum BracketSpec {
    /// `[*]` / `.*`
    Wildcard,
    /// `[n]` / `[?n]`
    Index(i64),
    /// `[:stop:step]` and friends.
    Slice(SliceSpec),
    /// `[[n]]` — flatten then index.
    Nested(Vec<Ast>),
    /// `[a, b]` multi-select list.
    List(Vec<Ast>),
    /// `[?cond]` — recognised so the parser can emit a dedicated diagnostic.
    Filter(Span),
}

/// How a node's accessor chain is shaped for evaluation.
///
/// This is computed **once**, when the query is compiled, so that evaluation is
/// a straight tree walk with no repeated chain analysis.  Without it every
/// projection in a deep chain would be rediscovered on each of the tens of
/// thousands of evaluations a batch job performs.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum EvalShape {
    /// No projection in the chain: evaluate the node directly.
    Plain,
    /// The chain projects at `source`, then applies `rest` to every element.
    Projected {
        /// The projection node itself (wildcard, slice, flatten or `||`).
        source: Box<Ast>,
        /// The accessors that follow the projection, rooted at the current node.
        rest: Box<Ast>,
        /// Whether the projection is a flatten (`a[]`), which splices first.
        flatten: bool,
    },
}

/// One AST node.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Ast {
    pub kind: AstKind,
    pub span: Span,
    /// Filled in by the type solver; `Type::Any` before solving.
    pub ty: Type,
    /// Evaluation shape, computed once at compile time.
    pub chain: EvalShape,
}

/// Node payloads.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AstKind {
    /// The document root, implicitly `@` typed before solving.
    Root,
    /// `@` — the current node.
    Current,
    /// `&expr` — re-checked sub-expression evaluated against the current node.
    RefExpr(Box<Ast>),
    /// Bare identifier; always a field access on the current node.
    Ident(String),
    /// Quoted identifier; always a field access, even when leading.
    QuotedField(String),
    /// `a.b`
    Field(Box<Ast>, String),
    /// `a[*]`, `a.*`
    Wildcard(Box<Ast>),
    /// `a[...]`
    Bracket(Box<Ast>, BracketSpec),
    /// `a[]`
    Flatten(Box<Ast>),
    /// `lhs[*] rhs...` — the JMESPath flatten projection `a || b`.
    ///
    /// `a || b` means `a[*]` with the accessor chain of `b` applied to every
    /// projected element.  It is *not* the same as `a[]` followed by `b`, which
    /// is why it gets its own node instead of being desugared into a wildcard
    /// plus a field access.
    Projection(Box<Ast>, Box<Ast>),
    /// `a | b`
    Pipe(Box<Ast>, Box<Ast>),
    /// `[a, b, c]`
    MultiSelectList(Vec<Ast>),
    /// `{k: v, ...}`
    MultiSelectHash(Vec<(String, String, Ast)>),
    /// `func(args...)`; `builtin` is `None` for unknown names so the type
    /// checker can produce an "unknown function" diagnostic.
    Call {
        name: String,
        name_span: Span,
        builtin: Option<Builtin>,
        args: Vec<Ast>,
    },
    /// Literal key inside a multi-select hash.
    LiteralKey(String),
    /// An integer literal in value position: `contains(a, 30)`.
    NumberLiteral(i64),
    /// A string literal in value position: `'x'`, `"x"` or `` `x` ``.  Quoting a
    /// string in *value* position yields a literal, whereas quoting it in *field*
    /// position (`a."b"`) yields a field name; the two are distinct nodes.
    StringLiteral(String),
}

impl Ast {
    /// Build a node with an un-inferred type.
    pub fn new(kind: AstKind, span: Span) -> Self {
        Ast {
            kind,
            span,
            ty: Type::Any,
            chain: EvalShape::Plain,
        }
    }

    /// Borrow the node kind.
    pub fn kind(&self) -> &AstKind {
        &self.kind
    }

    /// The node's source span.
    pub fn span(&self) -> Span {
        self.span
    }

    /// The node's inferred type.
    pub fn ty(&self) -> &Type {
        &self.ty
    }

    /// Rewrite the node's inferred type in place.
    pub fn set_ty(&mut self, t: Type) {
        self.ty = t;
    }

    /// Compute and store the evaluation shape of every node in the subtree.
    ///
    /// Call this once after the tree is built; evaluation then needs no further
    /// analysis.
    pub fn annotate_chains(&mut self) {
        let chain = self.compute_chain();
        self.chain = chain;
        for child in self.children_mut() {
            child.annotate_chains();
        }
    }

    /// Mutable access to every direct child.
    fn children_mut(&mut self) -> Vec<&mut Ast> {
        match &mut self.kind {
            AstKind::RefExpr(e)
            | AstKind::Field(e, _)
            | AstKind::Wildcard(e)
            | AstKind::Flatten(e) => vec![e.as_mut()],
            AstKind::Bracket(e, _) => vec![e.as_mut()],
            AstKind::Pipe(a, b) | AstKind::Projection(a, b) => vec![a.as_mut(), b.as_mut()],
            AstKind::MultiSelectList(items) => items.iter_mut().collect(),
            AstKind::MultiSelectHash(items) => items.iter_mut().map(|(_, _, v)| v).collect(),
            AstKind::Call { args, .. } => args.iter_mut().collect(),
            _ => Vec::new(),
        }
    }

    /// Split this node at its left-most projection.
    fn compute_chain(&self) -> EvalShape {
        match &self.kind {
            // A projection node is the source, but its operand may itself
            // contain an earlier projection: in `people[*].tags[]` the operand
            // `people[*].tags` projects over people and applies `tags`, and only
            // then does the outer flatten splice.  That inner split is kept as
            // the source, and the outer node becomes part of the remainder.
            AstKind::Projection(_, r) => EvalShape::Projected {
                source: Box::new(self.clone()),
                rest: Box::new(r.as_ref().clone()),
                flatten: false,
            },
            AstKind::Flatten(l) => {
                // Resolve the operand first: in `a[*].b[]` the operand `a[*].b`
                // projects over `a` and applies `b` per element, and only then
                // does the flatten splice across everything it produced.
                match l.compute_chain() {
                    EvalShape::Projected { source, rest, .. } => EvalShape::Projected {
                        source,
                        rest,
                        flatten: true,
                    },
                    // A plain operand is projected directly and then spliced.
                    EvalShape::Plain => EvalShape::Projected {
                        source: Box::new(l.as_ref().clone()),
                        rest: Box::new(Ast::new(AstKind::Current, self.span)),
                        flatten: true,
                    },
                }
            }
            AstKind::Wildcard(_)
            | AstKind::Bracket(_, BracketSpec::Wildcard)
            | AstKind::Bracket(_, BracketSpec::Slice(_)) => EvalShape::Projected {
                source: Box::new(self.clone()),
                rest: Box::new(Ast::new(AstKind::Current, self.span)),
                flatten: false,
            },
            // Accessors: if the left side projects, keep descending and extend the
            // remaining accessors.
            AstKind::Field(l, name) => match l.compute_chain() {
                EvalShape::Projected {
                    source,
                    rest,
                    flatten,
                } => EvalShape::Projected {
                    source,
                    rest: Box::new(Ast::new(AstKind::Field(rest, name.clone()), self.span)),
                    flatten,
                },
                EvalShape::Plain => EvalShape::Plain,
            },
            AstKind::Bracket(l, spec) => match l.compute_chain() {
                EvalShape::Projected {
                    source,
                    rest,
                    flatten,
                } => EvalShape::Projected {
                    source,
                    rest: Box::new(Ast::new(AstKind::Bracket(rest, spec.clone()), self.span)),
                    flatten,
                },
                EvalShape::Plain => EvalShape::Plain,
            },
            _ => EvalShape::Plain,
        }
    }

    /// Depth of the subtree, used for recursion guards and diagnostics.
    pub fn depth(&self) -> usize {
        match &self.kind {
            AstKind::Root
            | AstKind::Current
            | AstKind::Ident(_)
            | AstKind::QuotedField(_)
            | AstKind::LiteralKey(_)
            | AstKind::StringLiteral(_)
            | AstKind::NumberLiteral(_) => 0,
            AstKind::RefExpr(e) => e.depth(),
            AstKind::Field(l, _)
            | AstKind::Wildcard(l)
            | AstKind::Bracket(l, _)
            | AstKind::Flatten(l) => l.depth() + 1,
            AstKind::Projection(l, r) => l.depth().max(r.depth()) + 1,
            AstKind::Pipe(l, r) => l.depth().max(r.depth()) + 1,
            AstKind::MultiSelectList(items) => {
                items.iter().map(|i| i.depth()).max().unwrap_or(0) + 1
            }
            AstKind::MultiSelectHash(items) => {
                items.iter().map(|(_, _, v)| v.depth()).max().unwrap_or(0) + 1
            }
            AstKind::Call { args, .. } => args.iter().map(|a| a.depth()).max().unwrap_or(0) + 1,
        }
    }
}

/// A node with a fully resolved type annotation.
pub type TypedAst = Ast;

/// Short human readable description of a node kind, for diagnostics.
pub fn describe_kind(k: &AstKind) -> &'static str {
    match k {
        AstKind::Root => "root",
        AstKind::Current => "`@`",
        AstKind::RefExpr(_) => "reference expression",
        AstKind::Ident(_) => "identifier",
        AstKind::QuotedField(_) => "quoted field",
        AstKind::Field(_, _) => "field access",
        AstKind::Wildcard(_) => "wildcard projection",
        AstKind::Bracket(_, _) => "index expression",
        AstKind::Flatten(_) => "flatten accessor",
        AstKind::Projection(_, _) => "flatten projection",
        AstKind::Pipe(_, _) => "pipe",
        AstKind::MultiSelectList(_) => "multi-select list",
        AstKind::MultiSelectHash(_) => "multi-select hash",
        AstKind::Call { .. } => "function call",
        AstKind::LiteralKey(_) => "literal key",
        AstKind::StringLiteral(_) => "string literal",
        AstKind::NumberLiteral(_) => "number literal",
    }
}

/// Render a compact textual form of the AST.
///
/// This is a *printing* helper, not a semantic normaliser: two differently
/// written but equivalent queries render differently, which is exactly why
/// [`crate::equiv`] performs structural comparison over a normal form rather
/// than string comparison.
pub fn canonical(a: &Ast) -> String {
    let mut out = String::new();
    render(a, &mut out);
    out
}

fn render(a: &Ast, out: &mut String) {
    match &a.kind {
        AstKind::Root | AstKind::Current => out.push('@'),
        AstKind::RefExpr(e) => {
            out.push('&');
            render(e, out);
        }
        AstKind::Ident(s) => out.push_str(s),
        AstKind::StringLiteral(s) => {
            out.push('\'');
            out.push_str(s);
            out.push('\'');
        }
        AstKind::NumberLiteral(n) => out.push_str(&n.to_string()),
        AstKind::QuotedField(s) | AstKind::LiteralKey(s) => {
            out.push('"');
            out.push_str(s);
            out.push('"');
        }
        AstKind::Field(l, f) => {
            // Field access is always written with a dot in the source grammar.
            render(l, out);
            out.push('.');
            out.push_str(f);
        }
        AstKind::Wildcard(l) => {
            render_target(l, out);
            out.push_str("[*]");
        }
        AstKind::Bracket(l, spec) => {
            render_target(l, out);
            render_spec(spec, out);
        }
        AstKind::Flatten(l) => {
            render_target(l, out);
            out.push_str("[]");
        }
        AstKind::Projection(l, r) => {
            render_target(l, out);
            out.push_str("||");
            render(r, out);
        }
        AstKind::Pipe(l, r) => {
            render(l, out);
            out.push('|');
            render(r, out);
        }
        AstKind::MultiSelectList(items) => {
            out.push('[');
            for (i, it) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                render(it, out);
            }
            out.push(']');
        }
        AstKind::MultiSelectHash(items) => {
            out.push('{');
            for (i, (k, _, v)) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                out.push('"');
                out.push_str(k);
                out.push_str("\":");
                render(v, out);
            }
            out.push('}');
        }
        AstKind::Call { name, args, .. } => {
            out.push_str(name);
            out.push('(');
            for (i, x) in args.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                render(x, out);
            }
            out.push(')');
        }
    }
}

/// Render the left-hand side of a `.` / `[]` accessor.
///
/// Render the left-hand side of a `[...]` or `||` accessor.
///
/// Only a bare or quoted identifier binds tightly enough to need no separating
/// dot (`people[0]`, not `people.[0]`); `@` and calls need one (`@[0]`, `f(x)[0]`).
fn render_target(a: &Ast, out: &mut String) {
    render(a, out);
    // Only a bare or quoted identifier binds tightly enough to need no dot:
    // `people[0].name` needs the dot, `a.b.c` does not.
    let needs_dot = !matches!(a.kind, AstKind::Ident(_) | AstKind::QuotedField(_));
    if needs_dot {
        out.push('.');
    }
}

fn render_spec(spec: &BracketSpec, out: &mut String) {
    match spec {
        BracketSpec::Wildcard => out.push_str("[*]"),
        BracketSpec::Index(i) => {
            out.push('[');
            out.push_str(&i.to_string());
            out.push(']');
        }
        BracketSpec::Slice(s) => {
            out.push('[');
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
        }
        BracketSpec::Nested(items) => {
            for it in items {
                out.push('[');
                render(it, out);
                out.push(']');
            }
        }
        BracketSpec::List(items) => {
            out.push('[');
            for (i, it) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                render(it, out);
            }
            out.push(']');
        }
        BracketSpec::Filter(_) => out.push_str("[?...]"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(src: &str) -> Ast {
        crate::parser::parse(src).unwrap()
    }

    #[test]
    fn builtin_metadata_is_consistent() {
        for b in Builtin::ALL {
            assert_eq!(Builtin::from_name(b.name()), Some(b));
        }
        assert_eq!(Builtin::from_name("sort_by"), None);
        assert_eq!(Builtin::Contains.arity(), 2);
    }

    #[test]
    fn canonical_rendering_is_stable() {
        assert_eq!(canonical(&at("people[0].name")), "people[0].name");
        assert_eq!(canonical(&at("@.name")), "@.name");
        assert_eq!(canonical(&at("[a,b]")), "[a,b]");
    }

    #[test]
    fn canonical_distinguishes_different_spellings() {
        // Printed forms differ; equivalence is decided structurally, not textually.
        assert_ne!(
            canonical(&at("people[0].name")),
            canonical(&at("people | [0] | name"))
        );
        assert_ne!(
            canonical(&at("length(items)")),
            canonical(&at("items | length(@)"))
        );
    }

    #[test]
    fn slice_spec_renders_all_forms() {
        assert_eq!(canonical(&at("a[1:2:3]")), "a[1:2:3]");
        assert_eq!(canonical(&at("a[:2]")), "a[:2]");
        assert_eq!(canonical(&at("a[::2]")), "a[::2]");
    }

    #[test]
    fn describe_kind_covers_every_variant() {
        let kinds = [
            AstKind::Root,
            AstKind::Current,
            AstKind::RefExpr(Box::new(at("a"))),
            AstKind::Ident("a".into()),
            AstKind::QuotedField("a".into()),
            AstKind::Field(Box::new(at("a")), "b".into()),
            AstKind::Wildcard(Box::new(at("a"))),
            AstKind::Bracket(Box::new(at("a")), BracketSpec::Index(0)),
            AstKind::Flatten(Box::new(at("a"))),
            AstKind::Pipe(Box::new(at("a")), Box::new(at("b"))),
            AstKind::MultiSelectList(vec![]),
            AstKind::MultiSelectHash(vec![]),
            AstKind::Call {
                name: "length".into(),
                name_span: Span::dummy(),
                builtin: Some(Builtin::Length),
                args: vec![],
            },
            AstKind::LiteralKey("k".into()),
            AstKind::StringLiteral("k".into()),
            AstKind::NumberLiteral(1),
        ];
        let mut seen: Vec<&str> = kinds.iter().map(describe_kind).collect();
        seen.sort_unstable();
        seen.dedup();
        assert_eq!(seen.len(), kinds.len(), "describe_kind is not injective");
    }
}
