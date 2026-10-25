// Package graph holds a hierarchical, level-of-detail model of a Go codebase:
// packages contain types and functions, types contain methods, and edges
// record calls, interface implementations and imports between them.
package graph

import (
	"fmt"
	"go/token"
	"slices"
	"sort"
	"strings"
)

type Kind string

const (
	KindPackage   Kind = "package"
	KindType      Kind = "type"
	KindInterface Kind = "interface"
	KindFunc      Kind = "func"
	KindMethod    Kind = "method"
)

type EdgeKind string

const (
	EdgeCalls      EdgeKind = "calls"
	EdgeImplements EdgeKind = "implements"
	EdgeImports    EdgeKind = "imports"
	EdgeAsync      EdgeKind = "async" // hand-off through a channel or a keyed handler registry
)

// Node is one symbol at some level of the hierarchy. Signature is the
// contract shown at the overview level; Source is revealed only on drill-down.
type Node struct {
	ID        string
	Kind      Kind
	Name      string
	Pkg       string
	Parent    string
	Children  []string
	Exported  bool
	Generated bool // package made only of "Code generated ... DO NOT EDIT" files
	Test      bool // declared in a _test.go file, or an external _test package
	Signature string
	Fields    []string // struct fields or embedded interfaces
	Doc       string
	Pos       token.Position
	Source    string
}

type Edge struct {
	From, To string
	Kind     EdgeKind
	Via      string // what an async edge rendezvous on, e.g. "chan worker.sendJob"
}

type Graph struct {
	Root     string // absolute directory the graph was loaded from
	Module   string
	Packages []string
	Nodes    map[string]*Node
	Warnings []string

	out  map[string][]Edge
	in   map[string][]Edge
	seen map[Edge]bool
}

func New() *Graph {
	return &Graph{
		Nodes: map[string]*Node{},
		out:   map[string][]Edge{},
		in:    map[string][]Edge{},
		seen:  map[Edge]bool{},
	}
}

func (g *Graph) add(n *Node) {
	g.Nodes[n.ID] = n
	if n.Kind == KindPackage {
		g.Packages = append(g.Packages, n.ID)
		return
	}
	if p := g.Nodes[n.Parent]; p != nil {
		p.Children = append(p.Children, n.ID)
	}
}

// AddEdge records a directed edge once; self-loops are kept (recursion).
func (g *Graph) AddEdge(from, to string, kind EdgeKind) {
	g.AddEdgeVia(from, to, kind, "")
}

func (g *Graph) AddEdgeVia(from, to string, kind EdgeKind, via string) {
	e := Edge{From: from, To: to, Kind: kind, Via: via}
	if g.seen[e] {
		return
	}
	g.seen[e] = true
	g.out[from] = append(g.out[from], e)
	g.in[to] = append(g.in[to], e)
}

// Out returns targets of edges of the given kind leaving id.
func (g *Graph) Out(id string, kind EdgeKind) []string {
	return filter(g.out[id], kind, func(e Edge) string { return e.To })
}

// In returns sources of edges of the given kind entering id.
func (g *Graph) In(id string, kind EdgeKind) []string {
	return filter(g.in[id], kind, func(e Edge) string { return e.From })
}

// OutEdges and InEdges return full edges of a kind, labels included.
func (g *Graph) OutEdges(id string, kind EdgeKind) []Edge { return edgesOf(g.out[id], kind) }
func (g *Graph) InEdges(id string, kind EdgeKind) []Edge  { return edgesOf(g.in[id], kind) }

