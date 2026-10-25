package graph

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
	packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedModule

// Load type-checks the packages matching patterns (default "./...") under dir
// and builds the graph.
func Load(dir string, patterns ...string) (*Graph, error) {
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	// Tests: true adds a variant of each package that includes its _test.go
	// files (fakes and mocks are real implementations agents must not miss).
	pkgs, err := packages.Load(&packages.Config{Mode: loadMode, Dir: root, Fset: fset, Tests: true}, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}

	b := &builder{
		g:     New(),
		fset:  fset,
		local: map[string]*packages.Package{},
		decls: map[types.Object]decl{},
		src:   map[string][]byte{},

		sends:      map[string][]string{},
		recvs:      map[string][]string{},
		emits:      map[string][]string{},
		handlers:   map[string][]string{},
		paramCalls: map[string][]string{},
		binds:      map[string][]string{},
	}
	b.g.Root = root
	warned := map[string]bool{}
	for _, p := range pkgs {
		for _, e := range p.Errors {
			if msg := e.Error(); !warned[msg] {
				warned[msg] = true
				b.g.Warnings = append(b.g.Warnings, msg)
			}
		}
		if p.Types == nil || p.TypesInfo == nil || inNodeModules(p) || strings.HasSuffix(p.PkgPath, ".test") {
			continue // .test is the generated test main
		}
		if p.Module != nil && b.g.Module == "" {
			b.g.Module = p.Module.Path
		}
		// The test variant shares PkgPath and is a superset of the plain package.
		if prev, ok := b.local[p.PkgPath]; ok && len(prev.Syntax) >= len(p.Syntax) {
			continue
		}
		b.local[p.PkgPath] = p
	}
	if len(b.local) == 0 {
		return nil, fmt.Errorf("no loadable packages matched %v in %s", patterns, root)
	}

	ordered := make([]*packages.Package, 0, len(b.local))
	for _, p := range b.local {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PkgPath < ordered[j].PkgPath })

	for _, p := range ordered {
		b.addPackage(p)
	}
	for _, p := range ordered {
		b.addCalls(p)
		for path := range p.Imports {
			if b.local[path] != nil {
				b.g.AddEdge(p.PkgPath, path, EdgeImports)
			}
		}
	}
	b.addImplements()
	b.linkAsync()
	b.g.finish()
	return b.g, nil
}

type decl struct {
	node ast.Node
	doc  *ast.CommentGroup
}

type builder struct {
	g     *Graph
	fset  *token.FileSet
	local map[string]*packages.Package
	decls map[types.Object]decl
	src   map[string][]byte

	// Rendezvous points collected while walking bodies; see async.go.
	sends, recvs      map[string][]string // channel element type -> funcs
	emits, handlers   map[string][]string // enum-like key -> emitting funcs / registered handlers
	paramCalls, binds map[string][]string // callback param -> funcs calling it / funcs passed for it
}

func (b *builder) addPackage(p *packages.Package) {
	pn := &Node{ID: p.PkgPath, Kind: KindPackage, Name: p.Name, Pkg: p.PkgPath, Exported: true}
	pn.Test = strings.HasSuffix(p.PkgPath, "_test") // external test package
	generated := 0
	for _, f := range p.Syntax {
		if ast.IsGenerated(f) {
			generated++
		}
		// Package docs live in regular files; a test file's header isn't one.
		if pn.Doc == "" && f.Doc != nil && !strings.HasSuffix(b.fset.Position(f.Package).Filename, "_test.go") {
			pn.Doc = strings.TrimSpace(f.Doc.Text())
			pn.Pos = b.fset.Position(f.Package)
		}
		b.indexDecls(p.TypesInfo, f)
	}
	// Generators like ent leave a couple of hand-written files next to
	// hundreds of generated ones; that package is still generated code.
	pn.Generated = generated > 0 && generated*10 >= len(p.Syntax)*9
	if !pn.Pos.IsValid() && len(p.Syntax) > 0 {
		pn.Pos = b.fset.Position(p.Syntax[0].Package)
	}
	b.g.add(pn)

	q := shortQualifier(p.Types)
	scope := p.Types.Scope()
	for _, name := range scope.Names() {
		switch obj := scope.Lookup(name).(type) {
		case *types.TypeName:
			b.addType(pn, obj, q)
		case *types.Func:
			b.addFunc(pn, obj, q)
		}
	}
}

