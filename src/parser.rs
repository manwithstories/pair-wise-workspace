//! Recursive-descent parser producing the untyped AST.
//!
//! Grammar (the supported JMESPath subset):
//!
//! ```text
//! expression   := pipe
//! pipe         := or_expr ('|' or_expr)*
//! or_expr      := chain
//! chain        := primary postfix*
//! postfix      := '.' ident | '.' '*' | '[' bracket ']'
//! bracket      := '*' | int | slice | nested | multiselect_list | filter
//! primary      := '@' | ident | quoted | '(' pipe ')' | multiselect_hash
//!                | '[' multiselect_list ']' | '&' expression
//! ```
//!
//! Every error carries a [`Span`], so a malformed query points at an exact
//! line and column instead of failing somewhere inside the evaluator.

use crate::ast::{Ast, AstKind, BracketSpec, Builtin, SliceSpec};
use crate::lexer::{tokenize, Span, Token, TokenKind};

/// Syntax error with precise location.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ParseError {
    pub msg: String,
    pub span: Span,
    /// Source line for a caret-style diagnostic.
    pub line_text: String,
    pub line_no: usize,
}

impl std::fmt::Display for ParseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        writeln!(
            f,
            "syntax error at line {}, column {}: {}",
            self.line_no, self.span.start.col, self.msg
        )?;
        writeln!(f, "  {}", self.line_text)?;
        let pad = " ".repeat(self.span.start.col.saturating_sub(1));
        writeln!(f, "  {}^", pad)
    }
}

impl std::error::Error for ParseError {}

/// Parse `src` into an untyped AST wrapped in a [`AstKind::Root`] node.
pub fn parse(src: &str) -> Result<Ast, ParseError> {
    let tokens = tokenize(src).map_err(|e| ParseError {
        msg: e.msg,
        span: e.span,
        line_text: line_of(src, e.span.start.line),
        line_no: e.span.start.line,
    })?;
    let mut p = Parser {
        toks: tokens,
        pos: 0,
        src,
        depth: 0,
        max_depth: 512,
    };
    let node = p.parse_expression()?;
    p.expect(TokenKind::Eof, "end of query")?;
    // The parsed expression *is* the query; the document root is implicit and
    // supplied by the solver as the ambient shape, so no wrapper node is added.
    Ok(node)
}

fn line_of(src: &str, line_no: usize) -> String {
    src.lines()
        .nth(line_no.saturating_sub(1))
        .unwrap_or("")
        .to_string()
}

/// Result of reading a bracket body: either a multi-select list of expressions
/// or a single index specification.
enum SpecResult {
    List(Vec<Ast>),
    Other(BracketSpec),
}

struct Parser<'a> {
    toks: Vec<Token>,
    pos: usize,
    src: &'a str,
    depth: usize,
    max_depth: usize,
}

impl<'a> Parser<'a> {
    fn peek(&self) -> &TokenKind {
        &self.toks[self.pos.min(self.toks.len() - 1)].kind
    }

    fn peek_at(&self, n: usize) -> &TokenKind {
        &self.toks[(self.pos + n).min(self.toks.len() - 1)].kind
    }

    fn span(&self) -> Span {
        self.toks[self.pos.min(self.toks.len() - 1)].span
    }

    fn bump(&mut self) -> Token {
        let t = self.toks[self.pos.min(self.toks.len() - 1)].clone();
        if self.pos < self.toks.len() - 1 {
            self.pos += 1;
        }
        t
    }

    fn err(&self, msg: impl Into<String>) -> ParseError {
        let span = self.span();
        ParseError {
            msg: msg.into(),
            span,
            line_text: line_of(self.src, span.start.line),
            line_no: span.start.line,
        }
    }

    fn err_at(&self, msg: impl Into<String>, span: Span) -> ParseError {
        ParseError {
            msg: msg.into(),
            span,
            line_text: line_of(self.src, span.start.line),
            line_no: span.start.line,
        }
    }

    fn expect(&mut self, want: TokenKind, what: &str) -> Result<Token, ParseError> {
        if *self.peek() == want {
            Ok(self.bump())
        } else {
            Err(self.err(format!(
                "expected {}, found {}",
                what,
                self.peek().describe()
            )))
        }
    }

    fn accept(&mut self, want: &TokenKind) -> bool {
        if self.peek() == want {
            self.bump();
            true
        } else {
            false
        }
    }

