//! Structural type lattice, constraint generation, unification, and minimal
//! unsatisfiable-subset extraction.
//!
//! # Design
//!
//! Types describe *shapes of `serde_json::Value`*, not Rust types:
//!
//! ```text
//! Any       - any JSON value (top of the lattice)
//! Null      - null
//! Bool      - true / false
//! Number    - any JSON number
//! String    - JSON string
//! Array(T)  - array whose element shape is T
//! Object(F) - object; F maps a key to its value shape
//! ```
//!
//! The input document is only known at run time, so the checker never assumes a
//! concrete root shape.  Instead it works in two stages.
//!
//! **Stage 1 — requirement generation.**  Every construct that can fail at run
//! time under JMESPath's "absent data yields `null`" rule emits a *requirement*
//! instead of an error: `a[0]` requires the shape of `a` to admit an array,
//! `a.b` requires `a` to admit an object carrying key `b`, `keys(a)` requires `a`
//! to admit an object, and so on.  A requirement is a predicate over a symbolic
//! shape variable.
//!
//! **Stage 2 — satisfiability + minimisation.**  A requirement is satisfiable
//! when some shape satisfies it.  Because each requirement is satisfiable *in
//! isolation* (the root variable is always free to adopt the needed shape), the
//! interesting failures are *conflicts*: two requirements that force the same
//! variable into incompatible shapes.  We therefore solve by bounded
//! unification over shape variables, and on failure run deletion-based
//! minimisation to extract a **minimal unsatisfiable subset** — an irreducible
//! set of requirements that cannot all hold at once.
//!
//! A realistic use case for a *hard* error: a document is known to be an object
//! (analyst passes `{"people": [...], ...}`), and a query indexes a scalar.
//! That is expressed through [`solve_with_shape`], which pins the root to a
//! concrete shape and makes the conflict observable at compile time — which is
//! exactly the class of bug the pipeline wants to catch before a 40-minute batch
//! run discovers it halfway through.

use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc as Rc;

use crate::ast::{Ast, AstKind, BracketSpec, Builtin, TypedAst};
use crate::lexer::Span;

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/// A structural JSON shape.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub enum Type {
    /// Top of the lattice: any JSON value.
    Any,
    Null,
    Bool,
    Number,
    String,
    Array(Box<Type>),
    /// Object with a map from key to value shape.  A `BTreeMap` keeps key
    /// iteration deterministic, which matters for reproducibility.
    Object(Rc<BTreeMap<String, Type>>),
}

impl Type {
    pub fn array(inner: Type) -> Type {
        Type::Array(Box::new(inner))
    }

    pub fn object(fields: Vec<(&str, Type)>) -> Type {
        let mut m = BTreeMap::new();
        for (k, v) in fields {
            m.insert(k.to_string(), v);
        }
        Type::Object(Rc::new(m))
    }

    /// Is this the unconstrained top type?
    pub fn is_any(&self) -> bool {
        matches!(self, Type::Any)
    }

    /// The JSON type name, as used in diagnostics.
    pub fn json_name(&self) -> &'static str {
        match self {
            Type::Any => "any",
            Type::Null => "null",
            Type::Bool => "boolean",
            Type::Number => "number",
            Type::String => "string",
            Type::Array(_) => "array",
            Type::Object(_) => "object",
        }
    }
}

impl fmt::Display for Type {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Type::Any => f.write_str("any"),
            Type::Null => f.write_str("null"),
            Type::Bool => f.write_str("boolean"),
            Type::Number => f.write_str("number"),
            Type::String => f.write_str("string"),
            Type::Array(t) => write!(f, "array<{}>", t),
            Type::Object(m) => {
                f.write_str("object{")?;
                for (i, (k, v)) in m.iter().enumerate() {
                    if i > 0 {
                        f.write_str(", ")?;
                    }
                    write!(f, "{}: {}", k, v)?;
                }
                f.write_str("}")
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Unification
// ---------------------------------------------------------------------------

/// First-order unification over the shape lattice.
///
/// * `Any` is the top type and absorbs anything (covariant unification).
/// * Two objects unify when they agree on every key mentioned by both; keys
///   present in only one side are open (that side simply does not constrain them).
/// * Two arrays unify when their elements unify.
/// * Anything else that is not syntactically identical is a conflict, and the
///   two offending types are returned for the diagnostic.
pub fn unify(a: &Type, b: &Type) -> Result<(), (Type, Type)> {
    match (a, b) {
        (Type::Any, _) | (_, Type::Any) => Ok(()),
        (Type::Null, Type::Null)
        | (Type::Bool, Type::Bool)
        | (Type::Number, Type::Number)
        | (Type::String, Type::String) => Ok(()),
        (Type::Array(x), Type::Array(y)) => unify(x, y),
        (Type::Object(f), Type::Object(g)) => {
            // Only keys known to both sides must agree.
            for (k, t) in f.iter() {
                if let Some(u) = g.get(k) {
                    unify(t, u)?;
                }
            }
            Ok(())
        }
        _ => Err((a.clone(), b.clone())),
    }
}

// ---------------------------------------------------------------------------
// Requirements
// ---------------------------------------------------------------------------

/// What a requirement demands of its operand shape.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ReqKind {
    /// Operand must admit indexing as an array.
    ArrayIndex,
    /// Operand must admit a wildcard projection (object or array).
    Wildcard,
    /// Operand must admit flattening as an array.
    Flatten,
    /// Operand must admit an object carrying this key.
    Field(String),
    /// Built-in call with already-checked argument shapes.
    Call { builtin: Builtin, args: Vec<Type> },
}

impl ReqKind {
    /// Text used in the minimal-unsatisfiable-subset report.
    fn render(&self, operand: Option<&Type>) -> String {
        let subject = match operand {
            Some(t) => format!("{}", t),
            None => "document root".to_string(),
        };
        match self {
            ReqKind::ArrayIndex => format!("{} supports indexing (must be an array)", subject),
            ReqKind::Wildcard => format!(
                "{} supports wildcard projection (must be an object or an array)",
                subject
            ),
            ReqKind::Flatten => format!("{} supports flattening (must be an array)", subject),
            ReqKind::Field(k) => format!("{} is an object containing key `{}`", subject, k),
            ReqKind::Call { builtin, args } => {
                let a: Vec<String> = args.iter().map(|t| t.to_string()).collect();
                format!(
                    "`{}` accepts ({})",
                    builtin.name(),
                    if a.is_empty() {
                        "no arguments".into()
                    } else {
                        a.join(", ")
                    }
                )
            }
        }
    }

