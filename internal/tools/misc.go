// Package tools — misc tools: calculator, datetime, process_list.
//
// calculator is a real expression evaluator (tokenizer + recursive-descent
// parser). datetime reports the current time in a requested IANA timezone.
// process_list reads /proc on Linux, falling back to `ps`.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// The factories for this file's tools (framework auto-discovery):
// each tool self-registers here, so adding a tool is one file.
func init() {
	RegisterFactory("calculator", func(deps ToolDeps) (Tool, error) { return &calculatorTool{}, nil })
	RegisterFactory("datetime", func(deps ToolDeps) (Tool, error) { return &datetimeTool{}, nil })
	RegisterFactory("process_list", func(deps ToolDeps) (Tool, error) { return &processListTool{}, nil })
}

// ---------------------------------------------------------------- calculator

type calculatorTool struct{}

func (t *calculatorTool) Name() string { return "calculator" }

// RiskClass declares the tool safe: it only evaluates arithmetic.
func (t *calculatorTool) RiskClass() permissions.Class { return permissions.ClassSafe }

func (t *calculatorTool) Description() string {
	return "Evaluate an arithmetic expression: + - * / % ^, parentheses, unary minus, functions sqrt/abs/sin/cos/tan/log/ln, constants pi and e."
}

func (t *calculatorTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expression": map[string]any{"type": "string", "description": "Arithmetic expression to evaluate, e.g. \"(2+3)*4\"."},
			},
			"required": []string{"expression"},
		},
	}
}

type calculatorArgs struct {
	Expression string `json:"expression"`
}

func (t *calculatorTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a calculatorArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	expr := strings.TrimSpace(a.Expression)
	if expr == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("expression must not be empty")}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	val, err := evalExpression(expr)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: err}
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("result is not a finite number")}
	}
	return fmt.Sprintf("%s = %v", expr, formatNumber(val)), nil
}

func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 15, 64)
}

// --- expression parser: tokenizer + recursive descent ---
//
// Grammar:
//   expr   := term (('+'|'-') term)*
//   term   := factor (('*'|'/'|'%') factor)*
//   factor := unary ('^' factor)?        // right-associative power
//   unary  := '-' unary | primary
//   primary:= number | constant | func '(' expr ')' | '(' expr ')'

type calcTokenKind int

const (
	tokNumber calcTokenKind = iota
	tokIdent
	tokOp
	tokLParen
	tokRParen
	tokEOF
)

type calcToken struct {
	kind calcTokenKind
	text string
	num  float64
}

func tokenizeExpr(input string) ([]calcToken, error) {
	var toks []calcToken
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c >= '0' && c <= '9' || c == '.':
			j := i
			dots := 0
			for j < len(input) && ((input[j] >= '0' && input[j] <= '9') || input[j] == '.') {
				if input[j] == '.' {
					dots++
				}
				j++
			}
			// Optional exponent part.
			if j < len(input) && (input[j] == 'e' || input[j] == 'E') {
				k := j + 1
				if k < len(input) && (input[k] == '+' || input[k] == '-') {
					k++
				}
				if k < len(input) && input[k] >= '0' && input[k] <= '9' {
					for k < len(input) && input[k] >= '0' && input[k] <= '9' {
						k++
					}
					j = k
				}
			}
			if dots > 1 {
				return nil, fmt.Errorf("invalid number %q", input[i:j])
			}
			n, err := strconv.ParseFloat(input[i:j], 64)
			if err != nil {
				return nil, fmt.Errorf("invalid number %q: %w", input[i:j], err)
			}
			toks = append(toks, calcToken{kind: tokNumber, text: input[i:j], num: n})
			i = j
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(input) && ((input[j] >= 'a' && input[j] <= 'z') || (input[j] >= 'A' && input[j] <= 'Z')) {
				j++
			}
			toks = append(toks, calcToken{kind: tokIdent, text: strings.ToLower(input[i:j])})
			i = j
		case c == '+' || c == '-' || c == '*' || c == '/' || c == '%' || c == '^':
			toks = append(toks, calcToken{kind: tokOp, text: string(c)})
			i++
		case c == '(':
			toks = append(toks, calcToken{kind: tokLParen, text: "("})
			i++
		case c == ')':
			toks = append(toks, calcToken{kind: tokRParen, text: ")"})
			i++
		default:
			return nil, fmt.Errorf("unexpected character %q in expression", string(c))
		}
	}
	toks = append(toks, calcToken{kind: tokEOF})
	return toks, nil
}