    // -- expression levels --------------------------------------------------

    /// pipe := chain ('|' chain)*
    fn parse_expression(&mut self) -> Result<Ast, ParseError> {
        self.depth += 1;
        if self.depth > self.max_depth {
            return Err(self.err("query nests too deeply"));
        }
        let mut lhs = self.parse_chain()?;
        loop {
            if self.accept(&TokenKind::Pipe) {
                let rhs = self.parse_chain()?;
                let span = join_span(lhs.span, rhs.span);
                lhs = Ast::new(AstKind::Pipe(Box::new(lhs), Box::new(rhs)), span);
            } else if matches!(self.peek(), TokenKind::Flatten) {
                // `a || b` is the JMESPath flatten projection: project `[*]`
                // over `a` and continue the accessor chain on each result.
                let op = self.bump();
                let rhs = self.parse_chain()?;
                let span = join_span(lhs.span, rhs.span);
                lhs = Ast::new(AstKind::Projection(Box::new(lhs), Box::new(rhs)), span);
                let _ = op;
            } else {
                break;
            }
        }
        self.depth -= 1;
        Ok(lhs)
    }

    /// chain := primary postfix*
    fn parse_chain(&mut self) -> Result<Ast, ParseError> {
        let mut node = self.parse_primary()?;
        loop {
            match self.peek().clone() {
                TokenKind::Dot => {
                    self.bump();
                    match self.peek().clone() {
                        TokenKind::Star => {
                            let s = self.bump().span;
                            let span = join_span(node.span, s);
                            node = Ast::new(AstKind::Wildcard(Box::new(node)), span);
                        }
                        TokenKind::Ident(name) => {
                            let s = self.bump().span;
                            let span = join_span(node.span, s);
                            node = Ast::new(AstKind::Field(Box::new(node), name), span);
                        }
                        TokenKind::QuotedIdent(name) => {
                            let s = self.bump().span;
                            let span = join_span(node.span, s);
                            node = Ast::new(AstKind::Field(Box::new(node), name), span);
                        }
                        other => {
                            return Err(self.err(format!(
                                "expected a field name or `*` after `.`, found {}",
                                other.describe()
                            )))
                        }
                    }
                }
                TokenKind::BracketOpen => {
                    // `a[]` is flatten; anything else inside the brackets is a
                    // real index specification.
                    if matches!(self.peek_at(1), TokenKind::BracketClose) {
                        let open = self.bump().span;
                        let close = self.expect(TokenKind::BracketClose, "`]`")?;
                        let span = join_span(node.span, close.span);
                        let _ = open;
                        node = Ast::new(AstKind::Flatten(Box::new(node)), span);
                        continue;
                    }
                    self.bump();
                    let spec = self.parse_bracket()?;
                    let close = self.expect(TokenKind::BracketClose, "`]` to close the index")?;
                    let span = join_span(node.span, close.span);
                    node = Ast::new(AstKind::Bracket(Box::new(node), spec), span);
                }
                _ => break,
            }
        }
        Ok(node)
    }

