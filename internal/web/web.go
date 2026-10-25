// Package web serves an interactive, zoomable view of the code graph:
// directories → packages → signatures → a function with its neighbours and
// source. Each level is aggregated server-side, so the browser only ever lays
// out the handful of nodes on screen.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/datfcknadam/codemap/internal/graph"
	"github.com/datfcknadam/codemap/internal/live"
)

//go:embed static
var static embed.FS

type Server struct {
	live *live.Graph
}

func New(dir string) *Server {
	return &Server{live: live.New(dir)}
}

// Warm builds the graph ahead of the first request.
func (s *Server) Warm() error {
	_, err := s.live.Get()
	return err
}

func (s *Server) Handler() http.Handler {
	files, _ := fs.Sub(static, "static")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServerFS(files))
	mux.HandleFunc("/api/scope", s.api(scopeView))
	mux.HandleFunc("/api/package", s.api(packageView))
	mux.HandleFunc("/api/node", s.api(nodeView))
	mux.HandleFunc("/api/search", s.api(searchView))
	return mux
}

// filters are the toggles shared by every view.
type filters struct {
	Tests     bool
	Generated bool
}

type viewFunc func(g *graph.Graph, r *http.Request, f filters) (any, int)

func (s *Server) api(fn viewFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g, err := s.live.Get()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		q := r.URL.Query()
		f := filters{Tests: q.Get("tests") == "1", Generated: q.Get("generated") == "1"}
		body, status := fn(g, r, f)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}
}

type apiError struct {
	Error string `json:"error"`
}

// ---- scope: one directory level of the module ----

type scopeNode struct {
	ID        string `json:"id"` // directory path relative to the module root
	Label     string `json:"label"`
	Kind      string `json:"kind"`              // "dir" or "pkg"
	Package   string `json:"package,omitempty"` // package ID living exactly at this path
	Packages  int    `json:"packages"`
	Types     int    `json:"types"`
	Funcs     int    `json:"funcs"`
	Doc       string `json:"doc,omitempty"`
	Generated bool   `json:"generated"`
	Outside   int    `json:"outside"` // imports leaving the current scope
}

type weightedEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Weight int    `json:"weight"`
	Kind   string `json:"kind,omitempty"` // call, interface, async (package view)
	Via    string `json:"via,omitempty"`
}

type scopeResp struct {
	Module   string         `json:"module"`
	Prefix   string         `json:"prefix"`
	Nodes    []*scopeNode   `json:"nodes"`
	Edges    []weightedEdge `json:"edges"`
	Hidden   int            `json:"hidden"` // generated packages filtered out
	Warnings []string       `json:"warnings,omitempty"`
}

func scopeView(g *graph.Graph, r *http.Request, f filters) (any, int) {
	prefix := strings.Trim(r.URL.Query().Get("prefix"), "/")
	resp := scopeResp{Module: g.Module, Prefix: prefix, Warnings: firstN(g.Warnings, 3)}
	nodes := map[string]*scopeNode{}
	keyOf := map[string]string{} // package ID -> child key under prefix

	for _, id := range g.Packages {
		p := g.Nodes[id]
		rel := relPath(g, id)
		if p.Test || !under(rel, prefix) {
			continue
		}
		if p.Generated && !f.Generated {
			resp.Hidden++
			continue
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(rel, prefix), "/")
		key, deeper := rel, false
		if seg, _, nested := strings.Cut(rest, "/"); nested {
			key, deeper = join(prefix, seg), true
		}
		n := nodes[key]
		if n == nil {
			n = &scopeNode{ID: key, Label: lastSegment(key, g.Module), Kind: "pkg"}
			nodes[key] = n
		}
		if deeper {
			n.Kind = "dir"
		} else {
			n.Package = id
			n.Doc = firstSentence(p.Doc)
		}
		n.Packages++
		n.Generated = n.Generated || p.Generated
		for _, c := range g.Children(p) {
			if c.Test {
				continue
			}
			if c.Kind == graph.KindFunc {
				n.Funcs++
			} else {
				n.Types++
			}
		}
		keyOf[id] = key
	}

	weights := map[[2]string]int{}
	for id, from := range keyOf {
		for _, imp := range g.Out(id, graph.EdgeImports) {
			to, ok := keyOf[imp]
			switch {
			case !ok:
				if !under(relPath(g, imp), prefix) {
					nodes[from].Outside++
				}
			case to != from:
				weights[[2]string{from, to}]++
			}
		}
	}
	for _, n := range nodes {
		resp.Nodes = append(resp.Nodes, n)
	}
	sort.Slice(resp.Nodes, func(i, j int) bool { return resp.Nodes[i].ID < resp.Nodes[j].ID })
	resp.Edges = sortedEdges(weights)
	return resp, http.StatusOK
}

// ---- package: signatures as cards, calls between them ----