    /// The shape an operand is *forced* into when this requirement is applied.
    /// `None` means the requirement is not a hard shape demand (it only rejects
    /// certain shapes).
    fn forces(&self) -> Option<Type> {
        match self {
            ReqKind::Field(k) => Some(Type::object(vec![(k.as_str(), Type::Any)])),
            ReqKind::ArrayIndex | ReqKind::Flatten => Some(Type::array(Type::Any)),
            ReqKind::Wildcard | ReqKind::Call { .. } => None,
        }
    }

    /// Does this requirement reject the given shape outright?
    fn rejects(&self, t: &Type) -> bool {
        match self {
            ReqKind::ArrayIndex | ReqKind::Flatten => !matches!(t, Type::Array(_) | Type::Any),
            ReqKind::Wildcard => !matches!(t, Type::Object(_) | Type::Array(_) | Type::Any),
            ReqKind::Field(_) => !matches!(t, Type::Object(_) | Type::Any),
            ReqKind::Call { builtin, args } => !call_admits(*builtin, args, t),
        }
    }
}

/// A short verb phrase for a requirement, used in the "cannot ..." message.
fn describe_requirement(k: &ReqKind) -> String {
    match k {
        ReqKind::ArrayIndex => "index it: an index or slice requires an array".into(),
        ReqKind::Wildcard => "project over it: a projection requires an array or an object".into(),
        ReqKind::Flatten => "flatten it: flattening requires an array".into(),
        ReqKind::Field(name) => format!("read field `{}` from it: requires an object", name),
        ReqKind::Call { builtin, args } => {
            let a: Vec<String> = args.iter().map(|t| t.to_string()).collect();
            format!("call `{}` with ({})", builtin.name(), a.join(", "))
        }
    }
}

/// Does builtin `b` applied to `args` accept a subject of shape `t`?
///
/// A subject shape is accepted if *some* refinement of it is accepted, which is
/// the correct notion here: the static shape is an over-approximation of the
/// runtime value, so a type error exists only when *no* refinement can work.
fn call_admits(b: Builtin, args: &[Type], subject: &Type) -> bool {
    match b {
        Builtin::Length => match subject {
            Type::String | Type::Array(_) | Type::Object(_) | Type::Any => true,
            // A number / bool / null can never be measured.
            _ => false,
        },
        Builtin::Keys | Builtin::Values | Builtin::Merge => {
            matches!(subject, Type::Object(_) | Type::Any)
        }
        Builtin::Contains => {
            // First argument is the haystack: a string or an array.
            let _ = args;
            matches!(subject, Type::String | Type::Array(_) | Type::Any)
        }
    }
}

// ---------------------------------------------------------------------------
// Diagnostics
// ---------------------------------------------------------------------------

/// Category of a type error.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TypeErrorKind {
    /// Two requirements force the same node into incompatible shapes.
    Conflict,
    /// A requirement is rejected outright by a known shape.
    Rejected,
    /// The requirement set is unsatisfiable but no single pair explains it.
    Unsatisfiable,
    /// Function arity mismatch.
    Arity,
    /// Unknown function name.
    UnknownFunction,
    /// Unsupported construct (e.g. filters) detected during type checking.
    Unsupported,
    /// Query nesting exceeded the configured depth budget.
    DepthLimit,
}

impl TypeErrorKind {
    pub fn as_str(self) -> &'static str {
        match self {
            TypeErrorKind::Conflict => "type conflict",
            TypeErrorKind::Rejected => "type error",
            TypeErrorKind::Unsatisfiable => "unsatisfiable query",
            TypeErrorKind::Arity => "wrong arity",
            TypeErrorKind::UnknownFunction => "unknown function",
            TypeErrorKind::Unsupported => "unsupported construct",
            TypeErrorKind::DepthLimit => "query too deep",
        }
    }
}

impl fmt::Display for TypeErrorKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A single located diagnostic.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Diagnostic {
    pub kind: TypeErrorKind,
    pub message: String,
    pub span: Span,
    /// Rendered types involved in the conflict, when applicable.
    pub types: Vec<String>,
}

impl fmt::Display for Diagnostic {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.kind, self.message)?;
        if !self.types.is_empty() {
            write!(f, " [{}]", self.types.join(" vs "))?;
        }
        write!(f, " at {}", self.span.start)
    }
}

