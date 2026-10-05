//! Hand written lexer for the JMESPath subset.
//!
//! The lexer is total: it never panics and never loops forever.  Every token
//! carries an absolute byte span plus 1-based line/column information so that
//! the parser and the type checker can report human readable diagnostics.

use std::fmt;

/// Source position, 1-based line, 1-based column (in UTF-8 characters).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Default)]
pub struct Pos {
    pub line: usize,
    pub col: usize,
}

impl Pos {
    pub fn new(line: usize, col: usize) -> Self {
        Pos { line, col }
    }
}

impl fmt::Display for Pos {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "line {}, column {}", self.line, self.col)
    }
}

/// Lexical error with precise location information.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LexError {
    pub msg: String,
    pub span: Span,
}

impl fmt::Display for LexError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} at {}", self.msg, self.span.start)
    }
}

impl std::error::Error for LexError {}

/// Byte range inside the query source.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Default)]
pub struct Span {
    pub start: Pos,
    pub end: Pos,
    /// Byte offset of `start` in the source string.
    pub start_byte: usize,
}

impl fmt::Display for Span {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} .. {}", self.start, self.end)
    }
}

impl Span {
    pub fn dummy() -> Self {
        Span {
            start: Pos::new(1, 1),
            end: Pos::new(1, 1),
            start_byte: 0,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TokenKind {
    /// Unquoted identifier / field name.
    Ident(String),
    /// `"quoted identifier"`.
    QuotedIdent(String),
    /// Backtick quoted `` `literal` `` identifier.
    Literal(String),
    /// Integer literal (used by index expressions).
    Int(i64),
    /// `.` field separator.
    Dot,
    /// `[` `]`
    BracketOpen,
    BracketClose,
    /// `*` wildcard.
    Star,
    /// `,` list separator.
    Comma,
    /// `{` `}`
    BraceOpen,
    BraceClose,
    /// `:` key/value separator inside multi-select hashes.
    Colon,
    /// `|` pipe.
    Pipe,
    /// `||` flatten projection operator.
    Flatten,
    /// `@` current node.
    At,
    /// `?` filter (recognised so we can emit a dedicated diagnostic).
    Filter,
    /// Comparison operators inside a filter condition.  These are recognised
    /// only so that a filter parses; the type checker then rejects filters as
    /// outside this subset.
    Compare,
    /// `!`, `&&`, `||`-style logical operators recognised for the same reason.
    Logical,
    /// `(` `)`
    ParenOpen,
    ParenClose,
    /// `&` reference to the root of the current expression.
    Ampersand,
    /// `&&`, `||`-like logical operators are not supported; reserved.
    Eof,
}

impl TokenKind {
    /// Human readable description used in diagnostics.
    pub fn describe(&self) -> String {
        match self {
            TokenKind::Ident(s) => format!("identifier `{}`", s),
            TokenKind::QuotedIdent(s) => format!("quoted identifier \"{}\"", s),
            TokenKind::Literal(s) => format!("literal `{}`", s),
            TokenKind::Int(i) => format!("integer {}", i),
            TokenKind::Dot => "`.`".into(),
            TokenKind::BracketOpen => "`[`".into(),
            TokenKind::BracketClose => "`]`".into(),
            TokenKind::Star => "`*`".into(),
            TokenKind::Comma => "`,`".into(),
            TokenKind::BraceOpen => "`{`".into(),
            TokenKind::BraceClose => "`}`".into(),
            TokenKind::Colon => "`:`".into(),
            TokenKind::Pipe => "`|`".into(),
            TokenKind::Flatten => "`||`".into(),
            TokenKind::At => "`@`".into(),
            TokenKind::Filter => "`?`".into(),
            TokenKind::Compare => "a comparison operator".into(),
            TokenKind::Logical => "a logical operator".into(),
            TokenKind::ParenOpen => "`(`".into(),
            TokenKind::ParenClose => "`)`".into(),
            TokenKind::Ampersand => "`&`".into(),
            TokenKind::Eof => "end of query".into(),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Token {
    pub kind: TokenKind,
    pub span: Span,
}

/// The lexer state machine.
struct Lexer {
    chars: Vec<(usize, char)>,
    idx: usize,
    line: usize,
    col: usize,
    byte: usize,
}

impl Lexer {
    fn peek(&self) -> Option<char> {
        self.chars.get(self.idx).map(|(_, c)| *c)
    }
    fn peek_at(&self, n: usize) -> Option<char> {
        self.chars.get(self.idx + n).map(|(_, c)| *c)
    }
    fn bump(&mut self) -> Option<char> {
        let (off, c) = *self.chars.get(self.idx)?;
        self.idx += 1;
        if c == '\n' {
            self.line += 1;
            self.col = 1;
        } else {
            self.col += 1;
        }
        self.byte = off + c.len_utf8();
        Some(c)
    }
    fn here(&self) -> Span {
        Span {
            start: Pos::new(self.line, self.col),
            end: Pos::new(self.line, self.col),
            start_byte: self.byte,
        }
    }
}

/// Tokenize `src`, returning all tokens including a terminating `Eof`.
///
/// Recognised identifiers use the JMESPath rule: `[A-Za-z_][A-Za-z0-9_]*`.
/// Keywords (`true`, `false`, `null`) are lexed as plain identifiers and are
/// interpreted by the parser / evaluator.
pub fn tokenize(src: &str) -> Result<Vec<Token>, LexError> {
    let chars: Vec<(usize, char)> = src.char_indices().collect();
    let mut lx = Lexer {
        chars,
        idx: 0,
        line: 1,
        col: 1,
        byte: 0,
    };
    let mut out: Vec<Token> = Vec::new();
    loop {
        // Skip insignificant whitespace.
        while matches!(lx.peek(), Some(' ') | Some('\t') | Some('\n') | Some('\r')) {
            lx.bump();
        }
        let start = lx.here();
        let Some(c) = lx.peek() else {
            out.push(Token {
                kind: TokenKind::Eof,
                span: start,
            });
            return Ok(out);
        };
        let kind = match c {
            '.' => {
                lx.bump();
                TokenKind::Dot
            }
            '[' => {
                lx.bump();
                TokenKind::BracketOpen
            }
            ']' => {
                lx.bump();
                TokenKind::BracketClose
            }
            '{' => {
                lx.bump();
                TokenKind::BraceOpen
            }
            '}' => {
                lx.bump();
                TokenKind::BraceClose
            }
            '(' => {
                lx.bump();
                TokenKind::ParenOpen
            }
            ')' => {
                lx.bump();
                TokenKind::ParenClose
            }
            ',' => {
                lx.bump();
                TokenKind::Comma
            }
            ':' => {
                lx.bump();
                TokenKind::Colon
            }
            '@' => {
                lx.bump();
                TokenKind::At
            }
            '*' => {
                lx.bump();
                TokenKind::Star
            }
            '?' => {
                lx.bump();
                TokenKind::Filter
            }
            '>' | '<' => {
                lx.bump();
                if lx.peek() == Some('=') {
                    lx.bump();
                }
                TokenKind::Compare
            }
            '=' | '!' => {
                lx.bump();
                if lx.peek() == Some('=') {
                    lx.bump();
                }
                TokenKind::Compare
            }
            // `&x` is a reference expression; `&&`, `^`, `%` are logical
            // operators that only appear inside filters.
            '&' => {
                lx.bump();
                if lx.peek() == Some('&') {
                    lx.bump();
                    TokenKind::Logical
                } else {
                    TokenKind::Ampersand
                }
            }
            '^' | '%' => {
                lx.bump();
                TokenKind::Logical
            }
            '|' => {
                lx.bump();
                if lx.peek() == Some('|') {
                    lx.bump();
                    TokenKind::Flatten
                } else {
                    TokenKind::Pipe
                }
            }
            '"' => {
                lx.bump();
                let s = lex_quoted(&mut lx, '"', &start)?;
                TokenKind::QuotedIdent(s)
            }
            '\'' => {
                lx.bump();
                let s = lex_quoted(&mut lx, '\'', &start)?;
                TokenKind::QuotedIdent(s)
            }
            '`' => {
                lx.bump();
                let s = lex_quoted(&mut lx, '`', &start)?;
                TokenKind::Literal(s)
            }
            '-' if matches!(lx.peek_at(1), Some(d) if d.is_ascii_digit()) => {
                lx.bump(); // '-'
                let mut s = String::from("-");
                while matches!(lx.peek(), Some(d) if d.is_ascii_digit()) {
                    s.push(lx.bump().unwrap());
                }
                let v: i64 = s.parse().map_err(|_| LexError {
                    msg: format!("integer literal `{}` is out of range for 64-bit", s),
                    span: finish(&lx, start),
                })?;
                TokenKind::Int(v)
            }
            c if c.is_ascii_digit() => {
                let mut s = String::new();
                while matches!(lx.peek(), Some(d) if d.is_ascii_digit()) {
                    s.push(lx.bump().unwrap());
                }
                // A leading-zero or directly-followed identifier char is a typo.
                if let Some(d) = lx.peek() {
                    if is_ident_start(d) {
                        return Err(LexError {
                            msg: format!("invalid number literal `{}{}`", s, d),
                            span: finish(&lx, start),
                        });
                    }
                }
                let v: i64 = s.parse().map_err(|_| LexError {
                    msg: format!("integer literal `{}` is out of range for 64-bit", s),
                    span: finish(&lx, start),
                })?;
                TokenKind::Int(v)
            }
            // A non-ASCII character is only legal inside a quoted identifier;
            // bare identifiers are ASCII by the JMESPath grammar.
            c if !c.is_ascii() => {
                return Err(LexError {
                    msg: format!(
                        "unexpected character `{}`; quote it as \"{}\" to use it as a field name",
                        c, c
                    ),
                    span: start,
                })
            }
            c if is_ident_start(c) => {
                let mut s = String::new();
                while matches!(lx.peek(), Some(d) if is_ident_continue(d)) {
                    s.push(lx.bump().unwrap());
                }
                TokenKind::Ident(s)
            }
            other => {
                return Err(LexError {
                    msg: format!("unexpected character `{}`", other),
                    span: start,
                })
            }
        };
        out.push(Token {
            kind,
            span: finish(&lx, start),
        });
    }
}

fn finish(lx: &Lexer, start: Span) -> Span {
    Span {
        start: start.start,
        end: Pos::new(lx.line, lx.col),
        start_byte: start.start_byte,
    }
}

fn is_ident_start(c: char) -> bool {
    c.is_ascii_alphabetic() || c == '_'
}

fn is_ident_continue(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '_'
}

/// Lex a quoted section; the opening quote has already been consumed.
/// Handles `\"` escapes and rejects raw control characters and newlines.
fn lex_quoted(lx: &mut Lexer, quote: char, start: &Span) -> Result<String, LexError> {
    let mut out = String::new();
    loop {
        match lx.peek() {
            None => {
                return Err(LexError {
                    msg: format!(
                        "unterminated {} quoted string, expected closing `{}`",
                        quote_name(quote),
                        quote
                    ),
                    span: finish(lx, *start),
                })
            }
            Some(c) if c == quote => {
                lx.bump();
                return Ok(out);
            }
            Some('\\') => {
                lx.bump();
                let Some(esc) = lx.bump() else {
                    return Err(LexError {
                        msg: "unterminated escape sequence".into(),
                        span: finish(lx, *start),
                    });
                };
                let ch = match esc {
                    'n' => '\n',
                    't' => '\t',
                    'r' => '\r',
                    '\\' => '\\',
                    '"' => '"',
                    '\'' => '\'',
                    '`' => '`',
                    '0' => '\0',
                    other => {
                        return Err(LexError {
                            msg: format!("unsupported escape sequence `\\{}`", other),
                            span: finish(lx, *start),
                        })
                    }
                };
                out.push(ch);
            }
            Some('\n') => {
                return Err(LexError {
                    msg: "unterminated string literal: newline inside quotes".into(),
                    span: finish(lx, *start),
                })
            }
            Some(c) => {
                lx.bump();
                out.push(c);
            }
        }
    }
}

fn quote_name(c: char) -> &'static str {
    match c {
        '"' => "double",
        '\'' => "single",
        '`' => "backtick",
        _ => "",
    }
}

/// Tokenize and return the token vector, tolerating lexical errors by
/// returning whatever was scanned plus the error.  Used by tooling/tests.
pub fn tokenize_lenient(src: &str) -> (Vec<Token>, Option<LexError>) {
    match tokenize(src) {
        Ok(t) => (t, None),
        Err(e) => (Vec::new(), Some(e)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn kinds(src: &str) -> Vec<TokenKind> {
        tokenize(src).unwrap().into_iter().map(|t| t.kind).collect()
    }

    #[test]
    fn basic_tokens() {
        assert_eq!(
            kinds("people[0].name"),
            vec![
                TokenKind::Ident("people".into()),
                TokenKind::BracketOpen,
                TokenKind::Int(0),
                TokenKind::BracketClose,
                TokenKind::Dot,
                TokenKind::Ident("name".into()),
                TokenKind::Eof,
            ]
        );
    }

    #[test]
    fn line_and_column_are_reported() {
        let src = "people\n  .[?bad]";
        let toks = tokenize(src).unwrap();
        let star = toks.iter().find(|t| t.kind == TokenKind::Filter).unwrap();
        // Line 2 is "  .[?bad]": `?` is the fifth character (1-based) and
        // `start_byte` is the offset of the byte *before* it (the `[`).
        assert_eq!(star.span.start, Pos::new(2, 5));
        assert_eq!(star.span.start_byte, 11);
    }

    #[test]
    fn flatten_is_distinguished_from_pipe() {
        assert_eq!(
            kinds("a||b | c"),
            vec![
                TokenKind::Ident("a".into()),
                TokenKind::Flatten,
                TokenKind::Ident("b".into()),
                TokenKind::Pipe,
                TokenKind::Ident("c".into()),
                TokenKind::Eof,
            ]
        );
    }

    #[test]
    fn rejects_illegal_characters() {
        let err = tokenize("a # 1").unwrap_err();
        assert!(err.msg.contains('#'));
        assert_eq!(err.span.start, Pos::new(1, 3));
        // Comparison operators are recognised so that filters parse and are
        // then rejected by the type checker rather than the lexer.
        assert!(tokenize("a == 1").is_ok());
        assert!(tokenize("a >= `1`").is_ok());
    }

    #[test]
    fn rejects_unterminated_string() {
        let err = tokenize("foo.\"bar").unwrap_err();
        assert!(err.msg.contains("unterminated"));
    }

    #[test]
    fn rejects_newline_in_string() {
        let err = tokenize("\"a\nb\"").unwrap_err();
        assert!(err.msg.contains("newline"));
    }

    #[test]
    fn numbers_followed_by_identifiers_are_errors() {
        assert!(tokenize("a[1x]").is_err());
        assert!(tokenize("a[1]").is_ok());
    }

    #[test]
    fn escapes_are_decoded() {
        assert_eq!(
            kinds(r#""a\"b""#),
            vec![TokenKind::QuotedIdent("a\"b".into()), TokenKind::Eof]
        );
    }

    #[test]
    fn lenient_returns_none_error_for_valid_input() {
        let (_, err) = tokenize_lenient("a.b");
        assert!(err.is_none());
    }
}
