// Package expr compiles the small expression and template language used by flow
// definitions.
//
// The language is deliberately smaller than Go templates: there are no method
// calls, no arithmetic and no user-defined functions, so a flow definition can
// only read the context and compare values. A path is resolved against a schema at
// compile time; an unknown path is an error instead of an empty string. The same
// compiler and evaluator are used by the validator, the engine and the parity
// harness, which is what keeps "validated" and "executable" the same statement.
package expr

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Type is the static type of an expression. Any is the type of a dynamic value
// (the raw event payload, manual parameters); it is compatible with every other
// type and is checked again at evaluation time.
type Type int

const (
	Any Type = iota
	String
	Number
	Bool
	Object
	Array
	Null
)

func (t Type) String() string {
	switch t {
	case String:
		return "string"
	case Number:
		return "number"
	case Bool:
		return "bool"
	case Object:
		return "object"
	case Array:
		return "array"
	case Null:
		return "null"
	default:
		return "any"
	}
}

// Resolver answers the type of a context path. A path that is not known must be
// reported as false so compilation can refuse it.
type Resolver interface {
	Resolve(path []string) (Type, bool)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(path []string) (Type, bool)

func (f ResolverFunc) Resolve(path []string) (Type, bool) { return f(path) }

// Program is a compiled expression.
type Program struct {
	source string
	typ    Type
	root   *node
}

// Type reports the static type of the expression.
func (p *Program) Type() Type { return p.typ }

// Source returns the expression text, for error messages.
func (p *Program) Source() string { return p.source }

// Compile parses and type-checks source against resolver.
func Compile(source string, resolver Resolver) (*Program, error) {
	parser := &parser{lex: &lexer{src: source}}
	if err := parser.advance(); err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(source), err)
	}
	root, err := parser.parseExpr()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(source), err)
	}
	if parser.tok.kind != tokenEOF {
		return nil, fmt.Errorf("%s: unexpected %q", strings.TrimSpace(source), parser.tok.text)
	}
	typ, err := typeCheck(root, resolver)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(source), err)
	}
	return &Program{source: source, typ: typ, root: root}, nil
}

// Eval evaluates the expression against ctx. A missing path is an error; it is
// never treated as a zero value.
func (p *Program) Eval(ctx map[string]any) (any, error) {
	value, err := eval(p.root, ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(p.source), err)
	}
	return value, nil
}

// Bool evaluates the expression and requires a boolean result.
func (p *Program) Bool(ctx map[string]any) (bool, error) {
	value, err := p.Eval(ctx)
	if err != nil {
		return false, err
	}
	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s: result is %T, not bool", strings.TrimSpace(p.source), value)
	}
	return result, nil
}

// Template is a string with zero or more {{ expression }} slots.
type Template struct {
	source string
	parts  []templatePart
}

type templatePart struct {
	literal string
	program *Program
}

// CompileTemplate parses a string and compiles every expression it contains.
func CompileTemplate(source string, resolver Resolver) (*Template, error) {
	template := &Template{source: source}
	rest := source
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			if rest != "" {
				template.parts = append(template.parts, templatePart{literal: rest})
			}
			return template, nil
		}
		if start > 0 {
			template.parts = append(template.parts, templatePart{literal: rest[:start]})
		}
		rest = rest[start+2:]
		end := strings.Index(rest, "}}")
		if end < 0 {
			return nil, fmt.Errorf("template: unclosed expression")
		}
		expression := strings.TrimSpace(rest[:end])
		if expression == "" {
			return nil, fmt.Errorf("template: empty expression")
		}
		program, err := Compile(expression, resolver)
		if err != nil {
			return nil, fmt.Errorf("template: %w", err)
		}
		if program.typ == Object || program.typ == Array {
			return nil, fmt.Errorf("template: expression %q renders an %s", expression, program.typ)
		}
		template.parts = append(template.parts, templatePart{program: program})
		rest = rest[end+2:]
	}
}

// Render evaluates every expression and joins the parts. A template can only
// render a scalar; an object, an array or null is an error rather than a silently
// empty string.
func (t *Template) Render(ctx map[string]any) (string, error) {
	var builder strings.Builder
	for _, part := range t.parts {
		if part.program == nil {
			builder.WriteString(part.literal)
			continue
		}
		value, err := part.program.Eval(ctx)
		if err != nil {
			return "", err
		}
		text, err := renderScalar(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", strings.TrimSpace(t.source), err)
		}
		builder.WriteString(text)
	}
	return builder.String(), nil
}