    /// primary := '@' | ident | quoted | '(' e ')' | hash | '[' list ']' | '&' e
    fn parse_primary(&mut self) -> Result<Ast, ParseError> {
        let start = self.span();
        match self.peek().clone() {
            TokenKind::At => {
                let t = self.bump();
                Ok(Ast::new(AstKind::Current, t.span))
            }
            TokenKind::Ampersand => {
                self.bump();
                let inner = self.parse_expression()?;
                let span = join_span(start, inner.span);
                Ok(Ast::new(AstKind::RefExpr(Box::new(inner)), span))
            }
            TokenKind::Ident(name) => {
                let t = self.bump();
                // A bare identifier followed by `(` is a function call.
                if matches!(self.peek(), TokenKind::ParenOpen) {
                    return self.parse_call(name, t.span);
                }
                Ok(Ast::new(AstKind::Ident(name), t.span))
            }
            // A bare integer in value position is a JSON number literal.
            TokenKind::Int(v) => {
                let t = self.bump();
                Ok(Ast::new(AstKind::NumberLiteral(v), t.span))
            }
            // A quoted token in expression position names a *field*, exactly
            // like a bare identifier — `"a"` and `a` are the same lookup.  String
            // literals in this subset use backticks, which are unambiguous.
            TokenKind::QuotedIdent(name) => {
                let t = self.bump();
                if matches!(self.peek(), TokenKind::ParenOpen) {
                    return self.parse_call(name, t.span);
                }
                Ok(Ast::new(AstKind::QuotedField(name), t.span))
            }
            TokenKind::Literal(name) => {
                let t = self.bump();
                Ok(Ast::new(AstKind::StringLiteral(name), t.span))
            }
            TokenKind::ParenOpen => {
                self.bump();
                let inner = self.parse_expression()?;
                self.expect(TokenKind::ParenClose, "`)`")?;
                Ok(inner)
            }
            TokenKind::BraceOpen => self.parse_multiselect_hash(),
            TokenKind::BracketOpen => {
                // A leading `[` starts either a multi-select list or, as in the
                // pipe spelling `people | [0] | name`, a bare index applied to
                // the current node.
                self.bump();
                let spec = self.parse_bracket_body()?;
                let close = self.expect(TokenKind::BracketClose, "`]`")?;
                let span = join_span(start, close.span);
                let node = match spec {
                    SpecResult::List(items) => Ast::new(AstKind::MultiSelectList(items), span),
                    SpecResult::Other(b) => {
                        // Implicit base: the current node.
                        let cur = Ast::new(AstKind::Current, start);
                        Ast::new(AstKind::Bracket(Box::new(cur), b), span)
                    }
                };
                Ok(node)
            }
            other => Err(self.err(format!(
                "expected an expression, found {}",
                other.describe()
            ))),
        }
    }

    fn parse_call(&mut self, name: String, name_span: Span) -> Result<Ast, ParseError> {
        self.expect(TokenKind::ParenOpen, "`(`")?;
        let mut args = Vec::new();
        if !matches!(self.peek(), TokenKind::ParenClose) {
            loop {
                args.push(self.parse_expression()?);
                if !self.accept(&TokenKind::Comma) {
                    break;
                }
                if matches!(self.peek(), TokenKind::ParenClose) {
                    return Err(self.err("trailing comma in argument list"));
                }
            }
        }
        let close = self.expect(TokenKind::ParenClose, "`)` to close the call")?;
        let span = join_span(name_span, close.span);
        Ok(Ast::new(
            AstKind::Call {
                builtin: Builtin::from_name(&name),
                name,
                name_span,
                args,
            },
            span,
        ))
    }

    /// Parse the inside of a `[...]` specifier (after the opening bracket).
    fn parse_bracket(&mut self) -> Result<BracketSpec, ParseError> {
        match self.parse_bracket_body()? {
            SpecResult::List(items) => Ok(BracketSpec::List(items)),
            SpecResult::Other(b) => Ok(b),
        }
    }

    /// Parse a bracket body.  `[a, b]` and `[0, 1]` are multi-select lists; the
    /// rest of the forms are index specifications.
    fn parse_bracket_body(&mut self) -> Result<SpecResult, ParseError> {
        // An empty `[]` is the flatten accessor, which the caller has already
        // routed here via the `a[]` spelling; at the start of a query it is an
        // error because there is no operand to flatten.
        if matches!(self.peek(), TokenKind::BracketClose) {
            return Err(self.err("empty index expression `[]`"));
        }
        // Wildcard
        if matches!(self.peek(), TokenKind::Star) {
            self.bump();
            return Ok(SpecResult::Other(BracketSpec::Wildcard));
        }
        // Multi-select list `[a, b]` — but `[0, 1]` is an index list too; both
        // are lists of expressions, so one code path handles both.
        if matches!(self.peek(), TokenKind::BracketOpen) {
            self.bump();
            let items = self.parse_multiselect_list(TokenKind::BracketClose)?;
            self.expect(TokenKind::BracketClose, "`]`")?;
            return Ok(SpecResult::Other(BracketSpec::Nested(items)));
        }
        if matches!(
            self.peek(),
            TokenKind::Ident(_) | TokenKind::QuotedIdent(_) | TokenKind::At
        ) {
            let items = self.parse_multiselect_list(TokenKind::BracketClose)?;
            return Ok(SpecResult::List(items));
        }
        if matches!(self.peek(), TokenKind::Filter) {
            let s = self.bump().span;
            // Consume the condition so the enclosing bracket still closes, but
            // stop at the first `]` that is not nested inside another bracket so
            // that operators such as `==` do not make us overshoot.
            let mut depth = 0usize;
            loop {
                match self.peek() {
                    TokenKind::BracketOpen => {
                        depth += 1;
                        self.bump();
                    }
                    TokenKind::BracketClose => {
                        if depth == 0 {
                            break;
                        }
                        depth -= 1;
                        self.bump();
                    }
                    TokenKind::Eof => break,
                    _ => {
                        self.bump();
                    }
                }
            }
            return Ok(SpecResult::Other(BracketSpec::Filter(s)));
        }
        // Slice or index.
        Ok(SpecResult::Other(self.parse_index_or_slice()?))
    }

