package lifecycletest

import (
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// This file is a small evaluator for the subset of DynamoDB condition and
// update expressions that package lifecycle uses, with DynamoDB's semantics:
//
//	condition  attribute_exists(p), attribute_not_exists(p), begins_with(p, v),
//	           contains(p, v), = < <= > >=, AND, OR, NOT, parentheses
//	update     SET p = v | v + v | v - v | if_not_exists(p, v)
//	           REMOVE p, ADD p v (number or set), DELETE p v (set)
//
// A path is #name or a bare identifier, optionally dotted into maps. Anything
// outside the subset (list indexes, <>, BETWEEN, IN, size, ...) is rejected
// with a ValidationException rather than guessed at: a fake that silently
// mis-evaluates an expression would let a broken condition pass its tests.
//
// Like DynamoDB, it rejects an expression that references an undefined #name
// or :value, a request that supplies a name or value no expression uses, and
// an update whose paths overlap.

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tName  // #name
	tValue // :value
	tLParen
	tRParen
	tComma
	tDot
	tOp // = < <= > >= <> + -
)

type token struct {
	kind tokKind
	text string
}

func lex(s string) ([]token, error) {
	var out []token
	r := []rune(s)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(':
			out = append(out, token{tLParen, "("})
			i++
		case c == ')':
			out = append(out, token{tRParen, ")"})
			i++
		case c == ',':
			out = append(out, token{tComma, ","})
			i++
		case c == '.':
			out = append(out, token{tDot, "."})
			i++
		case c == '=' || c == '+' || c == '-':
			out = append(out, token{tOp, string(c)})
			i++
		case c == '<' || c == '>':
			op := string(c)
			if i+1 < len(r) && (r[i+1] == '=' || (c == '<' && r[i+1] == '>')) {
				op += string(r[i+1])
				i++
			}
			out = append(out, token{tOp, op})
			i++
		case c == '#' || c == ':' || isIdent(c):
			j := i + 1
			for j < len(r) && isIdent(r[j]) {
				j++
			}
			kind := tIdent
			switch c {
			case '#':
				kind = tName
			case ':':
				kind = tValue
			}
			if (kind != tIdent && j == i+1) || (kind == tIdent && unicode.IsDigit(c)) {
				return nil, fmt.Errorf("invalid token at %q", string(r[i:]))
			}
			out = append(out, token{kind, string(r[i:j])})
			i = j
		default:
			return nil, fmt.Errorf("unsupported character %q", c)
		}
	}
	return append(out, token{tEOF, ""}), nil
}

// ddbError is an error worded as DynamoDB words it (capitalised, as the
// service returns it), so a test can match on the service's own text.
type ddbError string

func (e ddbError) Error() string { return string(e) }

func ddbErrorf(format string, args ...any) error { return ddbError(fmt.Sprintf(format, args...)) }

func isIdent(c rune) bool { return c == '_' || unicode.IsLetter(c) || unicode.IsDigit(c) }

// exprCtx resolves #names and :values, and records which were used.
type exprCtx struct {
	names     map[string]string
	values    map[string]types.AttributeValue
	usedNames map[string]bool
	usedVals  map[string]bool
}

func newExprCtx(names map[string]string, values map[string]types.AttributeValue) *exprCtx {
	return &exprCtx{names: names, values: values, usedNames: map[string]bool{}, usedVals: map[string]bool{}}
}

// checkAllUsed reproduces DynamoDB's rejection of unused names and values.
func (c *exprCtx) checkAllUsed() error {
	for k := range c.names {
		if !c.usedNames[k] {
			return ddbErrorf("Value provided in ExpressionAttributeNames unused in expressions: keys: {%s}", k)
		}
	}
	for k := range c.values {
		if !c.usedVals[k] {
			return ddbErrorf("Value provided in ExpressionAttributeValues unused in expressions: keys: {%s}", k)
		}
	}
	return nil
}

type parser struct {
	toks []token
	pos  int
	ctx  *exprCtx
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) expect(k tokKind, what string) error {
	if t := p.next(); t.kind != k {
		return fmt.Errorf("syntax error: expected %s, got %q", what, t.text)
	}
	return nil
}