/// A type-check failure, carrying individual diagnostics plus the minimal
/// unsatisfiable subset that explains them.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TypeError {
    pub diagnostics: Vec<Diagnostic>,
    /// Ids of the requirements forming a minimal unsatisfiable subset, in
    /// source order.
    pub minimal_core: Vec<usize>,
    /// One rendered line per entry of `minimal_core`, aligned with it.
    pub core_constraints: Vec<String>,
}

impl TypeError {
    /// A single-diagnostic error with no minimisation pass (used for errors
    /// detected before the requirement set exists).
    pub fn simple(kind: TypeErrorKind, message: impl Into<String>, span: Span) -> TypeError {
        TypeError {
            diagnostics: vec![Diagnostic {
                kind,
                message: message.into(),
                span,
                types: vec![],
            }],
            minimal_core: vec![],
            core_constraints: vec![],
        }
    }
}

impl fmt::Display for TypeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let n = self.diagnostics.len();
        writeln!(
            f,
            "static type check failed with {} diagnostic{}:",
            n,
            if n == 1 { "" } else { "s" }
        )?;
        for d in &self.diagnostics {
            writeln!(f, "  - {d}")?;
        }
        if !self.minimal_core.is_empty() {
            writeln!(
                f,
                "  minimal unsatisfiable subset ({} requirement{}):",
                self.minimal_core.len(),
                if self.minimal_core.len() == 1 {
                    ""
                } else {
                    "s"
                }
            )?;
            for c in &self.core_constraints {
                writeln!(f, "    * {c}")?;
            }
        }
        Ok(())
    }
}

impl std::error::Error for TypeError {}

/// Shorthand result type.
pub type TypeResult<T> = Result<T, TypeError>;

// ---------------------------------------------------------------------------
// Solver configuration
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug)]
pub struct SolverConfig {
    /// Maximum AST depth explored.
    pub max_depth: usize,
    /// Upper bound on deletion probes during core minimisation.  Reaching the
    /// bound degrades gracefully: the core is then a minimal *prefix* rather
    /// than fully irreducible.
    pub max_core_probes: usize,
}

impl Default for SolverConfig {
    fn default() -> Self {
        SolverConfig {
            max_depth: 256,
            max_core_probes: 20_000,
        }
    }
}

// ---------------------------------------------------------------------------
// Requirement collection
// ---------------------------------------------------------------------------

/// Flat view of one requirement in the set handed to the solver.
#[derive(Clone, Debug)]
struct Requirement {
    /// Monotonic id, kept for stable ordering in reports and tests.
    #[allow(dead_code)]
    id: usize,
    /// Identity of the node this requirement constrains.
    ///
    /// Requirements only interact when they constrain the *same* node, so this
    /// is what the solver groups by.  The operand shape alone is not enough:
    /// two unrelated nodes can both be `any`, and treating them as one node
    /// would invent conflicts that do not exist.
    node: NodeId,
    operand: Option<Type>,
    kind: ReqKind,
    span: Span,
}

/// Identity of the AST node a requirement constrains.
///
/// The canonical rendering alone is enough and is the right key: within one
/// parsed query, two sub-nodes with the same canonical text are the same node for
/// constraint purposes (they denote the same path), while different paths render
/// differently.  Using source offsets instead would split `people` in `a.people`
/// and `people` in `people[0]` into different nodes even though both
/// requirements constrain the document root.
type NodeId = String;

/// Collects requirements and drives unification + minimisation.
#[derive(Debug)]
pub struct Solver {
    cfg: SolverConfig,
    reqs: Vec<Requirement>,
    /// Diagnostic sink, filled while solving.
    diags: Vec<Diagnostic>,
}

impl Solver {
    pub fn new(cfg: SolverConfig) -> Self {
        Solver {
            cfg,
            reqs: Vec::new(),
            diags: Vec::new(),
        }
    }

    /// Solve a parsed query with no knowledge of the document shape.
    ///
    /// Every requirement is individually satisfiable because the root shape is
    /// unconstrained, so this returns an annotated tree unless the query is
    /// structurally impossible (unknown function, bad arity, filters, ...).
    pub fn solve(self, ast: &Ast) -> TypeResult<TypedAst> {
        self.solve_with_shape(ast, None)
    }

    /// Solve a parsed query against a declared document shape.
    ///
    /// Supplying `Some(shape)` pins the document root, which is what turns
    /// requirements into checkable predicates and surfaces conflicts such as
    /// indexing a scalar.  This is the mode the risk-control pipeline uses: the
    /// envelope schema is registered once per upstream system.
    pub fn solve_with_shape(self, ast: &Ast, shape: Option<&Type>) -> TypeResult<TypedAst> {
        // 1. One structural pass: depth budget, unsupported constructs, and
        //    call signatures, all before any requirement is generated.
        precheck(ast, 0, self.cfg.max_depth)?;

        // 2. Generate requirements.
        let mut collector = Collector {
            reqs: Vec::new(),
            next_id: 0,
            root: shape.cloned(),
            scope: 0,
        };
        collector.walk(ast)?;

        // 3. Satisfiability + minimisation.
        let mut solver = Solver {
            cfg: self.cfg,
            reqs: collector.reqs,
            diags: Vec::new(),
        };
        let (core, diags) = solver.analyze();
        solver.diags = diags;

        if !core.is_empty() {
            let core_constraints = core
                .iter()
                .map(|&i| {
                    let r = &solver.reqs[i];
                    r.kind.render(r.operand.as_ref())
                })
                .collect();
            return Err(TypeError {
                diagnostics: solver.diags.clone(),
                minimal_core: core,
                core_constraints,
            });
        }
        if !solver.diags.is_empty() {
            return Err(TypeError {
                diagnostics: solver.diags.clone(),
                minimal_core: vec![],
                core_constraints: vec![],
            });
        }

        // 4. Annotate the tree bottom-up by re-walking it.
        annotate(ast, shape.cloned())
    }

