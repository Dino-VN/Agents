package tools

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Eval evaluates an arithmetic expression with a small recursive-descent parser.
func Eval(expr string) (float64, error) {
	p := &parser{s: strings.ReplaceAll(expr, " ", "")}
	v, err := p.expr()
	if err != nil {
		return 0, err
	}
	if p.i < len(p.s) {
		return 0, fmt.Errorf("ký tự thừa ở vị trí %d: %q", p.i, p.s[p.i:])
	}
	return v, nil
}

type parser struct {
	s string
	i int
}

func (p *parser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *parser) expr() (float64, error) {
	v, err := p.term()
	for err == nil && (p.peek() == '+' || p.peek() == '-') {
		op := p.s[p.i]
		p.i++
		var r float64
		if r, err = p.term(); op == '+' {
			v += r
		} else {
			v -= r
		}
	}
	return v, err
}

func (p *parser) term() (float64, error) {
	v, err := p.power()
	for err == nil && (p.peek() == '*' || p.peek() == '/' || p.peek() == '%') {
		op := p.s[p.i]
		p.i++
		var r float64
		r, err = p.power()
		switch op {
		case '*':
			v *= r
		case '/':
			v /= r
		case '%':
			v = math.Mod(v, r)
		}
	}
	return v, err
}

func (p *parser) power() (float64, error) {
	v, err := p.unary()
	if err == nil && p.peek() == '^' {
		p.i++
		var r float64
		r, err = p.power() // right-associative
		v = math.Pow(v, r)
	}
	return v, err
}

func (p *parser) unary() (float64, error) {
	if p.peek() == '-' {
		p.i++
		v, err := p.unary()
		return -v, err
	}
	if p.peek() == '+' {
		p.i++
	}
	return p.atom()
}

var funcs = map[string]func(float64) float64{
	"sqrt": math.Sqrt, "abs": math.Abs, "round": math.Round, "floor": math.Floor, "ceil": math.Ceil,
	"ln": math.Log, "log": math.Log10, "sin": math.Sin, "cos": math.Cos, "tan": math.Tan, "exp": math.Exp,
}

func (p *parser) atom() (float64, error) {
	c := p.peek()
	switch {
	case c == '(':
		p.i++
		v, err := p.expr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, fmt.Errorf("thiếu ')'")
		}
		p.i++
		return v, nil
	case c >= '0' && c <= '9' || c == '.':
		start := p.i
		for p.i < len(p.s) && (unicode.IsDigit(rune(p.s[p.i])) || p.s[p.i] == '.' ||
			(p.s[p.i] == 'e' || p.s[p.i] == 'E') && p.i+1 < len(p.s) && (unicode.IsDigit(rune(p.s[p.i+1])) || p.s[p.i+1] == '-')) {
			if p.s[p.i] == 'e' || p.s[p.i] == 'E' {
				p.i++ // consume exponent sign/digit together
			}
			p.i++
		}
		return strconv.ParseFloat(p.s[start:p.i], 64)
	case unicode.IsLetter(rune(c)):
		start := p.i
		for p.i < len(p.s) && unicode.IsLetter(rune(p.s[p.i])) {
			p.i++
		}
		name := strings.ToLower(p.s[start:p.i])
		switch name {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		}
		f, ok := funcs[name]
		if !ok {
			return 0, fmt.Errorf("hàm không hỗ trợ: %s", name)
		}
		v, err := p.atom()
		return f(v), err
	}
	return 0, fmt.Errorf("biểu thức không hợp lệ ở vị trí %d", p.i)
}

func formatNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'g', 12, 64)
}
