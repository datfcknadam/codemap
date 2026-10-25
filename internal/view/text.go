// Package view renders graph levels as compact text, meant to be read by an
// AI agent as much as by a human.
package view

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/datfcknadam/codemap/internal/graph"
)

type Options struct {
	ExportedOnly bool
	Body         bool
	Imports      bool   // map: list internal imports per package
	Prefix       string // map: only packages under this path
}

// Map is the top level: packages, their size and docs, optionally narrowed
// to a subtree and with internal imports.
func Map(w io.Writer, g *graph.Graph, opt Options) {
	var shown, generated []string
	for _, id := range g.Packages {
		p := g.Nodes[id]
		if p.Test || !underPrefix(g, id, opt.Prefix) {
			continue
		}
		if p.Generated {
			generated = append(generated, id)
		} else {
			shown = append(shown, id)
		}
	}
	scope := ""
	if opt.Prefix != "" {
		scope = " under " + opt.Prefix
	}
	fmt.Fprintf(w, "module %s — %d packages%s\n\n", orDash(g.Module), len(shown)+len(generated), scope)
	for _, id := range shown {
		p := g.Nodes[id]
		types, funcs := 0, 0
		for _, c := range g.Children(p) {
			if c.Test || opt.ExportedOnly && !c.Exported {
				continue
			}
			if c.Kind == graph.KindFunc {
				funcs++
			} else {
				types++
			}
		}
		fmt.Fprintf(w, "%s  (%d types, %d funcs)\n", g.ShortID(id), types, funcs)
		if s := firstSentence(p.Doc); s != "" {
			fmt.Fprintf(w, "  // %s\n", s)
		}
		if opt.Imports {
			if imps := importsSummary(g, id); imps != "" {
				fmt.Fprintf(w, "  imports: %s\n", imps)
			}
		}
	}
	if len(generated) > 0 {
		fmt.Fprintf(w, "\ngenerated (%d packages, use show to open): %s\n",
			len(generated), strings.Join(collapseTree(g, generated), ", "))
	}
	if !opt.Imports {
		fmt.Fprintln(w, "\n(imports hidden; pass imports=true or show <package> for a package's dependencies)")
	}
}