// Source returns the template text.
func (t *Template) Source() string { return t.source }

func renderScalar(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case nil:
		return "", fmt.Errorf("expression evaluated to null")
	default:
		return "", fmt.Errorf("expression rendered a %T, not a scalar", value)
	}
}

// ---- AST ----

type nodeKind int

const (
	kindLiteral nodeKind = iota
	kindPath
	kindUnary
	kindBinary
	kindCall
)

type node struct {
	kind  nodeKind
	lit   any
	path  []string
	op    string
	left  *node
	right *node
	args  []*node
	re    *regexp.Regexp
}

// ---- lexer ----

type tokenKind int

const (
	tokenEOF tokenKind = iota
	tokenIdent
	tokenString
	tokenNumber
	tokenOperator
)

type token struct {
	kind   tokenKind
	text   string
	number float64
	pos    int
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) {
		r, size := utf8.DecodeRuneInString(l.src[l.pos:])
		if !unicode.IsSpace(r) {
			break
		}
		l.pos += size
	}
	if l.pos >= len(l.src) {
		return token{kind: tokenEOF, pos: l.pos}, nil
	}
	start := l.pos
	r, size := utf8.DecodeRuneInString(l.src[l.pos:])
	switch {
	case unicode.IsLetter(r) || r == '_':
		l.pos += size
		for l.pos < len(l.src) {
			r, size = utf8.DecodeRuneInString(l.src[l.pos:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
				break
			}
			l.pos += size
		}
		return token{kind: tokenIdent, text: l.src[start:l.pos], pos: start}, nil
	case unicode.IsDigit(r):
		l.pos += size
		for l.pos < len(l.src) {
			r, size = utf8.DecodeRuneInString(l.src[l.pos:])
			if !unicode.IsDigit(r) && r != '.' {
				break
			}
			l.pos += size
		}
		number, err := strconv.ParseFloat(l.src[start:l.pos], 64)
		if err != nil {
			return token{}, fmt.Errorf("at %d: bad number %q", start, l.src[start:l.pos])
		}
		return token{kind: tokenNumber, text: l.src[start:l.pos], number: number, pos: start}, nil
	case r == '\'' || r == '"':
		quote := r
		l.pos += size
		var builder strings.Builder
		for l.pos < len(l.src) {
			r, size = utf8.DecodeRuneInString(l.src[l.pos:])
			if r == quote {
				l.pos += size
				return token{kind: tokenString, text: builder.String(), pos: start}, nil
			}
			if r == '\\' {
				l.pos += size
				if l.pos >= len(l.src) {
					return token{}, fmt.Errorf("at %d: unterminated string", start)
				}
				escaped, escapedSize := utf8.DecodeRuneInString(l.src[l.pos:])
				switch escaped {
				case 'n':
					builder.WriteByte('\n')
				case 't':
					builder.WriteByte('\t')
				case '\\':
					builder.WriteByte('\\')
				case '\'':
					builder.WriteByte('\'')
				case '"':
					builder.WriteByte('"')
				default:
					builder.WriteRune(escaped)
				}
				l.pos += escapedSize
				continue
			}
			builder.WriteRune(r)
			l.pos += size
		}
		return token{}, fmt.Errorf("at %d: unterminated string", start)
	case strings.HasPrefix(l.src[l.pos:], "=="),
		strings.HasPrefix(l.src[l.pos:], "!="),
		strings.HasPrefix(l.src[l.pos:], "<="),
		strings.HasPrefix(l.src[l.pos:], ">="),
		strings.HasPrefix(l.src[l.pos:], "&&"),
		strings.HasPrefix(l.src[l.pos:], "||"):
		op := l.src[l.pos : l.pos+2]
		l.pos += 2
		return token{kind: tokenOperator, text: op, pos: start}, nil
	case strings.ContainsRune("<>!(),.", r):
		l.pos += size
		return token{kind: tokenOperator, text: string(r), pos: start}, nil
	default:
		return token{}, fmt.Errorf("at %d: unexpected character %q", start, r)
	}
}

// ---- parser ----

type parser struct {
	lex *lexer
	tok token
}

func (p *parser) advance() error {
	tok, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = tok
	return nil
}

func (p *parser) parseExpr() (*node, error) {
	return p.parseOr()
}