    /// Decide satisfiability of the whole requirement set and, when it is
    /// unsatisfiable, extract a **minimal unsatisfiable subset**.
    ///
    /// A requirement is a predicate over the shape of one node.  A set of
    /// requirements is satisfiable when every node's shape can be chosen so that
    /// all requirements on it hold.  Concretely that means, per node:
    ///
    /// * every requirement must *admit* the node's declared shape, and
    /// * the shapes the requirements *force* on the node must unify.
    ///
    /// Satisfiability is therefore a per-node check, which makes deletion-based
    /// minimisation exact: dropping any requirement from a minimal core restores
    /// satisfiability, so the reported core really is irreducible.
    fn analyze(&mut self) -> (Vec<usize>, Vec<Diagnostic>) {
        // Pass 1: requirements rejected outright by a known shape.
        let mut hard: Vec<usize> = Vec::new();
        let mut diags: Vec<Diagnostic> = Vec::new();
        for (i, r) in self.reqs.iter().enumerate() {
            if let Some(op) = &r.operand {
                if r.kind.rejects(op) {
                    diags.push(Diagnostic {
                        kind: TypeErrorKind::Rejected,
                        message: format!(
                            "cannot {} — this node has type {}",
                            describe_requirement(&r.kind),
                            op
                        ),
                        span: r.span,
                        types: vec![],
                    });
                    hard.push(i);
                }
            }
        }
        if !hard.is_empty() {
            // Each of these is rejected on its own, so the set is already
            // minimal: no member can be dropped while the rest stay unsatisfied.
            return (hard, diags);
        }

        // Pass 2: pairwise conflicts — two requirements forcing incompatible
        // shapes onto the same node.
        let all: Vec<usize> = (0..self.reqs.len()).collect();
        if self.satisfiable(&all) {
            return (vec![], diags);
        }
        let core = self.minimise(&all);
        let mut core_diags = self.explain(&core);
        diags.append(&mut core_diags);
        (core, diags)
    }

    /// Is the given requirement subset simultaneously satisfiable?
    ///
    /// A requirement constrains one node, whose shape the document may or may not
    /// fix.  Three cases, checked per node:
    ///
    /// * **Unconstrained shape (`any`)** — the node can adopt whatever the
    ///   requirements demand, so no conflict is possible.  This is what keeps an
    ///   unknown-schema query from being rejected for reasons that only a
    ///   concrete document could settle.
    /// * **Requirement rejects the shape** — the shape is known and outright
    ///   incompatible with what the query needs.  Unsatisfiable.
    /// * **Two requirements force incompatible shapes** — for example `a[0]`
    ///   needs `a` to be an array while `keys(a)` needs it to be an object.
    ///   Unsatisfiable.
    ///
    /// Only the last two are conflicts; the first is always satisfiable.
    fn satisfiable(&self, subset: &[usize]) -> bool {
        let mut groups: BTreeMap<&NodeId, Vec<&Requirement>> = BTreeMap::new();
        for &i in subset {
            let r = &self.reqs[i];
            groups.entry(&r.node).or_default().push(r);
        }
        for group in groups.values() {
            // Every requirement must admit the node's declared shape.
            for r in group {
                if let Some(op) = &r.operand {
                    if r.kind.rejects(op) {
                        return false;
                    }
                }
            }
            // Requirements that force a shape must agree with each other and
            // with the declared shape.  An `any` shape imposes no constraint.
            let declared = group.iter().find_map(|r| match &r.operand {
                Some(t) if !t.is_any() => Some(t.clone()),
                _ => None,
            });
            for r in group {
                let Some(forced) = r.kind.forces() else {
                    continue;
                };
                // The declared shape must admit the forced shape.  With no
                // declared shape the node is free to adopt whatever any single
                // requirement needs, and with no concrete data to pin it down
                // there is no conflict to report — a `keys(a)` and an `a[0]` in
                // the same query only clash once we know what `a` actually is.
                if let Some(d) = &declared {
                    if unify(d, &forced).is_err() {
                        return false;
                    }
                }
            }
        }
        true
    }

    /// Deletion-based minimisation: repeatedly try to drop a requirement, keeping
    /// it dropped for good whenever the remainder is still unsatisfiable.  The
    /// surviving set is irreducible — no member can be removed.
    fn minimise(&self, set: &[usize]) -> Vec<usize> {
        let mut core = set.to_vec();
        let mut i = 0usize;
        let mut probes = 0usize;
        while i < core.len() && probes < self.cfg.max_core_probes {
            probes += 1;
            let mut trial = core.clone();
            trial.remove(i);
            if !trial.is_empty() && !self.satisfiable(&trial) {
                core = trial; // still unsat without it — permanently drop
            } else {
                i += 1;
            }
        }
        core.sort_unstable();
        core
    }

    /// Produce a diagnostic for each member of a minimal core.
    fn explain(&self, core: &[usize]) -> Vec<Diagnostic> {
        let mut out = Vec::new();
        for &i in core {
            let r = &self.reqs[i];
            let message = match (&r.operand, r.kind.forces()) {
                (Some(op), Some(forced)) if op != &forced => format!(
                    "{} conflicts with the shape {} already required for the same node",
                    r.kind.render(Some(op)),
                    forced
                ),
                _ => r.kind.render(r.operand.as_ref()),
            };
            out.push(Diagnostic {
                kind: TypeErrorKind::Conflict,
                message,
                span: r.span,
                types: r.operand.iter().map(|t| t.to_string()).collect(),
            });
        }
        out
    }
}