func underPrefix(g *graph.Graph, id, prefix string) bool {
	if prefix == "" {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	short := g.ShortID(id)
	return short == prefix || id == prefix ||
		strings.HasPrefix(short, prefix+"/") || strings.HasPrefix(id, prefix+"/")
}

// collapseTree folds sorted package paths under their topmost listed
// ancestor: internal/ent, internal/ent/user → "internal/ent (+1 nested)".
func collapseTree(g *graph.Graph, ids []string) []string {
	var out []string
	root, nested := "", 0
	flush := func() {
		if root == "" {
			return
		}
		if nested > 0 {
			out = append(out, fmt.Sprintf("%s (+%d nested)", g.ShortID(root), nested))
		} else {
			out = append(out, g.ShortID(root))
		}
	}
	for _, id := range ids {
		if root != "" && strings.HasPrefix(id, root+"/") {
			nested++
			continue
		}
		flush()
		root, nested = id, 0
	}
	flush()
	return out
}

// importsSummary lists hand-written imports and folds generated ones into a
// count: a module importing 40 ent packages is one fact, not 40.
func importsSummary(g *graph.Graph, id string) string {
	var names []string
	generated := 0
	for _, imp := range g.Out(id, graph.EdgeImports) {
		if g.Nodes[imp].Generated {
			generated++
			continue
		}
		names = append(names, g.ShortID(imp))
	}
	if generated > 0 {
		names = append(names, fmt.Sprintf("+%d generated", generated))
	}
	return strings.Join(names, ", ")
}

// Show renders one node at its natural level of detail.
func Show(w io.Writer, g *graph.Graph, n *graph.Node, opt Options) {
	switch n.Kind {
	case graph.KindPackage:
		showPackage(w, g, n, opt)
	case graph.KindType, graph.KindInterface:
		showType(w, g, n, opt)
	default:
		showFunc(w, g, n, opt)
	}
}

func showPackage(w io.Writer, g *graph.Graph, n *graph.Node, opt Options) {
	fmt.Fprintf(w, "package %s  (%s)\n", n.Name, g.ShortID(n.ID))
	writeDoc(w, "", n.Doc, false)
	if imps := g.Out(n.ID, graph.EdgeImports); len(imps) > 0 {
		fmt.Fprintf(w, "imports: %s\n", strings.Join(shortAll(g, imps), ", "))
	}
	if by := g.In(n.ID, graph.EdgeImports); len(by) > 0 {
		fmt.Fprintf(w, "imported by: %s\n", strings.Join(shortAll(g, by), ", "))
	}
	tests := 0
	defer func() {
		if tests > 0 {
			fmt.Fprintf(w, "\n(%d symbols from _test.go hidden; they still show up in impls, callers and path)\n", tests)
		}
	}()
	for _, c := range g.Children(n) {
		if c.Test {
			tests++
			continue
		}
		if opt.ExportedOnly && !c.Exported {
			continue
		}
		fmt.Fprintln(w)
		writeDoc(w, "", c.Doc, true)
		fmt.Fprintf(w, "%s  [%s]\n", c.Signature, loc(g, c))
		if c.Kind != graph.KindType && c.Kind != graph.KindInterface {
			continue // a func's children are closures, shown via show/path
		}
		for _, m := range g.Children(c) {
			if opt.ExportedOnly && !m.Exported {
				continue
			}
			fmt.Fprintf(w, "    %s\n", m.Signature)
		}
	}
}

func showType(w io.Writer, g *graph.Graph, n *graph.Node, opt Options) {
	fmt.Fprintf(w, "%s  [%s]\n", n.Signature, loc(g, n))
	writeDoc(w, "", n.Doc, false)
	if len(n.Fields) > 0 {
		label := "fields"
		if n.Kind == graph.KindInterface {
			label = "embeds"
		}
		fmt.Fprintf(w, "%s:\n", label)
		for _, f := range n.Fields {
			fmt.Fprintf(w, "    %s\n", f)
		}
	}
	if ms := g.Children(n); len(ms) > 0 {
		fmt.Fprintln(w, "methods:")
		for _, m := range ms {
			if opt.ExportedOnly && !m.Exported {
				continue
			}
			writeDoc(w, "    ", m.Doc, true)
			fmt.Fprintf(w, "    %s\n", m.Signature)
		}
	}
	writeList(w, g, "implements", g.Out(n.ID, graph.EdgeImplements))
	writeList(w, g, "implemented by", g.In(n.ID, graph.EdgeImplements))
	if opt.Body && n.Source != "" {
		fmt.Fprintf(w, "\n%s\n", n.Source)
	}
}

func showFunc(w io.Writer, g *graph.Graph, n *graph.Node, opt Options) {
	fmt.Fprintf(w, "%s  [%s]\n", n.Signature, loc(g, n))
	writeDoc(w, "", n.Doc, false)
	var internal, external []string
	for _, id := range g.Out(n.ID, graph.EdgeCalls) {
		if g.Nodes[id] != nil {
			internal = append(internal, id)
		} else {
			external = append(external, id)
		}
	}
	writeList(w, g, "calls", internal)
	writeList(w, g, "external", external)
	writeList(w, g, "called by", g.In(n.ID, graph.EdgeCalls))
	writeList(w, g, "dispatches to", g.DispatchTargets(n.ID))
	writeList(w, g, "reached via interface", g.InterfaceMethods(n.ID))
	writeAsync(w, g, "hands off to (async)", g.OutEdges(n.ID, graph.EdgeAsync), false)
	writeAsync(w, g, "receives from (async)", g.InEdges(n.ID, graph.EdgeAsync), true)
	if opt.Body && n.Source != "" {
		fmt.Fprintf(w, "\n%s\n", n.Source)
	}
}

// Legend explains hop markers; printed under trees and paths that use them.
const Legend = "(→ call, ⇢ through an interface, ⇝ indirect hand-off: channel, keyed handler registry or callback)"

// marker returns the arrow for a hop and, for async hops, what it goes through.
func marker(s graph.Step) (arrow, via string) {
	switch {
	case s.Via == "":
		return "→ ", ""
	case s.Via == graph.ViaInterface:
		return "⇢ ", ""
	default:
		return "⇝ ", "  (via " + s.Via + ")"
	}
}

// Tree prints callees (or callers) of n down to depth, following interface
// dispatch and async hand-offs. Repeated nodes are printed once and marked so
// cycles and diamonds don't blow up the output.
func Tree(w io.Writer, g *graph.Graph, n *graph.Node, callers bool, depth int) {
	seen := map[string]bool{}
	special := false
	var walk func(s graph.Step, level int)
	walk = func(s graph.Step, level int) {
		indent := strings.Repeat("  ", level)
		via := ""
		if s.Via != "" {
			var arrow string
			arrow, via = marker(s)
			indent += arrow
			special = true
		}
		node := g.Nodes[s.ID]
		switch {
		case node == nil:
			fmt.Fprintf(w, "%s%s  [ext]\n", indent, s.ID)
			return
		case seen[s.ID]:
			fmt.Fprintf(w, "%s%s  (shown above)%s\n", indent, g.ShortID(s.ID), via)
			return
		}
		seen[s.ID] = true
		fmt.Fprintf(w, "%s%s  %s%s\n", indent, g.ShortID(s.ID), node.Signature, via)
		if level >= depth {
			return
		}
		next := g.Next(s.ID)
		if callers {
			next = g.Prev(s.ID)
		}
		for _, c := range next {
			walk(c, level+1)
		}
	}
	walk(graph.Step{ID: n.ID}, 0)
	if special {
		fmt.Fprintln(w, Legend)
	}
}

// Path prints a shortest call chain between two functions, optionally with
// every function's source, so a whole flow can be read in one go. If there is
// no chain from -> to, the reverse direction is tried.
func Path(w io.Writer, g *graph.Graph, from, to *graph.Node, body bool) {
	steps := g.Path(from.ID, to.ID)
	if steps == nil {
		if steps = g.Path(to.ID, from.ID); steps != nil {
			fmt.Fprintf(w, "no call chain from %s to %s; showing the reverse direction\n\n",
				g.ShortID(from.ID), g.ShortID(to.ID))
		}
	}
	if steps == nil {
		fmt.Fprintf(w, "no call chain between %s and %s in either direction\n", g.ShortID(from.ID), g.ShortID(to.ID))
		return
	}
	for i, s := range steps {
		n := g.Nodes[s.ID]
		arrow, via := marker(s)
		if i == 0 {
			arrow, via = "", ""
		}
		fmt.Fprintf(w, "%d. %s%s  %s  [%s]%s\n", i+1, arrow, g.ShortID(s.ID), n.Signature, loc(g, n), via)
		if body && n.Source != "" {
			fmt.Fprintf(w, "\n%s\n\n", n.Source)
		}
	}
	fmt.Fprintln(w, Legend)
}

func writeAsync(w io.Writer, g *graph.Graph, label string, edges []graph.Edge, incoming bool) {
	if len(edges) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, e := range edges {
		id := e.To
		if incoming {
			id = e.From
		}
		fmt.Fprintf(w, "    %s  %s  (via %s)\n", g.ShortID(id), g.Nodes[id].Signature, e.Via)
	}
}

// Impls shows the interface side for a type, or implementers for an interface.
func Impls(w io.Writer, g *graph.Graph, n *graph.Node) {
	fmt.Fprintf(w, "%s\n", n.Signature)
	writeList(w, g, "implements", g.Out(n.ID, graph.EdgeImplements))
	writeList(w, g, "implemented by", g.In(n.ID, graph.EdgeImplements))
}

func writeList(w io.Writer, g *graph.Graph, label string, ids []string) {
	if len(ids) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, id := range ids {
		if n := g.Nodes[id]; n != nil && n.Kind != graph.KindPackage {
			fmt.Fprintf(w, "    %s  %s\n", g.ShortID(id), n.Signature)
		} else {
			fmt.Fprintf(w, "    %s\n", g.ShortID(id))
		}
	}
}

// writeDoc prints the full doc, or just its first sentence in brief mode.
func writeDoc(w io.Writer, indent, doc string, brief bool) {
	if doc == "" {
		return
	}
	if brief {
		doc = firstSentence(doc)
	}
	for _, line := range strings.Split(doc, "\n") {
		fmt.Fprintf(w, "%s// %s\n", indent, line)
	}
}

func firstSentence(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}

func loc(g *graph.Graph, n *graph.Node) string {
	if !n.Pos.IsValid() {
		return "?"
	}
	file, err := filepath.Rel(g.Root, n.Pos.Filename)
	if err != nil {
		file = n.Pos.Filename
	}
	return fmt.Sprintf("%s:%d", file, n.Pos.Line)
}

func shortAll(g *graph.Graph, ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = g.ShortID(id)
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