type member struct {
	ID        string `json:"id"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
}

type card struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Signature string   `json:"signature"`
	Doc       string   `json:"doc,omitempty"`
	Fields    []string `json:"fields,omitempty"`
	Members   []member `json:"members,omitempty"`
	Exported  bool     `json:"exported"`
	Test      bool     `json:"test"`
	Loc       string   `json:"loc"`
}

type extPkg struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Generated bool   `json:"generated"`
}

type packageResp struct {
	ID         string         `json:"id"`
	Rel        string         `json:"rel"`
	Name       string         `json:"name"`
	Doc        string         `json:"doc,omitempty"`
	Cards      []card         `json:"cards"`
	Externals  []extPkg       `json:"externals"`
	Edges      []weightedEdge `json:"edges"`
	Imports    []string       `json:"imports"`
	ImportedBy []string       `json:"importedBy"`
}

func packageView(g *graph.Graph, r *http.Request, f filters) (any, int) {
	p := g.Nodes[r.URL.Query().Get("id")]
	if p == nil || p.Kind != graph.KindPackage {
		return apiError{"no such package"}, http.StatusNotFound
	}
	resp := packageResp{ID: p.ID, Rel: relPath(g, p.ID), Name: p.Name, Doc: p.Doc,
		Imports: g.Out(p.ID, graph.EdgeImports), ImportedBy: g.In(p.ID, graph.EdgeImports)}

	for _, c := range g.Children(p) {
		if c.Test && !f.Tests {
			continue
		}
		cd := card{ID: c.ID, Kind: string(c.Kind), Name: c.Name, Signature: c.Signature,
			Doc: firstSentence(c.Doc), Fields: c.Fields, Exported: c.Exported, Test: c.Test, Loc: loc(g, c)}
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			for _, m := range g.Children(c) {
				if m.Test && !f.Tests {
					continue
				}
				cd.Members = append(cd.Members, member{ID: m.ID, Signature: m.Signature, Exported: m.Exported})
			}
		}
		resp.Cards = append(resp.Cards, cd)
	}

	type key struct{ from, to, kind, via string }
	weights := map[key]int{}
	exts := map[string]bool{}
	for _, n := range g.Nodes {
		if n.Pkg != p.ID || n.Kind == graph.KindPackage || (n.Test && !f.Tests) {
			continue
		}
		from := topLevel(g, n)
		for _, s := range g.Next(n.ID) {
			t := g.Nodes[s.ID]
			if t == nil || (t.Test && !f.Tests) {
				continue
			}
			to := topLevel(g, t)
			if t.Pkg != p.ID {
				if pkg := g.Nodes[t.Pkg]; pkg == nil || (pkg.Generated && !f.Generated) {
					continue
				}
				to = t.Pkg
				exts[to] = true
			}
			if to == from {
				continue
			}
			kind, via := "call", ""
			switch {
			case s.Via == graph.ViaInterface:
				kind = "interface"
			case s.Async():
				kind, via = "async", s.Via
			}
			weights[key{from, to, kind, via}]++
		}
	}
	for id := range exts {
		resp.Externals = append(resp.Externals, extPkg{ID: id, Label: relPath(g, id), Generated: g.Nodes[id].Generated})
	}
	sort.Slice(resp.Externals, func(i, j int) bool { return resp.Externals[i].ID < resp.Externals[j].ID })
	for k, w := range weights {
		resp.Edges = append(resp.Edges, weightedEdge{From: k.from, To: k.to, Weight: w, Kind: k.kind, Via: k.via})
	}
	sort.Slice(resp.Edges, func(i, j int) bool {
		a, b := resp.Edges[i], resp.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	return resp, http.StatusOK
}

// topLevel returns the ID of n's ancestor that is a direct child of its
// package: methods fold into their type, closures into their function.
func topLevel(g *graph.Graph, n *graph.Node) string {
	for {
		parent := g.Nodes[n.Parent]
		if parent == nil || parent.Kind == graph.KindPackage {
			return n.ID
		}
		n = parent
	}
}

// ---- node: one symbol with neighbours and source ----

type ref struct {
	ID        string `json:"id"`
	Short     string `json:"short"`
	Kind      string `json:"kind"`
	Signature string `json:"signature"`
	Via       string `json:"via,omitempty"`
	Test      bool   `json:"test"`
}

type nodeResp struct {
	ID            string   `json:"id"`
	Short         string   `json:"short"`
	Kind          string   `json:"kind"`
	Name          string   `json:"name"`
	Package       string   `json:"package"`
	Parent        *ref     `json:"parent,omitempty"`
	Signature     string   `json:"signature"`
	Doc           string   `json:"doc,omitempty"`
	Loc           string   `json:"loc"`
	Source        string   `json:"source,omitempty"`
	Fields        []string `json:"fields,omitempty"`
	Members       []ref    `json:"members,omitempty"`
	Callers       []ref    `json:"callers"`
	Callees       []ref    `json:"callees"`
	External      []string `json:"external"`
	Implements    []ref    `json:"implements"`
	ImplementedBy []ref    `json:"implementedBy"`
}

func nodeView(g *graph.Graph, r *http.Request, f filters) (any, int) {
	n := g.Nodes[r.URL.Query().Get("id")]
	if n == nil || n.Kind == graph.KindPackage {
		return apiError{"no such symbol"}, http.StatusNotFound
	}
	resp := nodeResp{ID: n.ID, Short: g.ShortID(n.ID), Kind: string(n.Kind), Name: n.Name, Package: n.Pkg,
		Signature: n.Signature, Doc: n.Doc, Loc: loc(g, n), Source: n.Source, Fields: n.Fields,
		Callers: []ref{}, Callees: []ref{}, External: []string{}, Implements: []ref{}, ImplementedBy: []ref{}}
	if p := g.Nodes[n.Parent]; p != nil && p.Kind != graph.KindPackage {
		pr := toRef(g, p.ID, "")
		resp.Parent = &pr
	}
	for _, m := range g.Children(n) {
		if m.Test && !f.Tests {
			continue
		}
		resp.Members = append(resp.Members, toRef(g, m.ID, ""))
	}
	for _, s := range g.Prev(n.ID) {
		if t := g.Nodes[s.ID]; t != nil && (f.Tests || !t.Test) {
			resp.Callers = append(resp.Callers, toRef(g, s.ID, s.Via))
		}
	}
	for _, s := range g.Next(n.ID) {
		t := g.Nodes[s.ID]
		switch {
		case t == nil:
			resp.External = append(resp.External, s.ID)
		case f.Tests || !t.Test:
			resp.Callees = append(resp.Callees, toRef(g, s.ID, s.Via))
		}
	}
	for _, id := range g.Out(n.ID, graph.EdgeImplements) {
		resp.Implements = append(resp.Implements, toRef(g, id, ""))
	}
	for _, id := range g.In(n.ID, graph.EdgeImplements) {
		if t := g.Nodes[id]; f.Tests || !t.Test {
			resp.ImplementedBy = append(resp.ImplementedBy, toRef(g, id, ""))
		}
	}
	return resp, http.StatusOK
}

// toRef describes a neighbour; IDs without a node (stdlib, error) still render.
func toRef(g *graph.Graph, id, via string) ref {
	r := ref{ID: id, Short: g.ShortID(id), Via: via}
	if n := g.Nodes[id]; n != nil {
		r.Kind, r.Signature, r.Test = string(n.Kind), n.Signature, n.Test
	}
	return r
}

// ---- search ----

type hit struct {
	ID        string `json:"id"`
	Short     string `json:"short"`
	Kind      string `json:"kind"`
	Signature string `json:"signature,omitempty"`
}

func searchView(g *graph.Graph, r *http.Request, f filters) (any, int) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	hits := []hit{}
	if q == "" {
		return hits, http.StatusOK
	}
	type scored struct {
		hit
		score int
	}
	var all []scored
	for id, n := range g.Nodes {
		if (n.Test && !f.Tests) || isClosure(id) {
			continue
		}
		if pkg := g.Nodes[n.Pkg]; pkg != nil && pkg.Generated && !f.Generated {
			continue
		}
		short := g.ShortID(id)
		name, low := strings.ToLower(n.Name), strings.ToLower(short)
		score := 0
		switch {
		case name == q:
			score = 3
		case strings.HasPrefix(name, q):
			score = 2
		case strings.Contains(low, q):
			score = 1
		default:
			continue
		}
		if n.Exported {
			score++ // exported API first among equals
		}
		all = append(all, scored{hit{ID: id, Short: short, Kind: string(n.Kind), Signature: n.Signature}, score})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		if len(all[i].Short) != len(all[j].Short) {
			return len(all[i].Short) < len(all[j].Short)
		}
		return all[i].Short < all[j].Short
	})
	for i := 0; i < len(all) && i < limit; i++ {
		hits = append(hits, all[i].hit)
	}
	return hits, http.StatusOK
}

func isClosure(id string) bool {
	i := strings.LastIndex(id, ".func")
	return i >= 0 && strings.TrimLeft(id[i+len(".func"):], "0123456789") == ""
}

// ---- helpers ----

// relPath is a package's directory relative to the module root ("" for it).
func relPath(g *graph.Graph, id string) string {
	if id == g.Module {
		return ""
	}
	if rest, ok := strings.CutPrefix(id, g.Module+"/"); ok {
		return rest
	}
	return id
}

func under(rel, prefix string) bool {
	return prefix == "" || rel == prefix || strings.HasPrefix(rel, prefix+"/")
}

func join(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "/" + seg
}

func lastSegment(path, module string) string {
	if path == "" {
		return module
	}
	return path[strings.LastIndex(path, "/")+1:]
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
		return ""
	}
	file, err := filepath.Rel(g.Root, n.Pos.Filename)
	if err != nil {
		file = n.Pos.Filename
	}
	return file + ":" + strconv.Itoa(n.Pos.Line)
}

func sortedEdges(weights map[[2]string]int) []weightedEdge {
	out := make([]weightedEdge, 0, len(weights))
	for k, w := range weights {
		out = append(out, weightedEdge{From: k[0], To: k[1], Weight: w})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