// ---------------------------------------------------------------------------
// Requirement generation
// ---------------------------------------------------------------------------

struct Collector {
    reqs: Vec<Requirement>,
    next_id: usize,
    /// The shape the *current* node has.  It starts as the document root and is
    /// rebound whenever a construct rebinds the node, which is what lets
    /// `people | [0] | name` see `people`'s real element type.
    root: Option<Type>,
    /// Identity of the current node within the query.
    ///
    /// A bare identifier constrains the current node, so `people` and `name` are
    /// both looked up on "the current node" — but they are current nodes at
    /// different points in the pipeline and must not be treated as one.  This
    /// counter gives each such point a distinct identity.
    scope: usize,
}

/// Stable identity for an AST node.
fn node_key(a: &Ast) -> NodeId {
    crate::ast::canonical(a)
}

/// Resolve the shape of a field lookup in `base`, if `base` is concrete.
fn field_shape(base: &Type, key: &str) -> Type {
    match base {
        Type::Object(m) => m.get(key).cloned().unwrap_or(Type::Any),
        Type::Any => Type::Any,
        // A concrete non-object cannot yield the field at all.
        _ => Type::Any,
    }
}

impl Collector {
    fn push(&mut self, node: &Ast, operand: Option<Type>, kind: ReqKind, span: Span) -> usize {
        self.push_keyed(&node_key(node), operand, kind, span)
    }

    /// Push a requirement against an explicit node identity.
    fn push_keyed(
        &mut self,
        node: &NodeId,
        operand: Option<Type>,
        kind: ReqKind,
        span: Span,
    ) -> usize {
        let id = self.next_id;
        self.next_id += 1;
        self.reqs.push(Requirement {
            id,
            node: node.clone(),
            operand,
            kind,
            span,
        });
        id
    }

    /// Walk `ast` with the current ambient shape.
    fn walk(&mut self, ast: &Ast) -> TypeResult<Type> {
        let ambient = self.root.clone();
        self.walk_in(ast, &ambient)
    }

    /// Walk `ast` against an explicit ambient shape, emitting requirements.
    fn walk_in(&mut self, ast: &Ast, ambient: &Option<Type>) -> TypeResult<Type> {
        let prev = std::mem::replace(&mut self.root, ambient.clone());
        let out = self.walk_inner(ast);
        self.root = prev;
        out
    }

    fn walk_inner(&mut self, ast: &Ast) -> TypeResult<Type> {
        match &ast.kind {
            // The root shape is threaded explicitly rather than recorded as a
            // requirement, so it never takes part in conflict detection.
            AstKind::Root => Ok(self.root.clone().unwrap_or(Type::Any)),
            AstKind::Current => Ok(self.root.clone().unwrap_or(Type::Any)),
            AstKind::RefExpr(inner) => self.walk(inner),
            AstKind::Ident(name) | AstKind::QuotedField(name) => {
                // `foo` is `$.foo`: the lookup constrains the *ambient* node (the
                // document root, or whatever a pipe fed in), not `foo` itself.
                let root = self.root.clone();
                let key = format!("current#{}", self.scope);
                self.push_keyed(&key, root.clone(), ReqKind::Field(name.clone()), ast.span);
                Ok(root
                    .as_ref()
                    .map(|r| field_shape(r, name))
                    .unwrap_or(Type::Any))
            }
            AstKind::LiteralKey(_) | AstKind::StringLiteral(_) => Ok(Type::String),
            AstKind::NumberLiteral(_) => Ok(Type::Number),
            AstKind::Field(lhs, name) => {
                let base = self.walk(lhs)?;
                self.push(
                    lhs,
                    Some(base.clone()),
                    ReqKind::Field(name.clone()),
                    ast.span,
                );
                Ok(field_shape(&base, name))
            }
            AstKind::Wildcard(lhs) => {
                let base = self.walk(lhs)?;
                self.push(lhs, Some(base), ReqKind::Wildcard, ast.span);
                Ok(Type::Any)
            }
            AstKind::Flatten(lhs) => {
                let base = self.walk(lhs)?;
                self.push(lhs, Some(base), ReqKind::Flatten, ast.span);
                Ok(Type::Any)
            }
            AstKind::Projection(lhs, rhs) => {
                // Projecting requires a container on the left; the right-hand
                // chain is checked against each projected element.
                let base = self.walk(lhs)?;
                self.push(lhs, Some(base.clone()), ReqKind::Wildcard, ast.span);
                let elem = match &base {
                    Type::Array(e) => (**e).clone(),
                    Type::Object(m) => m.values().cloned().reduce(unify_join).unwrap_or(Type::Any),
                    _ => Type::Any,
                };
                self.scope += 1;
                self.walk_in(rhs, &Some(elem.clone()))?;
                self.scope -= 1;
                Ok(Type::array(elem))
            }
            AstKind::Bracket(lhs, spec) => {
                let base = self.walk(lhs)?;
                match spec {
                    BracketSpec::Index(_) | BracketSpec::Slice(_) => {
                        self.push(lhs, Some(base), ReqKind::ArrayIndex, ast.span);
                    }
                    BracketSpec::Wildcard => {
                        self.push(lhs, Some(base), ReqKind::Wildcard, ast.span);
                    }
                    BracketSpec::Nested(items) => {
                        self.push(lhs, Some(base), ReqKind::Flatten, ast.span);
                        for it in items {
                            self.walk(it)?;
                        }
                    }
                    BracketSpec::List(items) => {
                        for it in items {
                            self.walk(it)?;
                        }
                    }
                    BracketSpec::Filter(s) => {
                        return Err(TypeError::simple(
                            TypeErrorKind::Unsupported,
                            "filter expressions `[?...]` are not part of this subset",
                            *s,
                        ))
                    }
                }
                Ok(Type::Any)
            }
            AstKind::Pipe(lhs, rhs) => {
                // The right-hand side sees the left-hand side's result, which is
                // what lets `people | [0] | name` resolve `name` against the
                // element shape rather than against the document root.
                let lshape = self.walk(lhs)?;
                self.scope += 1;
                let out = self.walk_in(rhs, &Some(lshape));
                self.scope -= 1;
                out
            }
            AstKind::MultiSelectList(items) => {
                for it in items {
                    self.walk(it)?;
                }
                Ok(Type::Any)
            }
            AstKind::MultiSelectHash(items) => {
                for (_, _, v) in items {
                    self.walk(v)?;
                }
                Ok(Type::Any)
            }
            // `precheck` has already rejected unknown functions and wrong
            // arities, so the builtin is known here and `atys` has the arity the
            // signature expects.
            AstKind::Call { builtin, args, .. } => {
                let mut atys = Vec::with_capacity(args.len());
                for a in args {
                    atys.push(self.walk(a)?);
                }
                let b = builtin.expect("precheck rejects unknown functions");
                debug_assert_eq!(atys.len(), b.arity());
                let subject = atys[0].clone();
                self.push(
                    ast,
                    Some(subject),
                    ReqKind::Call {
                        builtin: b,
                        args: atys,
                    },
                    ast.span,
                );
                Ok(builtin_ret(b))
            }
        }
    }
}