// indexDecls maps declared objects to their AST so nodes can carry docs and
// source. Interface methods are indexed via their fields.
func (b *builder) indexDecls(info *types.Info, f *ast.File) {
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if obj := info.Defs[d.Name]; obj != nil {
				b.decls[obj] = decl{node: d, doc: d.Doc}
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts := spec.(*ast.TypeSpec)
				dc := decl{node: ts, doc: ts.Doc}
				if len(d.Specs) == 1 {
					dc.node = d
					if dc.doc == nil {
						dc.doc = d.Doc
					}
				}
				if obj := info.Defs[ts.Name]; obj != nil {
					b.decls[obj] = dc
				}
				if it, ok := ts.Type.(*ast.InterfaceType); ok {
					for _, m := range it.Methods.List {
						for _, name := range m.Names {
							if obj := info.Defs[name]; obj != nil {
								b.decls[obj] = decl{node: m, doc: m.Doc}
							}
						}
					}
				}
			}
		}
	}
}

func (b *builder) addType(pkg *Node, obj *types.TypeName, q types.Qualifier) {
	n := &Node{
		ID:       pkg.ID + "." + obj.Name(),
		Kind:     KindType,
		Name:     obj.Name(),
		Pkg:      pkg.ID,
		Parent:   pkg.ID,
		Exported: obj.Exported(),
	}
	b.fill(n, obj)
	b.g.add(n) // before methods, so they attach as children

	if obj.IsAlias() {
		n.Signature = fmt.Sprintf("type %s = %s", obj.Name(), types.TypeString(types.Unalias(obj.Type()), q))
		return
	}
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return
	}
	head := "type " + obj.Name() + typeParams(named.TypeParams(), q)
	switch u := named.Underlying().(type) {
	case *types.Interface:
		n.Kind = KindInterface
		n.Signature = head + " interface"
		for i := range u.NumEmbeddeds() {
			n.Fields = append(n.Fields, types.TypeString(u.EmbeddedType(i), q))
		}
		for i := range u.NumExplicitMethods() {
			b.addFunc(n, u.ExplicitMethod(i), q)
		}
	case *types.Struct:
		n.Signature = head + " struct"
		for i := range u.NumFields() {
			f := u.Field(i)
			if f.Embedded() {
				n.Fields = append(n.Fields, types.TypeString(f.Type(), q))
			} else {
				n.Fields = append(n.Fields, f.Name()+" "+types.TypeString(f.Type(), q))
			}
		}
	default:
		n.Signature = head + " " + types.TypeString(u, q)
	}
	for i := range named.NumMethods() {
		b.addFunc(n, named.Method(i), q)
	}
}

func (b *builder) addFunc(parent *Node, f *types.Func, q types.Qualifier) {
	id := FuncID(f)
	if id == "" {
		return
	}
	sig := f.Type().(*types.Signature)
	params := strings.TrimPrefix(types.TypeString(sig, q), "func")
	n := &Node{
		ID:        id,
		Kind:      KindFunc,
		Name:      f.Name(),
		Pkg:       parent.Pkg,
		Parent:    parent.ID,
		Exported:  f.Exported(),
		Signature: "func " + f.Name() + params,
	}
	if recv := sig.Recv(); recv != nil {
		n.Kind = KindMethod
		if types.IsInterface(recv.Type()) {
			n.Signature = f.Name() + params
		} else {
			r := types.TypeString(recv.Type(), q)
			if recv.Name() != "" && recv.Name() != "_" {
				r = recv.Name() + " " + r
			}
			n.Signature = fmt.Sprintf("func (%s) %s%s", r, f.Name(), params)
		}
	}
	b.fill(n, f)
	b.g.add(n)
}

func (b *builder) fill(n *Node, obj types.Object) {
	n.Pos = b.fset.Position(obj.Pos())
	n.Test = strings.HasSuffix(n.Pos.Filename, "_test.go")
	d, ok := b.decls[obj]
	if !ok {
		return
	}
	n.Doc = strings.TrimSpace(d.doc.Text())
	n.Source = b.source(d.node)
}

func (b *builder) source(node ast.Node) string {
	start, end := b.fset.Position(node.Pos()), b.fset.Position(node.End())
	data, ok := b.src[start.Filename]
	if !ok {
		data, _ = os.ReadFile(start.Filename)
		b.src[start.Filename] = data
	}
	if end.Offset > len(data) || start.Offset > end.Offset {
		return ""
	}
	return string(data[start.Offset:end.Offset])
}

