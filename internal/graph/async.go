package graph

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Static call edges stop where control is handed off indirectly. Three
// rendezvous patterns recover most of those hops without knowing any
// library's API:
//
//   - channels: a send on chan T links to every function receiving from a
//     chan T, for named local T (chan error or chan struct{} would link
//     everything to everything);
//   - keys: a call passing an enum-like key (a package-level constant or
//     variable of a local named string/int type) together with a function
//     value registers that function as the key's handler
//     (bus.Subscribe(topic, h), reg(CmdStart, func...)); any other call
//     passing the same key emits it and links to those handlers;
//   - callbacks: a function calling its own func-typed parameter links to
//     every function value passed for that parameter at its call sites.
//
// Closures get their own nodes (Owner.funcN), because handlers and goroutine
// bodies are usually anonymous.

// maxFanOut skips rendezvous that link too many pairs to be informative.
const maxFanOut = 64

// params maps a function's func-typed parameters to callback keys
// ("pkg.Type.Method(name)"); closures see their enclosing function's map.
type params map[*types.Var]string

func funcParams(info *types.Info, owner string, ft *ast.FuncType) params {
	ps := params{}
	for _, field := range ft.Params.List {
		for _, name := range field.Names {
			v, ok := info.Defs[name].(*types.Var)
			if !ok || name.Name == "_" {
				continue
			}
			if _, isFunc := v.Type().Underlying().(*types.Signature); isFunc {
				ps[v] = owner + "(" + name.Name + ")"
			}
		}
	}
	return ps
}

// walkBody records calls, closures and rendezvous points of one function body.
func (b *builder) walkBody(p *packages.Package, owner string, body *ast.BlockStmt, ps params) {
	info := p.TypesInfo
	lits := map[*ast.FuncLit]string{}
	litID := func(lit *ast.FuncLit) string {
		if id, ok := lits[lit]; ok {
			return id
		}
		id := fmt.Sprintf("%s.func%d", owner, len(lits)+1)
		lits[lit] = id
		return id
	}

	ast.Inspect(body, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.FuncLit:
			id := litID(x)
			b.addClosure(p, owner, id, x)
			b.g.AddEdge(owner, id, EdgeCalls)
			b.walkBody(p, id, x.Body, ps)
			return false
		case *ast.Ident:
			if callee, ok := info.Uses[x].(*types.Func); ok {
				if to := FuncID(callee); to != "" {
					b.g.AddEdge(owner, to, EdgeCalls)
				}
			}
		case *ast.SendStmt:
			if key := b.chanKey(info.TypeOf(x.Chan)); key != "" {
				b.sends[key] = append(b.sends[key], owner)
			}
		case *ast.UnaryExpr:
			if x.Op == token.ARROW {
				if key := b.chanKey(info.TypeOf(x.X)); key != "" {
					b.recvs[key] = append(b.recvs[key], owner)
				}
			}
		case *ast.RangeStmt:
			if key := b.chanKey(info.TypeOf(x.X)); key != "" {
				b.recvs[key] = append(b.recvs[key], owner)
			}
		case *ast.CallExpr:
			if id, ok := ast.Unparen(x.Fun).(*ast.Ident); ok {
				if v, ok := info.Uses[id].(*types.Var); ok && ps[v] != "" {
					b.paramCalls[ps[v]] = append(b.paramCalls[ps[v]], owner)
				}
			}
			b.rendezvous(info, owner, x, litID)
		}
		return true
	})
}

func (b *builder) addClosure(p *packages.Package, owner, id string, lit *ast.FuncLit) {
	parent := b.g.Nodes[owner]
	sig := types.TypeString(p.TypesInfo.TypeOf(lit), shortQualifier(p.Types))
	n := &Node{
		ID:        id,
		Kind:      KindFunc,
		Name:      id[strings.LastIndex(id, ".")+1:],
		Pkg:       parent.Pkg,
		Parent:    owner,
		Signature: sig + "  (closure in " + parent.Name + ")",
		Pos:       b.fset.Position(lit.Pos()),
		Source:    b.source(lit),
	}
	n.Test = strings.HasSuffix(n.Pos.Filename, "_test.go")
	b.g.add(n)
}

// rendezvous inspects the arguments of one call. Keys passed with a function
// value register handlers, keys passed without one are emitted, and function
// values bound to a known callee's func parameters become callback targets.
func (b *builder) rendezvous(info *types.Info, owner string, call *ast.CallExpr, litID func(*ast.FuncLit) string) {
	callee := calleeFunc(info, call)
	var keys, funcs []string
	for i, arg := range call.Args {
		arg = ast.Unparen(arg)
		if k := b.localKey(info, arg); k != "" {
			keys = append(keys, k)
		}
		fn := ""
		switch a := arg.(type) {
		case *ast.FuncLit:
			fn = litID(a)
		case *ast.Ident:
			if f, ok := info.Uses[a].(*types.Func); ok {
				fn = FuncID(f)
			}
		case *ast.SelectorExpr:
			if f, ok := info.Uses[a.Sel].(*types.Func); ok {
				fn = FuncID(f)
			}
		}
		if fn == "" {
			continue
		}
		funcs = append(funcs, fn)
		if key := paramKey(callee, i); key != "" {
			b.binds[key] = append(b.binds[key], fn)
		}
	}
	for _, k := range keys {
		if len(funcs) > 0 {
			b.handlers[k] = append(b.handlers[k], funcs...)
		} else {
			b.emits[k] = append(b.emits[k], owner)
		}
	}
}