/// Result type of a well-typed built-in.
fn builtin_ret(b: Builtin) -> Type {
    match b {
        Builtin::Length => Type::Number,
        Builtin::Keys => Type::array(Type::String),
        Builtin::Values => Type::array(Type::Any),
        Builtin::Contains => Type::Bool,
        Builtin::Merge => Type::object(vec![("*", Type::Any)]),
    }
}

// ---------------------------------------------------------------------------
// Structural pre-checks and annotation
// ---------------------------------------------------------------------------

/// Single structural pass over the query: enforce the depth budget, reject
/// unsupported constructs, and validate every call.
///
/// Doing this in one pass rather than several keeps the failure for a malformed
/// query deterministic and cheap, and guarantees that a construct the collector
/// later assumes cannot exist has already been rejected.
fn precheck(a: &Ast, depth: usize, max_depth: usize) -> TypeResult<()> {
    if depth > max_depth {
        return Err(TypeError::simple(
            TypeErrorKind::DepthLimit,
            format!("query nests deeper than {} levels", max_depth),
            a.span,
        ));
    }
    match &a.kind {
        AstKind::Root
        | AstKind::Current
        | AstKind::Ident(_)
        | AstKind::QuotedField(_)
        | AstKind::LiteralKey(_)
        | AstKind::StringLiteral(_)
        | AstKind::NumberLiteral(_) => {}
        AstKind::RefExpr(e) => precheck(e, depth + 1, max_depth)?,
        AstKind::Field(l, _) | AstKind::Wildcard(l) | AstKind::Flatten(l) => {
            precheck(l, depth + 1, max_depth)?
        }
        AstKind::Projection(l, r) => {
            precheck(l, depth + 1, max_depth)?;
            precheck(r, depth + 1, max_depth)?;
        }
        AstKind::Bracket(l, spec) => {
            precheck(l, depth + 1, max_depth)?;
            if let BracketSpec::Filter(s) = spec {
                return Err(TypeError::simple(
                    TypeErrorKind::Unsupported,
                    "filter expressions `[?...]` are not part of this subset",
                    *s,
                ));
            }
            for child in bracket_children(spec) {
                precheck(child, depth + 1, max_depth)?;
            }
        }
        AstKind::Pipe(l, r) => {
            precheck(l, depth + 1, max_depth)?;
            precheck(r, depth + 1, max_depth)?;
        }
        AstKind::MultiSelectList(items) => {
            for it in items {
                precheck(it, depth + 1, max_depth)?;
            }
        }
        AstKind::MultiSelectHash(items) => {
            for (_, _, v) in items {
                precheck(v, depth + 1, max_depth)?;
            }
        }
        AstKind::Call {
            name,
            name_span,
            builtin,
            args,
        } => {
            for x in args {
                precheck(x, depth + 1, max_depth)?;
            }
            let Some(b) = builtin else {
                return Err(TypeError::simple(
                    TypeErrorKind::UnknownFunction,
                    format!(
                        "unknown function `{}`; this subset provides {}",
                        name,
                        Builtin::ALL
                            .iter()
                            .map(|x| x.name())
                            .collect::<Vec<_>>()
                            .join(", ")
                    ),
                    *name_span,
                ));
            };
            if args.len() != b.arity() {
                return Err(TypeError::simple(
                    TypeErrorKind::Arity,
                    format!(
                        "`{}` takes {} argument{} but got {}",
                        b.name(),
                        b.arity(),
                        if b.arity() == 1 { "" } else { "s" },
                        args.len()
                    ),
                    *name_span,
                ));
            }
        }
    }
    Ok(())
}