func (p *parser) isKeyword(kw string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

// path is a resolved document path: attribute names, outermost first.
type path []string

func (a path) String() string { return strings.Join(a, ".") }

func (a path) overlaps(b path) bool {
	n := min(len(a), len(b))
	return reflect.DeepEqual([]string(a[:n]), []string(b[:n]))
}

func (p *parser) parsePath() (path, error) {
	var out path
	for {
		t := p.next()
		switch t.kind {
		case tName:
			n, ok := p.ctx.names[t.text]
			if !ok {
				return nil, ddbErrorf("An expression attribute name used in the document path is not defined; attribute name: %s", t.text)
			}
			p.ctx.usedNames[t.text] = true
			out = append(out, n)
		case tIdent:
			out = append(out, t.text)
		default:
			return nil, fmt.Errorf("syntax error: expected an attribute path, got %q", t.text)
		}
		if p.peek().kind != tDot {
			return out, nil
		}
		p.next()
	}
}

// operand is a value expression, evaluated against an item.
type operand func(item map[string]types.AttributeValue) (types.AttributeValue, bool, error)

func (p *parser) parseValueRef() (operand, error) {
	t := p.next()
	v, ok := p.ctx.values[t.text]
	if !ok {
		return nil, ddbErrorf("An expression attribute value used in expression is not defined; attribute value: %s", t.text)
	}
	p.ctx.usedVals[t.text] = true
	return func(map[string]types.AttributeValue) (types.AttributeValue, bool, error) { return v, true, nil }, nil
}

// parseOperand parses a path or :value (and, in updates, if_not_exists).
func (p *parser) parseOperand(allowIfNotExists bool) (operand, error) {
	t := p.peek()
	if t.kind == tValue {
		return p.parseValueRef()
	}
	if allowIfNotExists && t.kind == tIdent && t.text == "if_not_exists" {
		p.next()
		if err := p.expect(tLParen, "("); err != nil {
			return nil, err
		}
		pa, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tComma, ","); err != nil {
			return nil, err
		}
		def, err := p.parseOperand(true)
		if err != nil {
			return nil, err
		}
		if err := p.expect(tRParen, ")"); err != nil {
			return nil, err
		}
		return func(item map[string]types.AttributeValue) (types.AttributeValue, bool, error) {
			if v, ok := lookup(item, pa); ok {
				return v, true, nil
			}
			return def(item)
		}, nil
	}
	if t.kind == tIdent && p.toks[p.pos+1].kind == tLParen {
		return nil, fmt.Errorf("fake: function %s is not supported here", t.text)
	}
	pa, err := p.parsePath()
	if err != nil {
		return nil, err
	}
	return func(item map[string]types.AttributeValue) (types.AttributeValue, bool, error) {
		v, ok := lookup(item, pa)
		return v, ok, nil
	}, nil
}

func lookup(item map[string]types.AttributeValue, pa path) (types.AttributeValue, bool) {
	var cur types.AttributeValue = &types.AttributeValueMemberM{Value: item}
	for _, name := range pa {
		m, ok := cur.(*types.AttributeValueMemberM)
		if !ok {
			return nil, false
		}
		cur, ok = m.Value[name]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// cond is a parsed condition expression.
type cond func(item map[string]types.AttributeValue) (bool, error)

// parseCondition parses a whole condition expression.
func parseCondition(expr string, ctx *exprCtx) (cond, error) {
	toks, err := lex(expr)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, ctx: ctx}
	c, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, fmt.Errorf("syntax error: unexpected %q", t.text)
	}
	return c, nil
}

func (p *parser) parseOr() (cond, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("OR") {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l := left
		left = func(it map[string]types.AttributeValue) (bool, error) {
			a, err := l(it)
			if err != nil || a {
				return a, err
			}
			return right(it)
		}
	}
	return left, nil
}

func (p *parser) parseAnd() (cond, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("AND") {
		p.next()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l := left
		left = func(it map[string]types.AttributeValue) (bool, error) {
			a, err := l(it)
			if err != nil || !a {
				return a, err
			}
			return right(it)
		}
	}
	return left, nil
}