// calleeFunc resolves a call to a declared function or method, or nil.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	switch fn := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		f, _ := info.Uses[fn].(*types.Func)
		return f
	case *ast.SelectorExpr:
		f, _ := info.Uses[fn.Sel].(*types.Func)
		return f
	}
	return nil
}

// paramKey is the callback key of callee's i-th parameter, matching funcParams.
func paramKey(callee *types.Func, i int) string {
	if callee == nil {
		return ""
	}
	callee = callee.Origin()
	ps := callee.Type().(*types.Signature).Params()
	if i >= ps.Len() {
		return ""
	}
	name := ps.At(i).Name()
	if name == "" || name == "_" {
		return ""
	}
	id := FuncID(callee)
	if id == "" {
		return ""
	}
	return id + "(" + name + ")"
}

// localKey returns the ID of an enum-like key: a constant or package-level
// variable declared in a loaded package, whose type is a named string or
// integer type from a loaded package. Variables count because enums are often
// re-exported as "var X = otherpkg.X". Anything else returns "".
func (b *builder) localKey(info *types.Info, e ast.Expr) string {
	var id *ast.Ident
	switch e := e.(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return ""
	}
	obj := info.Uses[id]
	switch o := obj.(type) {
	case *types.Const:
	case *types.Var:
		if o.Pkg() == nil || o.Parent() != o.Pkg().Scope() {
			return "" // locals, params and fields aren't stable keys
		}
	default:
		return ""
	}
	if obj.Pkg() == nil || b.local[obj.Pkg().Path()] == nil {
		return ""
	}
	named, ok := types.Unalias(obj.Type()).(*types.Named)
	if !ok || named.Obj().Pkg() == nil || b.local[named.Obj().Pkg().Path()] == nil {
		return ""
	}
	basic, ok := named.Underlying().(*types.Basic)
	if !ok || basic.Info()&(types.IsString|types.IsInteger) == 0 {
		return ""
	}
	return obj.Pkg().Path() + "." + obj.Name()
}

// chanKey identifies a channel by its element type when that type is a named
// type declared in a loaded package, or returns "".
func (b *builder) chanKey(t types.Type) string {
	if t == nil {
		return ""
	}
	ch, ok := t.Underlying().(*types.Chan)
	if !ok {
		return ""
	}
	elem := ch.Elem()
	if p, ok := elem.(*types.Pointer); ok {
		elem = p.Elem()
	}
	named, ok := types.Unalias(elem).(*types.Named)
	if !ok || named.Obj().Pkg() == nil || b.local[named.Obj().Pkg().Path()] == nil {
		return ""
	}
	return TypeID(named.Origin().Obj())
}

// linkAsync turns collected rendezvous points into async edges.
func (b *builder) linkAsync() {
	link := func(from, to map[string][]string, via string) {
		for key, srcs := range from {
			dsts := to[key]
			srcs, dsts = dedupe(srcs), dedupe(dsts)
			if len(srcs)*len(dsts) > maxFanOut {
				continue
			}
			label := via + " " + b.g.ShortID(key)
			for _, s := range srcs {
				for _, d := range dsts {
					if s != d && b.linkable(s, d) {
						b.g.AddEdgeVia(s, d, EdgeAsync, label)
					}
				}
			}
		}
	}
	link(b.sends, b.recvs, "chan")
	link(b.emits, b.handlers, "key")
	link(b.paramCalls, b.binds, "callback")
}

// linkable keeps indirect edges out of test code unless they stay inside one
// test function: a test publishing to production handlers is a real flow, but
// production code (or another test) reaching a test's fake through a shared
// key or channel type is a coincidence of names.
func (b *builder) linkable(src, dst string) bool {
	s, d := b.g.Nodes[src], b.g.Nodes[dst]
	if s == nil || d == nil {
		return false
	}
	return !d.Test || rootFunc(src) == rootFunc(dst)
}

// rootFunc strips closure suffixes: "pkg.TestX.func2.func1" -> "pkg.TestX".
func rootFunc(id string) string {
	for {
		i := strings.LastIndex(id, ".func")
		if i < 0 || strings.TrimLeft(id[i+len(".func"):], "0123456789") != "" {
			return id
		}
		id = id[:i]
	}
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