type calcParser struct {
	toks []calcToken
	pos  int
}

// peek returns the current token, or the EOF token once the input is
// exhausted. next consumes the current token, or the EOF token at the
// end. Both are bounds-checked: tokenizeExpr always appends an EOF
// token, but a parser bug (or a future grammar change) must degrade
// to a parse error, never an index-out-of-range panic — and a panic
// anywhere on the tool path is also recovered by Registry.Execute.
func (p *calcParser) peek() calcToken {
	if p.pos < 0 || p.pos >= len(p.toks) {
		return calcToken{kind: tokEOF}
	}
	return p.toks[p.pos]
}
func (p *calcParser) next() calcToken {
	t := p.peek()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}
func (p *calcParser) expect(k calcTokenKind) (calcToken, error) {
	t := p.next()
	if t.kind != k {
		return t, fmt.Errorf("unexpected token %q", t.text)
	}
	return t, nil
}

func evalExpression(input string) (float64, error) {
	toks, err := tokenizeExpr(input)
	if err != nil {
		return 0, err
	}
	p := &calcParser{toks: toks}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.peek().kind != tokEOF {
		return 0, fmt.Errorf("unexpected trailing input %q", p.peek().text)
	}
	return v, nil
}

func (p *calcParser) parseExpr() (float64, error) {
	v, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind == tokOp && (t.text == "+" || t.text == "-") {
			p.next()
			rhs, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			if t.text == "+" {
				v += rhs
			} else {
				v -= rhs
			}
			continue
		}
		return v, nil
	}
}

func (p *calcParser) parseTerm() (float64, error) {
	v, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind == tokOp && (t.text == "*" || t.text == "/" || t.text == "%") {
			p.next()
			rhs, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			switch t.text {
			case "*":
				v *= rhs
			case "/":
				if rhs == 0 {
					return 0, fmt.Errorf("division by zero")
				}
				v /= rhs
			case "%":
				if rhs == 0 {
					return 0, fmt.Errorf("modulo by zero")
				}
				v = math.Mod(v, rhs)
			}
			continue
		}
		return v, nil
	}
}

func (p *calcParser) parseFactor() (float64, error) {
	base, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	if t := p.peek(); t.kind == tokOp && t.text == "^" {
		p.next()
		exp, err := p.parseFactor() // right-associative
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *calcParser) parseUnary() (float64, error) {
	if t := p.peek(); t.kind == tokOp && t.text == "-" {
		p.next()
		v, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		return -v, nil
	}
	if t := p.peek(); t.kind == tokOp && t.text == "+" {
		p.next()
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *calcParser) parsePrimary() (float64, error) {
	t := p.next()
	switch t.kind {
	case tokNumber:
		return t.num, nil
	case tokLParen:
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return 0, fmt.Errorf("missing closing parenthesis")
		}
		return v, nil
	case tokIdent:
		switch t.text {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		case "sqrt", "abs", "sin", "cos", "tan", "log", "ln":
			if _, err := p.expect(tokLParen); err != nil {
				return 0, fmt.Errorf("expected '(' after function %q", t.text)
			}
			arg, err := p.parseExpr()
			if err != nil {
				return 0, err
			}
			if _, err := p.expect(tokRParen); err != nil {
				return 0, fmt.Errorf("missing closing parenthesis after function %q", t.text)
			}
			return applyFunc(t.text, arg)
		default:
			return 0, fmt.Errorf("unknown identifier %q", t.text)
		}
	default:
		return 0, fmt.Errorf("unexpected token %q", t.text)
	}
}

func applyFunc(name string, arg float64) (float64, error) {
	switch name {
	case "sqrt":
		if arg < 0 {
			return 0, fmt.Errorf("sqrt of negative number")
		}
		return math.Sqrt(arg), nil
	case "abs":
		return math.Abs(arg), nil
	case "sin":
		return math.Sin(arg), nil
	case "cos":
		return math.Cos(arg), nil
	case "tan":
		return math.Tan(arg), nil
	case "log":
		if arg <= 0 {
			return 0, fmt.Errorf("log of non-positive number")
		}
		return math.Log10(arg), nil
	case "ln":
		if arg <= 0 {
			return 0, fmt.Errorf("ln of non-positive number")
		}
		return math.Log(arg), nil
	}
	return 0, fmt.Errorf("unknown function %q", name)
}

// ---------------------------------------------------------------- datetime

type datetimeTool struct{}

func (t *datetimeTool) Name() string { return "datetime" }

// RiskClass declares the tool safe: it only reports the time.
func (t *datetimeTool) RiskClass() permissions.Class { return permissions.ClassSafe }

func (t *datetimeTool) Description() string {
	return "Return the current date and time in a requested IANA timezone (default: local), in RFC3339 and human-readable form."
}

func (t *datetimeTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"timezone": map[string]any{"type": "string", "description": "IANA timezone name, e.g. \"Asia/Singapore\" or \"UTC\". Default: local timezone."},
				"format":   map[string]any{"type": "string", "description": "Optional Go time layout (e.g. \"2006-01-02 15:04:05\"). Default: RFC3339 plus a human-readable line."},
			},
		},
	}
}