func (p *parser) parseNot() (cond, error) {
	if p.isKeyword("NOT") {
		p.next()
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return func(it map[string]types.AttributeValue) (bool, error) {
			v, err := inner(it)
			return !v, err
		}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (cond, error) {
	t := p.peek()
	if t.kind == tLParen {
		p.next()
		c, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		return c, p.expect(tRParen, ")")
	}
	if t.kind == tIdent && p.toks[p.pos+1].kind == tLParen {
		return p.parseFunc()
	}
	left, err := p.parseOperand(false)
	if err != nil {
		return nil, err
	}
	op := p.next()
	if op.kind != tOp {
		return nil, fmt.Errorf("syntax error: expected a comparator, got %q", op.text)
	}
	switch op.text {
	case "=", "<", "<=", ">", ">=":
	default:
		return nil, fmt.Errorf("fake: comparator %q is not supported", op.text)
	}
	right, err := p.parseOperand(false)
	if err != nil {
		return nil, err
	}
	return func(it map[string]types.AttributeValue) (bool, error) {
		a, aok, err := left(it)
		if err != nil {
			return false, err
		}
		b, bok, err := right(it)
		if err != nil {
			return false, err
		}
		// DynamoDB: a comparison with a missing attribute is false.
		if !aok || !bok {
			return false, nil
		}
		return compare(op.text, a, b)
	}, nil
}

func (p *parser) parseFunc() (cond, error) {
	name := p.next().text
	p.next() // (
	pa, err := p.parsePath()
	if err != nil {
		return nil, err
	}
	var arg operand
	switch name {
	case "attribute_exists", "attribute_not_exists":
	case "begins_with", "contains":
		if err := p.expect(tComma, ","); err != nil {
			return nil, err
		}
		if arg, err = p.parseOperand(false); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("fake: function %s is not supported", name)
	}
	if err := p.expect(tRParen, ")"); err != nil {
		return nil, err
	}
	return func(it map[string]types.AttributeValue) (bool, error) {
		v, ok := lookup(it, pa)
		switch name {
		case "attribute_exists":
			return ok, nil
		case "attribute_not_exists":
			return !ok, nil
		}
		a, aok, err := arg(it)
		if err != nil || !ok || !aok {
			return false, err
		}
		if name == "begins_with" {
			s, ok1 := v.(*types.AttributeValueMemberS)
			pre, ok2 := a.(*types.AttributeValueMemberS)
			return ok1 && ok2 && strings.HasPrefix(s.Value, pre.Value), nil
		}
		switch x := v.(type) {
		case *types.AttributeValueMemberS:
			s, ok := a.(*types.AttributeValueMemberS)
			return ok && strings.Contains(x.Value, s.Value), nil
		case *types.AttributeValueMemberSS:
			s, ok := a.(*types.AttributeValueMemberS)
			if !ok {
				return false, nil
			}
			for _, e := range x.Value {
				if e == s.Value {
					return true, nil
				}
			}
		}
		return false, nil
	}, nil
}

func compare(op string, a, b types.AttributeValue) (bool, error) {
	var c int
	switch x := a.(type) {
	case *types.AttributeValueMemberS:
		y, ok := b.(*types.AttributeValueMemberS)
		if !ok {
			return false, nil
		}
		c = strings.Compare(x.Value, y.Value)
	case *types.AttributeValueMemberN:
		y, ok := b.(*types.AttributeValueMemberN)
		if !ok {
			return false, nil
		}
		xr, err := parseNum(x.Value)
		if err != nil {
			return false, err
		}
		yr, err := parseNum(y.Value)
		if err != nil {
			return false, err
		}
		c = xr.Cmp(yr)
	default:
		if op != "=" {
			return false, nil
		}
		return attrEqual(a, b), nil
	}
	switch op {
	case "=":
		return c == 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	default: // ">="
		return c >= 0, nil
	}
}

func attrEqual(a, b types.AttributeValue) bool {
	if sa, ok := a.(*types.AttributeValueMemberSS); ok {
		sb, ok := b.(*types.AttributeValueMemberSS)
		if !ok {
			return false
		}
		x := append([]string(nil), sa.Value...)
		y := append([]string(nil), sb.Value...)
		sort.Strings(x)
		sort.Strings(y)
		return reflect.DeepEqual(x, y)
	}
	return reflect.DeepEqual(a, b)
}

func parseNum(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid number %q", s)
	}
	return r, nil
}

func formatNum(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	return r.FloatString(20)
}

// update is a parsed update expression: actions applied in order to a copy of
// the item.
type update struct {
	actions []func(item map[string]types.AttributeValue, old map[string]types.AttributeValue) error
	paths   []path
}

func parseUpdate(expr string, ctx *exprCtx) (*update, error) {
	toks, err := lex(expr)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, ctx: ctx}
	u := &update{}
	seen := map[string]bool{}
	for p.peek().kind != tEOF {
		kw := p.next()
		clause := strings.ToUpper(kw.text)
		if kw.kind != tIdent || (clause != "SET" && clause != "REMOVE" && clause != "ADD" && clause != "DELETE") {
			return nil, fmt.Errorf("syntax error: expected SET, REMOVE, ADD or DELETE, got %q", kw.text)
		}
		if seen[clause] {
			return nil, ddbErrorf("The %s section can only be used once in an update expression", clause)
		}
		seen[clause] = true
		for {
			if err := p.parseAction(clause, u); err != nil {
				return nil, err
			}
			if p.peek().kind != tComma {
				break
			}
			p.next()
		}
	}
	if len(u.actions) == 0 {
		return nil, fmt.Errorf("empty update expression")
	}
	for i := range u.paths {
		for j := i + 1; j < len(u.paths); j++ {
			if u.paths[i].overlaps(u.paths[j]) {
				return nil, ddbErrorf("Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [%s], path two: [%s]", u.paths[i], u.paths[j])
			}
		}
	}
	return u, nil
}