func (p *parser) parseOr() (*node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.tok.kind == tokenOperator && p.tok.text == "||" {
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &node{kind: kindBinary, op: "||", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (*node, error) {
	left, err := p.parseCompare()
	if err != nil {
		return nil, err
	}
	for p.tok.kind == tokenOperator && p.tok.text == "&&" {
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.parseCompare()
		if err != nil {
			return nil, err
		}
		left = &node{kind: kindBinary, op: "&&", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseCompare() (*node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	operator := ""
	switch {
	case p.tok.kind == tokenOperator && isComparison(p.tok.text):
		operator = p.tok.text
	case p.tok.kind == tokenIdent && (p.tok.text == "in" || p.tok.text == "matches"):
		operator = p.tok.text
	default:
		return left, nil
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	right, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	return &node{kind: kindBinary, op: operator, left: left, right: right}, nil
}

func isComparison(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	default:
		return false
	}
}

func (p *parser) parseUnary() (*node, error) {
	if p.tok.kind == tokenOperator && p.tok.text == "!" {
		if err := p.advance(); err != nil {
			return nil, err
		}
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &node{kind: kindUnary, op: "!", left: operand}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (*node, error) {
	switch p.tok.kind {
	case tokenString:
		value := p.tok.text
		if err := p.advance(); err != nil {
			return nil, err
		}
		return &node{kind: kindLiteral, lit: value}, nil
	case tokenNumber:
		value := p.tok.number
		if err := p.advance(); err != nil {
			return nil, err
		}
		return &node{kind: kindLiteral, lit: value}, nil
	case tokenIdent:
		name := p.tok.text
		if err := p.advance(); err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return &node{kind: kindLiteral, lit: true}, nil
		case "false":
			return &node{kind: kindLiteral, lit: false}, nil
		case "null":
			return &node{kind: kindLiteral, lit: nil}, nil
		}
		if p.tok.kind == tokenOperator && p.tok.text == "(" {
			return p.parseCall(name)
		}
		path := []string{name}
		for p.tok.kind == tokenOperator && p.tok.text == "." {
			if err := p.advance(); err != nil {
				return nil, err
			}
			if p.tok.kind != tokenIdent {
				return nil, fmt.Errorf("at %d: expected a path segment", p.tok.pos)
			}
			path = append(path, p.tok.text)
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		return &node{kind: kindPath, path: path}, nil
	case tokenOperator:
		if p.tok.text == "(" {
			if err := p.advance(); err != nil {
				return nil, err
			}
			inside, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if p.tok.kind != tokenOperator || p.tok.text != ")" {
				return nil, fmt.Errorf("at %d: expected )", p.tok.pos)
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
			return inside, nil
		}
	}
	return nil, fmt.Errorf("at %d: expected a value", p.tok.pos)
}

func (p *parser) parseCall(name string) (*node, error) {
	if err := p.advance(); err != nil { // consume (
		return nil, err
	}
	call := &node{kind: kindCall, op: name}
	if p.tok.kind == tokenOperator && p.tok.text == ")" {
		if err := p.advance(); err != nil {
			return nil, err
		}
		return call, nil
	}
	for {
		argument, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		call.args = append(call.args, argument)
		if p.tok.kind == tokenOperator && p.tok.text == "," {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		if p.tok.kind == tokenOperator && p.tok.text == ")" {
			if err := p.advance(); err != nil {
				return nil, err
			}
			return call, nil
		}
		return nil, fmt.Errorf("at %d: expected , or )", p.tok.pos)
	}
}

// ---- type checking ----

func typeCheck(n *node, resolver Resolver) (Type, error) {
	switch n.kind {
	case kindLiteral:
		switch n.lit.(type) {
		case string:
			return String, nil
		case float64:
			return Number, nil
		case bool:
			return Bool, nil
		default:
			return Null, nil
		}
	case kindPath:
		typ, ok := resolver.Resolve(n.path)
		if !ok {
			return Any, fmt.Errorf("unknown path %q", strings.Join(n.path, "."))
		}
		return typ, nil
	case kindUnary:
		operand, err := typeCheck(n.left, resolver)
		if err != nil {
			return Any, err
		}
		if operand != Bool && operand != Any {
			return Any, fmt.Errorf("! needs a bool, got %s", operand)
		}
		return Bool, nil
	case kindBinary:
		left, err := typeCheck(n.left, resolver)
		if err != nil {
			return Any, err
		}
		right, err := typeCheck(n.right, resolver)
		if err != nil {
			return Any, err
		}
		switch n.op {
		case "&&", "||":
			if !isBoolLike(left) || !isBoolLike(right) {
				return Any, fmt.Errorf("%s needs bools, got %s and %s", n.op, left, right)
			}
			return Bool, nil
		case "==", "!=":
			if !comparable(left, right) {
				return Any, fmt.Errorf("%s needs comparable values, got %s and %s", n.op, left, right)
			}
			return Bool, nil
		case "<", "<=", ">", ">=":
			if !ordered(left, right) {
				return Any, fmt.Errorf("%s needs two numbers or two strings, got %s and %s", n.op, left, right)
			}
			return Bool, nil
		case "in":
			if right != Array && right != Any {
				return Any, fmt.Errorf("in needs an array on the right, got %s", right)
			}
			return Bool, nil
		case "matches":
			if !isStringLike(left) || !isStringLike(right) {
				return Any, fmt.Errorf("matches needs strings, got %s and %s", left, right)
			}
			if n.right.kind == kindLiteral {
				pattern, ok := n.right.lit.(string)
				if !ok {
					return Any, fmt.Errorf("matches needs a string pattern")
				}
				compiled, err := regexp.Compile(pattern)
				if err != nil {
					return Any, fmt.Errorf("matches: %w", err)
				}
				n.re = compiled
			}
			return Bool, nil
		}
		return Any, fmt.Errorf("unknown operator %q", n.op)
	case kindCall:
		switch n.op {
		case "len":
			if len(n.args) != 1 {
				return Any, fmt.Errorf("len takes one argument")
			}
			argument, err := typeCheck(n.args[0], resolver)
			if err != nil {
				return Any, err
			}
			if !sized(argument) {
				return Any, fmt.Errorf("len needs a string, array or object, got %s", argument)
			}
			return Number, nil
		case "has":
			if len(n.args) != 2 {
				return Any, fmt.Errorf("has takes two arguments")
			}
			object, err := typeCheck(n.args[0], resolver)
			if err != nil {
				return Any, err
			}
			key, err := typeCheck(n.args[1], resolver)
			if err != nil {
				return Any, err
			}
			if object != Object && object != Any {
				return Any, fmt.Errorf("has needs an object, got %s", object)
			}
			if !isStringLike(key) {
				return Any, fmt.Errorf("has needs a string key, got %s", key)
			}
			return Bool, nil
		default:
			return Any, fmt.Errorf("unknown function %q", n.op)
		}
	}
	return Any, fmt.Errorf("unknown expression")
}

func isBoolLike(t Type) bool   { return t == Bool || t == Any }
func isStringLike(t Type) bool { return t == String || t == Any }

func comparable(left, right Type) bool {
	if left == Any || right == Any || left == Null || right == Null {
		return true
	}
	return left == right
}

func ordered(left, right Type) bool {
	if left == Any || right == Any {
		return true
	}
	return (left == Number && right == Number) || (left == String && right == String)
}

func sized(t Type) bool {
	return t == String || t == Array || t == Object || t == Any
}

// ---- evaluation ----

func eval(n *node, ctx map[string]any) (any, error) {
	switch n.kind {
	case kindLiteral:
		return n.lit, nil
	case kindPath:
		return lookup(ctx, n.path)
	case kindUnary:
		value, err := eval(n.left, ctx)
		if err != nil {
			return nil, err
		}
		boolean, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("! needs a bool, got %T", value)
		}
		return !boolean, nil
	case kindBinary:
		switch n.op {
		case "&&":
			left, err := eval(n.left, ctx)
			if err != nil {
				return nil, err
			}
			leftBool, ok := left.(bool)
			if !ok {
				return nil, fmt.Errorf("&& needs bools, got %T", left)
			}
			if !leftBool {
				return false, nil
			}
			right, err := eval(n.right, ctx)
			if err != nil {
				return nil, err
			}
			rightBool, ok := right.(bool)
			if !ok {
				return nil, fmt.Errorf("&& needs bools, got %T", right)
			}
			return rightBool, nil
		case "||":
			left, err := eval(n.left, ctx)
			if err != nil {
				return nil, err
			}
			leftBool, ok := left.(bool)
			if !ok {
				return nil, fmt.Errorf("|| needs bools, got %T", left)
			}
			if leftBool {
				return true, nil
			}
			right, err := eval(n.right, ctx)
			if err != nil {
				return nil, err
			}
			rightBool, ok := right.(bool)
			if !ok {
				return nil, fmt.Errorf("|| needs bools, got %T", right)
			}
			return rightBool, nil
		case "in":
			left, err := eval(n.left, ctx)
			if err != nil {
				return nil, err
			}
			right, err := eval(n.right, ctx)
			if err != nil {
				return nil, err
			}
			return contains(right, left)
		case "matches":
			left, err := eval(n.left, ctx)
			if err != nil {
				return nil, err
			}
			text, ok := left.(string)
			if !ok {
				return nil, fmt.Errorf("matches needs a string, got %T", left)
			}
			if n.re != nil {
				return n.re.MatchString(text), nil
			}
			pattern, err := eval(n.right, ctx)
			if err != nil {
				return nil, err
			}
			patternText, ok := pattern.(string)
			if !ok {
				return nil, fmt.Errorf("matches needs a string pattern, got %T", pattern)
			}
			compiled, err := regexp.Compile(patternText)
			if err != nil {
				return nil, err
			}
			return compiled.MatchString(text), nil
		}
		left, err := eval(n.left, ctx)
		if err != nil {
			return nil, err
		}
		right, err := eval(n.right, ctx)
		if err != nil {
			return nil, err
		}
		return compare(n.op, left, right)
	case kindCall:
		switch n.op {
		case "len":
			value, err := eval(n.args[0], ctx)
			if err != nil {
				return nil, err
			}
			return length(value)
		case "has":
			object, err := eval(n.args[0], ctx)
			if err != nil {
				return nil, err
			}
			key, err := eval(n.args[1], ctx)
			if err != nil {
				return nil, err
			}
			fields, ok := object.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("has needs an object, got %T", object)
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("has needs a string key, got %T", key)
			}
			_, found := fields[name]
			return found, nil
		}
		return nil, fmt.Errorf("unknown function %q", n.op)
	}
	return nil, fmt.Errorf("unknown expression")
}

func lookup(ctx map[string]any, path []string) (any, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	var current any = ctx
	for index, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("path %q: %s is not an object", strings.Join(path, "."), strings.Join(path[:index], "."))
		}
		value, found := object[key]
		if !found {
			return nil, fmt.Errorf("unknown path %q", strings.Join(path, "."))
		}
		current = value
	}
	return current, nil
}

func compare(op string, left, right any) (any, error) {
	switch op {
	case "==":
		return equal(left, right), nil
	case "!=":
		return !equal(left, right), nil
	case "<", "<=", ">", ">=":
		if leftNumber, ok := numberValue(left); ok {
			rightNumber, ok := numberValue(right)
			if !ok {
				return nil, fmt.Errorf("%s needs two numbers", op)
			}
			switch op {
			case "<":
				return leftNumber < rightNumber, nil
			case "<=":
				return leftNumber <= rightNumber, nil
			case ">":
				return leftNumber > rightNumber, nil
			default:
				return leftNumber >= rightNumber, nil
			}
		}
		leftText, leftOK := left.(string)
		rightText, rightOK := right.(string)
		if !leftOK || !rightOK {
			return nil, fmt.Errorf("%s needs two numbers or two strings", op)
		}
		switch op {
		case "<":
			return leftText < rightText, nil
		case "<=":
			return leftText <= rightText, nil
		case ">":
			return leftText > rightText, nil
		default:
			return leftText >= rightText, nil
		}
	}
	return nil, fmt.Errorf("unknown operator %q", op)
}

func equal(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftNumber, leftOK := numberValue(left)
	rightNumber, rightOK := numberValue(right)
	if leftOK && rightOK {
		return leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func contains(haystack, needle any) (bool, error) {
	switch values := haystack.(type) {
	case []any:
		for _, value := range values {
			if equal(value, needle) {
				return true, nil
			}
		}
		return false, nil
	case []string:
		for _, value := range values {
			if equal(value, needle) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("in needs an array on the right, got %T", haystack)
	}
}

func length(value any) (int, error) {
	switch v := value.(type) {
	case string:
		return utf8.RuneCountInString(v), nil
	case []any:
		return len(v), nil
	case []string:
		return len(v), nil
	case map[string]any:
		return len(v), nil
	default:
		return 0, fmt.Errorf("len needs a string, array or object, got %T", value)
	}
}