fn bracket_children(spec: &BracketSpec) -> Vec<&Ast> {
    match spec {
        BracketSpec::Nested(items) | BracketSpec::List(items) => items.iter().collect(),
        _ => vec![],
    }
}

/// Produce a typed AST bottom-up, projecting shapes through the constructs.
fn annotate(a: &Ast, root: Option<Type>) -> TypeResult<TypedAst> {
    Ok(annotate_inner(a, &root))
}

fn annotate_inner(a: &Ast, root: &Option<Type>) -> TypedAst {
    let mut node = a.clone();
    node.ty = match &a.kind {
        AstKind::Root => root.clone().unwrap_or(Type::Any),
        AstKind::Current => root.clone().unwrap_or(Type::Any),
        AstKind::RefExpr(inner) => {
            let t = annotate_inner(inner, root);
            node.kind = AstKind::RefExpr(Box::new(t));
            Type::Any
        }
        AstKind::Ident(name) | AstKind::QuotedField(name) => {
            // `foo` is `$.foo`: look the key up in the root shape when known.
            match root {
                Some(Type::Object(m)) => m.get(name).cloned().unwrap_or(Type::Any),
                _ => Type::Any,
            }
        }
        AstKind::LiteralKey(_) | AstKind::StringLiteral(_) => Type::String,
        AstKind::NumberLiteral(_) => Type::Number,
        AstKind::Field(lhs, name) => {
            let lt = annotate_inner(lhs, root);
            let base = lt.ty.clone();
            node.kind = AstKind::Field(Box::new(lt), name.clone());
            match base {
                Type::Object(m) => m.get(name).cloned().unwrap_or(Type::Any),
                _ => Type::Any,
            }
        }
        AstKind::Wildcard(lhs) => {
            let lt = annotate_inner(lhs, root);
            let base = lt.ty.clone();
            node.kind = AstKind::Wildcard(Box::new(lt));
            match base {
                Type::Object(m) => {
                    // Wildcard over an object projects all values.
                    let mut elems = Vec::new();
                    for v in m.values() {
                        elems.push(v.clone());
                    }
                    elems
                        .into_iter()
                        .reduce(unify_join)
                        .map(Type::array)
                        .unwrap_or_else(|| Type::array(Type::Any))
                }
                Type::Array(e) => Type::array((*e).clone()),
                _ => Type::array(Type::Any),
            }
        }
        AstKind::Projection(lhs, rhs) => {
            let lt = annotate_inner(lhs, root);
            let base = lt.ty.clone();
            let elem = match &base {
                Type::Array(e) => (**e).clone(),
                Type::Object(m) => m.values().cloned().reduce(unify_join).unwrap_or(Type::Any),
                _ => Type::Any,
            };
            let rt = annotate_inner(rhs, &Some(elem.clone()));
            let rty = rt.ty.clone();
            node.kind = AstKind::Projection(Box::new(lt), Box::new(rt));
            Type::array(rty)
        }
        AstKind::Flatten(lhs) => {
            let lt = annotate_inner(lhs, root);
            let base = lt.ty.clone();
            node.kind = AstKind::Flatten(Box::new(lt));
            match base {
                Type::Array(e) => match *e {
                    Type::Array(inner) => Type::array(*inner),
                    other => Type::array(other),
                },
                _ => Type::array(Type::Any),
            }
        }
        AstKind::Bracket(lhs, spec) => {
            let lt = annotate_inner(lhs, root);
            let base = lt.ty.clone();
            node.kind = AstKind::Bracket(Box::new(lt), spec.clone());
            match spec {
                BracketSpec::Index(_) | BracketSpec::Slice(_) => match &base {
                    Type::Array(e) => (**e).clone(),
                    _ => Type::Any,
                },
                BracketSpec::Wildcard => match &base {
                    Type::Array(e) => Type::array((**e).clone()),
                    _ => Type::array(Type::Any),
                },
                BracketSpec::Nested(items) => {
                    for it in items {
                        annotate_inner(it, &Some(base.clone()));
                    }
                    match &base {
                        Type::Array(e) => match &**e {
                            Type::Array(inner) => (**inner).clone(),
                            other => other.clone(),
                        },
                        _ => Type::Any,
                    }
                }
                BracketSpec::List(items) => {
                    let mut tys = Vec::new();
                    for it in items {
                        let at = annotate_inner(it, &Some(base.clone()));
                        tys.push(at.ty.clone());
                    }
                    tys.into_iter()
                        .reduce(unify_join)
                        .map(Type::array)
                        .unwrap_or_else(|| Type::array(Type::Any))
                }
                BracketSpec::Filter(_) => Type::Any,
            }
        }
        AstKind::Pipe(lhs, rhs) => {
            // The right-hand side is annotated against the *left* result, which
            // is what makes `a | [0] | name` type-check like `a[0].name`.
            let lt = annotate_inner(lhs, root);
            let lshape = Some(lt.ty.clone());
            let rt = annotate_inner(rhs, &lshape);
            let rty = rt.ty.clone();
            node.kind = AstKind::Pipe(Box::new(lt), Box::new(rt));
            rty
        }
        AstKind::MultiSelectList(items) => {
            let mut tys = Vec::new();
            let mut new = Vec::with_capacity(items.len());
            for it in items {
                let t = annotate_inner(it, root);
                tys.push(t.ty.clone());
                new.push(t);
            }
            node.kind = AstKind::MultiSelectList(new);
            tys.into_iter()
                .reduce(unify_join)
                .map(Type::array)
                .unwrap_or_else(|| Type::array(Type::Any))
        }
        AstKind::MultiSelectHash(items) => {
            let mut fields = BTreeMap::new();
            let mut new = Vec::with_capacity(items.len());
            for (k, ks, v) in items {
                let t = annotate_inner(v, root);
                fields.insert(k.clone(), t.ty.clone());
                new.push((k.clone(), ks.clone(), t));
            }
            node.kind = AstKind::MultiSelectHash(new);
            Type::Object(Rc::new(fields))
        }
        AstKind::Call {
            name,
            name_span,
            builtin,
            args,
        } => {
            let mut new = Vec::with_capacity(args.len());
            let mut tys = Vec::with_capacity(args.len());
            for x in args {
                let t = annotate_inner(x, root);
                tys.push(t.ty.clone());
                new.push(t);
            }
            node.kind = AstKind::Call {
                name: name.clone(),
                name_span: *name_span,
                builtin: *builtin,
                args: new,
            };
            match builtin {
                Some(b) => builtin_ret(*b),
                None => Type::Any,
            }
        }
    };
    node
}