func (p *parser) parseAction(clause string, u *update) error {
	pa, err := p.parsePath()
	if err != nil {
		return err
	}
	u.paths = append(u.paths, pa)
	switch clause {
	case "SET":
		if t := p.next(); t.kind != tOp || t.text != "=" {
			return fmt.Errorf("syntax error: expected = after SET %s", pa)
		}
		a, err := p.parseOperand(true)
		if err != nil {
			return err
		}
		val := a
		if t := p.peek(); t.kind == tOp && (t.text == "+" || t.text == "-") {
			p.next()
			b, err := p.parseOperand(true)
			if err != nil {
				return err
			}
			val = arith(t.text, a, b)
		}
		u.actions = append(u.actions, func(item, old map[string]types.AttributeValue) error {
			v, ok, err := val(old)
			if err != nil {
				return err
			}
			if !ok {
				return ddbErrorf("The provided expression refers to an attribute that does not exist in the item")
			}
			return setPath(item, pa, v)
		})
	case "REMOVE":
		u.actions = append(u.actions, func(item, _ map[string]types.AttributeValue) error {
			removePath(item, pa)
			return nil
		})
	case "ADD", "DELETE":
		if p.peek().kind != tValue {
			return fmt.Errorf("syntax error: %s needs a :value operand", clause)
		}
		v, err := p.parseValueRef()
		if err != nil {
			return err
		}
		if len(pa) != 1 {
			return fmt.Errorf("fake: %s on a nested path is not supported", clause)
		}
		u.actions = append(u.actions, func(item, _ map[string]types.AttributeValue) error {
			arg, _, _ := v(nil)
			cur, exists := item[pa[0]]
			if clause == "ADD" {
				return addAttr(item, pa[0], cur, exists, arg)
			}
			return deleteFromSet(item, pa[0], cur, exists, arg)
		})
	}
	return nil
}

func arith(op string, a, b operand) operand {
	return func(item map[string]types.AttributeValue) (types.AttributeValue, bool, error) {
		x, xok, err := a(item)
		if err != nil {
			return nil, false, err
		}
		y, yok, err := b(item)
		if err != nil {
			return nil, false, err
		}
		if !xok || !yok {
			return nil, false, ddbErrorf("The provided expression refers to an attribute that does not exist in the item")
		}
		xn, ok1 := x.(*types.AttributeValueMemberN)
		yn, ok2 := y.(*types.AttributeValueMemberN)
		if !ok1 || !ok2 {
			return nil, false, ddbErrorf("An operand in the update expression has an incorrect data type")
		}
		xr, err := parseNum(xn.Value)
		if err != nil {
			return nil, false, err
		}
		yr, err := parseNum(yn.Value)
		if err != nil {
			return nil, false, err
		}
		if op == "+" {
			xr.Add(xr, yr)
		} else {
			xr.Sub(xr, yr)
		}
		return &types.AttributeValueMemberN{Value: formatNum(xr)}, true, nil
	}
}