// addCalls records every reference to a function or method inside a function
// body, including method values and callbacks, not just direct calls.
// Calls through an interface point at the interface method; use
// implementations to follow the dispatch.
func (b *builder) addCalls(p *packages.Package) {
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			obj, _ := p.TypesInfo.Defs[fd.Name].(*types.Func)
			if obj == nil {
				continue
			}
			if from := FuncID(obj); b.g.Nodes[from] != nil {
				b.walkBody(p, from, fd.Body, funcParams(p.TypesInfo, from, fd.Type))
			}
		}
	}
}

// addImplements links concrete local types to the local interfaces (and
// error) they satisfy, by value or pointer receiver. Generic types are
// skipped: uninstantiated generics can't be checked meaningfully.
//
// Method sets are compared by signature strings rather than types.Implements:
// with tests loaded, one package can appear as two type-checked variants, and
// their types are not identical to go/types even though they are the same code.
func (b *builder) addImplements() {
	type methods map[string]string
	type iface struct {
		id      string
		methods methods
	}
	var ifaces []iface
	var concretes []*types.TypeName
	for _, p := range b.local {
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || named.TypeParams().Len() > 0 {
				continue
			}
			if it, ok := named.Underlying().(*types.Interface); ok {
				if it.NumMethods() > 0 {
					ms := methods{}
					for i := range it.NumMethods() {
						ms[methodKey(it.Method(i))] = sigKey(it.Method(i))
					}
					ifaces = append(ifaces, iface{id: TypeID(tn), methods: ms})
				}
			} else {
				concretes = append(concretes, tn)
			}
		}
	}
	errorMethod := types.Universe.Lookup("error").Type().Underlying().(*types.Interface).Method(0)
	ifaces = append(ifaces, iface{id: "error", methods: methods{"Error": sigKey(errorMethod)}})

	for _, c := range concretes {
		// The pointer method set includes value methods and promoted ones.
		mset := types.NewMethodSet(types.NewPointer(c.Type()))
		if mset.Len() == 0 {
			continue
		}
		have := make(methods, mset.Len())
		for i := range mset.Len() {
			f := mset.At(i).Obj().(*types.Func)
			have[methodKey(f)] = sigKey(f)
		}
	next:
		for _, i := range ifaces {
			for name, sig := range i.methods {
				if have[name] != sig {
					continue next
				}
			}
			b.g.AddEdge(TypeID(c), i.id, EdgeImplements)
		}
	}
}

// methodKey qualifies unexported names: they only match within one package.
func methodKey(f *types.Func) string {
	if f.Exported() || f.Pkg() == nil {
		return f.Name()
	}
	return f.Pkg().Path() + "." + f.Name()
}

// sigKey renders a method signature from types alone; parameter names don't
// affect whether a method satisfies an interface.
func sigKey(f *types.Func) string {
	sig := f.Type().(*types.Signature)
	tuple := func(t *types.Tuple) string {
		parts := make([]string, t.Len())
		for i := range t.Len() {
			parts[i] = types.TypeString(t.At(i).Type(), (*types.Package).Path)
		}
		return "(" + strings.Join(parts, ",") + ")"
	}
	key := tuple(sig.Params()) + tuple(sig.Results())
	if sig.Variadic() {
		key += "..."
	}
	return key
}

// FuncID is pkgpath.Func or pkgpath.Type.Method; "" for methods of
// anonymous interfaces, which have no stable name.
func FuncID(f *types.Func) string {
	f = f.Origin()
	sig := f.Type().(*types.Signature)
	recv := sig.Recv()
	if recv == nil {
		if f.Pkg() == nil {
			return f.Name()
		}
		return f.Pkg().Path() + "." + f.Name()
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return ""
	}
	return TypeID(named.Obj()) + "." + f.Name()
}

func TypeID(tn *types.TypeName) string {
	if tn.Pkg() == nil {
		return tn.Name() // universe: error, comparable
	}
	return tn.Pkg().Path() + "." + tn.Name()
}

// inNodeModules drops Go files vendored by JS tooling; "./..." doesn't skip them.
func inNodeModules(p *packages.Package) bool {
	for _, f := range p.GoFiles {
		if strings.Contains(f, string(filepath.Separator)+"node_modules"+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// shortQualifier writes other packages by name, as they appear in source.
func shortQualifier(self *types.Package) types.Qualifier {
	return func(p *types.Package) string {
		if p == self {
			return ""
		}
		return p.Name()
	}
}

func typeParams(list *types.TypeParamList, q types.Qualifier) string {
	if list.Len() == 0 {
		return ""
	}
	parts := make([]string, list.Len())
	for i := range list.Len() {
		tp := list.At(i)
		parts[i] = tp.Obj().Name() + " " + types.TypeString(tp.Constraint(), q)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