type datetimeArgs struct {
	Timezone string `json:"timezone,omitempty"`
	Format   string `json:"format,omitempty"`
}

func (t *datetimeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a datetimeArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	loc := time.Local
	if tz := strings.TrimSpace(a.Timezone); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown timezone %q: %w", tz, err)}
		}
		loc = l
	}
	now := time.Now().In(loc)
	if f := strings.TrimSpace(a.Format); f != "" {
		return now.Format(f), nil
	}
	return fmt.Sprintf("rfc3339: %s\nhuman: %s\ntimezone: %s",
		now.Format(time.RFC3339), now.Format("Monday, 2006-01-02 15:04:05 MST"), loc.String()), nil
}

// ---------------------------------------------------------------- process_list

type processListTool struct{}

func (t *processListTool) Name() string { return "process_list" }

// RiskClass declares the tool safe: it only lists processes.
func (t *processListTool) RiskClass() permissions.Class { return permissions.ClassSafe }

func (t *processListTool) Description() string {
	return "List running processes (pid, command, state), up to 50, sorted by pid."
}

func (t *processListTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

type procInfo struct {
	pid   int
	comm  string
	state string
}

func (t *processListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// No arguments expected; tolerate empty/null args.
	if len(args) > 0 && strings.TrimSpace(string(args)) != "" && strings.TrimSpace(string(args)) != "null" {
		var probe map[string]any
		if err := json.Unmarshal(args, &probe); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("invalid JSON arguments: %w", err)}
		}
	}
	procs, err := listProcsFromProc()
	if err != nil || len(procs) == 0 {
		fallback, ferr := listProcsViaPS(ctx)
		if ferr != nil {
			if err != nil {
				return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot list processes (/proc: %v; ps: %v)", err, ferr)}
			}
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("cannot list processes: %w", ferr)}
		}
		procs = fallback
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].pid < procs[j].pid })
	if len(procs) > 50 {
		procs = procs[:50]
	}
	var sb strings.Builder
	sb.WriteString("PID\tCOMMAND\tSTATE\n")
	for _, p := range procs {
		fmt.Fprintf(&sb, "%d\t%s\t%s\n", p.pid, p.comm, p.state)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func listProcsFromProc() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		// /proc/<pid>/stat: "pid (comm) state ..."
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(data)
		open := strings.Index(s, "(")
		closeIdx := strings.LastIndex(s, ")")
		if open < 0 || closeIdx < 0 || closeIdx <= open {
			continue
		}
		comm := s[open+1 : closeIdx]
		rest := strings.TrimSpace(s[closeIdx+1:])
		state := ""
		if fields := strings.Fields(rest); len(fields) > 0 {
			state = fields[0]
		}
		out = append(out, procInfo{pid: pid, comm: comm, state: state})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no processes found under /proc")
	}
	return out, nil
}

func listProcsViaPS(ctx context.Context) ([]procInfo, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Only pid, command name and state: `args` would leak the full
	// command line (and any secret passed in it) to the model.
	cmd := exec.CommandContext(timeoutCtx, "ps", "-eo", "pid,comm,stat")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps failed: %w", err)
	}
	var procs []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PID") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		state := ""
		if len(fields) > 2 {
			state = fields[2]
		}
		procs = append(procs, procInfo{pid: pid, comm: fields[1], state: state})
	}
	return procs, nil
}