func setPath(item map[string]types.AttributeValue, pa path, v types.AttributeValue) error {
	m := item
	for _, name := range pa[:len(pa)-1] {
		next, ok := m[name].(*types.AttributeValueMemberM)
		if !ok {
			return ddbErrorf("The document path provided in the update expression is invalid for update")
		}
		m = next.Value
	}
	m[pa[len(pa)-1]] = cloneAttr(v)
	return nil
}

func removePath(item map[string]types.AttributeValue, pa path) {
	m := item
	for _, name := range pa[:len(pa)-1] {
		next, ok := m[name].(*types.AttributeValueMemberM)
		if !ok {
			return
		}
		m = next.Value
	}
	delete(m, pa[len(pa)-1])
}

func addAttr(item map[string]types.AttributeValue, name string, cur types.AttributeValue, exists bool, arg types.AttributeValue) error {
	switch a := arg.(type) {
	case *types.AttributeValueMemberN:
		if !exists {
			item[name] = cloneAttr(a)
			return nil
		}
		c, ok := cur.(*types.AttributeValueMemberN)
		if !ok {
			return ddbErrorf("An operand in the update expression has an incorrect data type")
		}
		return setPath(item, path{name}, mustArith("+", c, a))
	case *types.AttributeValueMemberSS:
		set := map[string]bool{}
		if exists {
			c, ok := cur.(*types.AttributeValueMemberSS)
			if !ok {
				return ddbErrorf("An operand in the update expression has an incorrect data type")
			}
			for _, e := range c.Value {
				set[e] = true
			}
		}
		for _, e := range a.Value {
			set[e] = true
		}
		item[name] = &types.AttributeValueMemberSS{Value: sortedKeys(set)}
		return nil
	}
	return ddbErrorf("Incorrect operand type for operator or function; operator: ADD")
}

func deleteFromSet(item map[string]types.AttributeValue, name string, cur types.AttributeValue, exists bool, arg types.AttributeValue) error {
	a, ok := arg.(*types.AttributeValueMemberSS)
	if !ok {
		return ddbErrorf("Incorrect operand type for operator or function; operator: DELETE")
	}
	if !exists {
		return nil
	}
	c, ok := cur.(*types.AttributeValueMemberSS)
	if !ok {
		return ddbErrorf("An operand in the update expression has an incorrect data type")
	}
	set := map[string]bool{}
	for _, e := range c.Value {
		set[e] = true
	}
	for _, e := range a.Value {
		delete(set, e)
	}
	if len(set) == 0 {
		delete(item, name) // DynamoDB has no empty sets
		return nil
	}
	item[name] = &types.AttributeValueMemberSS{Value: sortedKeys(set)}
	return nil
}

func mustArith(op string, a, b *types.AttributeValueMemberN) types.AttributeValue {
	v, _, _ := arith(op, constOp(a), constOp(b))(nil)
	return v
}

func constOp(v types.AttributeValue) operand {
	return func(map[string]types.AttributeValue) (types.AttributeValue, bool, error) { return v, true, nil }
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cloneAttr(v types.AttributeValue) types.AttributeValue {
	switch x := v.(type) {
	case *types.AttributeValueMemberM:
		return &types.AttributeValueMemberM{Value: cloneItem(x.Value)}
	case *types.AttributeValueMemberSS:
		return &types.AttributeValueMemberSS{Value: append([]string(nil), x.Value...)}
	case *types.AttributeValueMemberNS:
		return &types.AttributeValueMemberNS{Value: append([]string(nil), x.Value...)}
	case *types.AttributeValueMemberL:
		l := make([]types.AttributeValue, len(x.Value))
		for i, e := range x.Value {
			l[i] = cloneAttr(e)
		}
		return &types.AttributeValueMemberL{Value: l}
	case *types.AttributeValueMemberS:
		return &types.AttributeValueMemberS{Value: x.Value}
	case *types.AttributeValueMemberN:
		return &types.AttributeValueMemberN{Value: x.Value}
	case *types.AttributeValueMemberBOOL:
		return &types.AttributeValueMemberBOOL{Value: x.Value}
	}
	return v
}

func cloneItem(m map[string]types.AttributeValue) map[string]types.AttributeValue {
	if m == nil {
		return nil
	}
	out := make(map[string]types.AttributeValue, len(m))
	for k, v := range m {
		out[k] = cloneAttr(v)
	}
	return out
}