    fn parse_index_or_slice(&mut self) -> Result<BracketSpec, ParseError> {
        let start = match self.peek().clone() {
            TokenKind::Int(v) => {
                let t = self.bump();
                // A colon turns it into a slice.
                if matches!(self.peek(), TokenKind::Colon) {
                    return self.finish_slice(Some(v), false);
                }
                if matches!(self.peek(), TokenKind::Colon | TokenKind::BracketClose) {
                    return Ok(BracketSpec::Index(v));
                }
                let _ = t;
                BracketSpec::Index(v)
            }
            TokenKind::Colon => return self.finish_slice(None, false),
            other => {
                return Err(self.err(format!(
                    "expected an index, a slice or `*`, found {}",
                    other.describe()
                )))
            }
        };
        Ok(start)
    }

    /// Continue after having seen `[` and possibly the `start:` part.
    fn finish_slice(
        &mut self,
        start: Option<i64>,
        seen_colon: bool,
    ) -> Result<BracketSpec, ParseError> {
        // We are positioned at a colon (or at `]` if `seen_colon` is false and
        // start was Some — handled by the caller).
        self.expect(TokenKind::Colon, "`:`")?;
        let mut stop = None;
        let mut has_second_colon = seen_colon;
        if let TokenKind::Int(v) = self.peek().clone() {
            self.bump();
            stop = Some(v);
        }
        if self.accept(&TokenKind::Colon) {
            has_second_colon = true;
            if let TokenKind::Int(v) = self.peek().clone() {
                self.bump();
                return Ok(BracketSpec::Slice(SliceSpec {
                    start,
                    stop,
                    step: Some(v),
                    has_second_colon,
                }));
            }
            return Ok(BracketSpec::Slice(SliceSpec {
                start,
                stop,
                step: None,
                has_second_colon,
            }));
        }
        Ok(BracketSpec::Slice(SliceSpec {
            start,
            stop,
            step: None,
            has_second_colon,
        }))
    }

    /// Parse `expr, expr, ...` until `close`.
    fn parse_multiselect_list(&mut self, close: TokenKind) -> Result<Vec<Ast>, ParseError> {
        let mut items = Vec::new();
        loop {
            if self.peek() == &close {
                break;
            }
            items.push(self.parse_expression()?);
            if !self.accept(&TokenKind::Comma) {
                break;
            }
        }
        if items.is_empty() {
            return Err(self.err("empty multi-select list"));
        }
        Ok(items)
    }

    fn parse_multiselect_hash(&mut self) -> Result<Ast, ParseError> {
        let open = self.expect(TokenKind::BraceOpen, "`{`")?;
        let mut items: Vec<(String, String, Ast)> = Vec::new();
        let mut seen: Vec<String> = Vec::new();
        loop {
            if matches!(self.peek(), TokenKind::BraceClose) {
                break;
            }
            let (key, key_span) = match self.peek().clone() {
                TokenKind::QuotedIdent(k) => {
                    let t = self.bump();
                    (k, t.span)
                }
                TokenKind::Ident(k) => {
                    let t = self.bump();
                    (k, t.span)
                }
                TokenKind::Literal(k) => {
                    let t = self.bump();
                    (k, t.span)
                }
                other => {
                    return Err(self.err(format!(
                        "expected a key name in the multi-select hash, found {}",
                        other.describe()
                    )))
                }
            };
            self.expect(TokenKind::Colon, "`:` after the key")?;
            // Hash values are accessor chains: `{a: x | y}` must not swallow the
            // `|` as a pipe and then complain about the following `,`.
            let value = self.parse_chain()?;
            if seen.contains(&key) {
                return Err(self.err_at(
                    format!("duplicate key `{}` in multi-select hash", key),
                    key_span,
                ));
            }
            seen.push(key.clone());
            let key_src = key_src(&key);
            items.push((key, key_src, value));
            // Entries are comma separated; the closing `}` ends the loop.
            if !self.accept(&TokenKind::Comma) {
                break;
            }
        }
        let close = self.expect(TokenKind::BraceClose, "`}` to close the multi-select hash")?;
        if items.is_empty() {
            return Err(self.err_at("empty multi-select hash", close.span));
        }
        let span = join_span(open.span, close.span);
        Ok(Ast::new(AstKind::MultiSelectHash(items), span))
    }
}