/// Join two shapes for a projection (multiselect / wildcard): prefer the more
/// specific one, falling back to `Any` when they are incompatible.
fn unify_join(a: Type, b: Type) -> Type {
    match (&a, &b) {
        (Type::Any, _) => b,
        (_, Type::Any) => a,
        _ => {
            if unify(&a, &b).is_ok() {
                a
            } else {
                Type::Any
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Signatures (exposed for tooling and tests)
// ---------------------------------------------------------------------------

/// A documented polymorphic signature for a built-in.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Sig {
    pub builtin: Builtin,
    pub params: Vec<&'static str>,
    pub accepts: &'static str,
    pub ret: Type,
}

/// Signature table used by `jmespath_guard::signatures` and by the docs tests.
pub fn signatures() -> Vec<Sig> {
    vec![
        Sig {
            builtin: Builtin::Length,
            params: vec!["subject"],
            accepts: "string | array | object",
            ret: Type::Number,
        },
        Sig {
            builtin: Builtin::Keys,
            params: vec!["subject"],
            accepts: "object",
            ret: Type::array(Type::String),
        },
        Sig {
            builtin: Builtin::Values,
            params: vec!["subject"],
            accepts: "object",
            ret: Type::array(Type::Any),
        },
        Sig {
            builtin: Builtin::Contains,
            params: vec!["haystack", "needle"],
            accepts: "string | array, any",
            ret: Type::Bool,
        },
        Sig {
            builtin: Builtin::Merge,
            params: vec!["subject"],
            accepts: "object",
            ret: Type::Object(Rc::new(BTreeMap::new())),
        },
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unify_is_reflexive_and_total_on_top() {
        assert!(unify(&Type::Any, &Type::Number).is_ok());
        assert!(unify(&Type::Number, &Type::Number).is_ok());
        assert!(unify(&Type::String, &Type::Number).is_err());
        assert!(unify(&Type::array(Type::Number), &Type::array(Type::Any)).is_ok());
        assert!(unify(&Type::array(Type::Number), &Type::array(Type::String)).is_err());
    }

    #[test]
    fn unify_objects_checks_only_shared_keys() {
        let a = Type::object(vec![("x", Type::Number)]);
        let b = Type::object(vec![("y", Type::String)]);
        assert!(unify(&a, &b).is_ok());
        let c = Type::object(vec![("x", Type::String)]);
        assert!(unify(&a, &c).is_err());
    }

    #[test]
    fn call_admission_matches_documented_signatures() {
        assert!(call_admits(Builtin::Length, &[], &Type::array(Type::Any)));
        assert!(!call_admits(Builtin::Length, &[], &Type::Number));
        assert!(call_admits(
            Builtin::Keys,
            &[],
            &Type::object(vec![("a", Type::Any)])
        ));
        assert!(!call_admits(Builtin::Keys, &[], &Type::array(Type::Any)));
        assert!(call_admits(
            Builtin::Merge,
            &[],
            &Type::object(vec![("a", Type::Any)])
        ));
    }

    #[test]
    fn rejections_fire_on_known_shapes() {
        assert!(ReqKind::ArrayIndex.rejects(&Type::Number));
        assert!(ReqKind::ArrayIndex.rejects(&Type::Object(BTreeMap::new().into())));
        assert!(!ReqKind::ArrayIndex.rejects(&Type::array(Type::Any)));
        assert!(ReqKind::Field("a".into()).rejects(&Type::String));
        assert!(!ReqKind::Field("a".into()).rejects(&Type::Any));
    }

    #[test]
    fn depth_limit_is_enforced() {
        let mut ast = Ast::new(AstKind::Root, Span::dummy());
        for _ in 0..20 {
            ast = Ast::new(AstKind::Field(Box::new(ast), "a".into()), Span::dummy());
        }
        let err = precheck(&ast, 0, 8).unwrap_err();
        assert_eq!(err.diagnostics[0].kind, TypeErrorKind::DepthLimit);
    }

    #[test]
    fn signature_table_is_complete() {
        assert_eq!(signatures().len(), Builtin::ALL.len());
    }

    #[test]
    fn unify_join_prefers_specific_type() {
        assert_eq!(unify_join(Type::Any, Type::Number), Type::Number);
        assert_eq!(unify_join(Type::String, Type::Number), Type::Any);
    }
}