func edgesOf(edges []Edge, kind EdgeKind) []Edge {
	var out []Edge
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

func filter(edges []Edge, kind EdgeKind, end func(Edge) string) []string {
	var ids []string
	for _, e := range edges {
		if e.Kind == kind {
			ids = append(ids, end(e))
		}
	}
	sort.Strings(ids)
	return ids
}

// ViaInterface labels a hop between an interface method and an implementation.
const ViaInterface = "interface"

// Step is one hop in a call chain. Via is empty for a direct call,
// ViaInterface for interface dispatch, or an async edge's label.
type Step struct {
	ID  string
	Via string
}

func (s Step) Async() bool { return s.Via != "" && s.Via != ViaInterface }

// Next returns what control can flow to from id: its calls, async hand-offs
// and, for an interface method, every concrete implementation of it.
func (g *Graph) Next(id string) []Step {
	var out []Step
	for _, to := range g.Out(id, EdgeCalls) {
		out = append(out, Step{ID: to})
	}
	for _, to := range g.DispatchTargets(id) {
		out = append(out, Step{ID: to, Via: ViaInterface})
	}
	for _, e := range g.OutEdges(id, EdgeAsync) {
		out = append(out, Step{ID: e.To, Via: e.Via})
	}
	return out
}

// Prev is Next reversed: callers of id, async producers and, for a concrete
// method, the interface methods through which it can be reached.
func (g *Graph) Prev(id string) []Step {
	var out []Step
	for _, from := range g.In(id, EdgeCalls) {
		out = append(out, Step{ID: from})
	}
	for _, from := range g.InterfaceMethods(id) {
		out = append(out, Step{ID: from, Via: ViaInterface})
	}
	for _, e := range g.InEdges(id, EdgeAsync) {
		out = append(out, Step{ID: e.From, Via: e.Via})
	}
	return out
}

// DispatchTargets lists concrete methods a call to interface method id can reach.
func (g *Graph) DispatchTargets(id string) []string {
	n := g.Nodes[id]
	if n == nil || n.Kind != KindMethod {
		return nil
	}
	parent := g.Nodes[n.Parent]
	if parent == nil || parent.Kind != KindInterface {
		return nil
	}
	var out []string
	for _, t := range g.In(parent.ID, EdgeImplements) {
		if m := t + "." + n.Name; g.Nodes[m] != nil {
			out = append(out, m)
		}
	}
	return out
}

// InterfaceMethods lists interface methods that concrete method id implements.
func (g *Graph) InterfaceMethods(id string) []string {
	n := g.Nodes[id]
	if n == nil || n.Kind != KindMethod {
		return nil
	}
	var out []string
	for _, i := range g.Out(n.Parent, EdgeImplements) {
		if m := i + "." + n.Name; g.Nodes[m] != nil {
			out = append(out, m)
		}
	}
	return out
}

// Path finds a shortest call chain from -> to, following interface dispatch.
// It returns nil when to is unreachable from from.
func (g *Graph) Path(from, to string) []Step {
	prev := map[string]Step{from: {}}
	queue := []string{from}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == to {
			// prev[x] holds x's predecessor and the label of the hop into x.
			path := []Step{{ID: to, Via: prev[to].Via}}
			for at := to; at != from; {
				at = prev[at].ID
				path = append(path, Step{ID: at, Via: prev[at].Via})
			}
			slices.Reverse(path)
			return path
		}
		for _, s := range g.Next(id) {
			if _, seen := prev[s.ID]; seen || g.Nodes[s.ID] == nil {
				continue
			}
			prev[s.ID] = Step{ID: id, Via: s.Via}
			queue = append(queue, s.ID)
		}
	}
	return nil
}

func (g *Graph) Children(n *Node) []*Node {
	out := make([]*Node, 0, len(n.Children))
	for _, id := range n.Children {
		out = append(out, g.Nodes[id])
	}
	return out
}

// Find resolves a user query: an exact ID, or any ID ending in "/query" or
// ".query", so "graph.Load" and "Load" both work.
func (g *Graph) Find(query string) []*Node {
	if n, ok := g.Nodes[query]; ok {
		return []*Node{n}
	}
	var out []*Node
	for id, n := range g.Nodes {
		if strings.HasSuffix(id, "/"+query) || strings.HasSuffix(id, "."+query) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Resolve is Find narrowed to exactly one node; an ambiguous query lists the
// candidates so the caller can retry with a longer suffix.
func (g *Graph) Resolve(query string) (*Node, error) {
	found := g.Find(query)
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no symbol matches %q", query)
	case 1:
		return found[0], nil
	}
	ids := make([]string, len(found))
	for i, n := range found {
		ids[i] = "  " + g.ShortID(n.ID)
	}
	return nil, fmt.Errorf("%q is ambiguous:\n%s", query, strings.Join(ids, "\n"))
}

// ShortID strips the module path so IDs stay compact but remain valid queries.
func (g *Graph) ShortID(id string) string {
	if g.Module != "" {
		if rest, ok := strings.CutPrefix(id, g.Module+"/"); ok {
			return rest
		}
	}
	return id
}

// finish puts children in source order so output reads like the code.
func (g *Graph) finish() {
	sort.Strings(g.Packages)
	for _, n := range g.Nodes {
		sort.SliceStable(n.Children, func(i, j int) bool {
			a, b := g.Nodes[n.Children[i]].Pos, g.Nodes[n.Children[j]].Pos
			if a.Filename != b.Filename {
				return a.Filename < b.Filename
			}
			return a.Offset < b.Offset
		})
	}
}