fn key_src(key: &str) -> String {
    key.to_string()
}

fn join_span(a: Span, b: Span) -> Span {
    Span {
        start: a.start,
        end: b.end,
        start_byte: a.start_byte,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ast::canonical;

    fn ok(src: &str) -> Ast {
        parse(src).unwrap_or_else(|e| panic!("expected `{src}` to parse, got:\n{e}"))
    }

    #[test]
    fn simple_field_chain() {
        assert_eq!(canonical(&ok("a.b.c")), "a.b.c");
    }

    #[test]
    fn index_and_field() {
        assert_eq!(canonical(&ok("people[0].name")), "people[0].name");
    }

    #[test]
    fn pipe_equivalent_spellings_parse_differently() {
        let a = ok("people[0].name");
        let b = ok("people | [0] | name");
        assert_ne!(canonical(&a), canonical(&b));
    }

    #[test]
    fn wildcard_forms() {
        assert_eq!(canonical(&ok("people[*].name")), "people[*].name");
        assert_eq!(canonical(&ok("people.*.name")), "people[*].name");
    }

    #[test]
    fn slices() {
        assert_eq!(canonical(&ok("a[1:3]")), "a[1:3]");
        assert_eq!(canonical(&ok("a[:2]")), "a[:2]");
        assert_eq!(canonical(&ok("a[::2]")), "a[::2]");
        assert_eq!(canonical(&ok("a[1:]")), "a[1:]");
    }

    #[test]
    fn multi_select_list_and_hash() {
        assert_eq!(canonical(&ok("[a, b]")), "[a,b]");
        assert_eq!(canonical(&ok("{x: a, y: b}")), r#"{"x":a,"y":b}"#);
    }

    #[test]
    fn calls() {
        assert_eq!(canonical(&ok("length(items)")), "length(items)");
        assert_eq!(canonical(&ok("items | length(@)")), "items|length(@)");
        assert_eq!(canonical(&ok("contains(tags, `x`)")), "contains(tags,'x')");
    }

    #[test]
    fn syntax_errors_carry_line_and_column() {
        let e = parse("a.\n  .b").unwrap_err();
        assert_eq!(e.line_no, 2);
        assert!(e.msg.contains("expected a field name"));
        let e2 = parse("a[0").unwrap_err();
        assert!(e2.msg.contains("expected `]`"));
        // Filters are syntactically accepted and rejected later by the type
        // checker, so they must not surface as syntax errors here.
        assert!(parse("a[?x == 'y']").is_ok());
    }

    #[test]
    fn duplicate_hash_key_is_rejected() {
        let e = parse("{a: x, a: y}").unwrap_err();
        assert!(e.msg.contains("duplicate key"));
    }

    #[test]
    fn trailing_comma_in_call_is_rejected() {
        let e = parse("length(a,)").unwrap_err();
        assert!(e.msg.contains("trailing comma"));
    }

    #[test]
    fn empty_list_is_rejected() {
        // `[]` with no operand has nothing to flatten and nothing to project.
        let e = parse("[]").unwrap_err();
        assert!(e.msg.contains("empty index expression"));
    }

    #[test]
    fn flatten_forms_parse_differently() {
        // `a[]` is the flatten accessor; `a || b` is the flatten *projection*
        // `a[*]` with `b` applied to each element.  They are different queries.
        assert_eq!(canonical(&ok("a[]")), "a[]");
        assert_eq!(canonical(&ok("a || b")), "a||b");
    }

    #[test]
    fn unknown_function_parses_but_names_it() {
        let ast = ok("sort_by(a, &b)");
        match &ast.kind {
            AstKind::Call { builtin, name, .. } => {
                assert_eq!(*builtin, None);
                assert_eq!(name, "sort_by");
            }
            other => panic!("unexpected root {other:?}"),
        }
    }
}
