// Package server exposes the code graph to AI agents over MCP. The graph is
// a live.Graph, so answers always reflect the code on disk, including the
// agent's own edits.
package server

import (
	"bytes"
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/datfcknadam/codemap/internal/graph"
	"github.com/datfcknadam/codemap/internal/live"
	"github.com/datfcknadam/codemap/internal/view"
)

const instructions = `codemap is a zoomable map of this Go codebase. Prefer it over grep and
reading whole files when you need to understand structure:
map → show <package> (signatures only) → show <type|func> (edges, docs) → show -body (source).
callers/callees answer "who uses this" and "what does this touch"; impls follows interfaces.
To read a flow end to end, use path from=<entry> to=<sink> body=true instead of many show calls.
Paths cross interfaces (⇢) and indirect hand-offs (⇝): channels, event buses / handler registries
keyed by enum constants, and callbacks passed as parameters; each ⇝ hop says what it went through.
Closures are nodes too, named Owner.funcN.
Test files are indexed: fakes and mocks appear in impls and callers.
Symbols are full IDs or any suffix: "game.Coordinator", "Coordinator.ClaimSector", "ClaimSector".`

type Server struct {
	live *live.Graph
}

func New(dir string) *Server {
	return &Server{live: live.New(dir)}
}

// Run serves MCP over stdio until the client disconnects.
func (s *Server) Run(ctx context.Context) error {
	go s.live.Get() // warm up while the client initializes; errors resurface on first call
	return s.mcpServer().Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) mcpServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "codemap", Version: "v0.1.0-beta"},
		&mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "map",
		Description: "Top level of the codebase: every package with its doc line and size; generated packages are collapsed. " +
			"Start here. In large repos narrow with prefix (a directory subtree); imports=true adds internal dependencies.",
	}, s.handleMap)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "show",
		Description: "Zoom into one symbol at its natural level of detail. Package: all type and func signatures, no bodies. " +
			"Type: fields, methods, interfaces it implements or implementers. Func/method: signature, doc, what it calls " +
			"(internal and external), who calls it, and for interface methods the concrete implementations. Set body=true for source.",
	}, s.handleShow)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "callees",
		Description: "Call tree going down from a function: everything it calls, transitively, up to depth. External calls are leaves marked [ext].",
	}, s.handleTree(false))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "callers",
		Description: "Call tree going up from a function: everything that calls it, transitively, up to depth. Use before changing a signature.",
	}, s.handleTree(true))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "impls",
		Description: "For an interface: the concrete types implementing it. For a type: the interfaces it satisfies.",
	}, s.handleImpls)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "path",
		Description: "Shortest call chain between two functions, following interface dispatch (⇢) to concrete implementations. " +
			"With body=true returns every function's source along the chain: the cheapest way to read a whole flow, " +
			"e.g. from an HTTP/WS handler down to the DB write. Tries the reverse direction if there is no chain from -> to.",
	}, s.handlePath)
	return srv
}

type mapIn struct {
	Prefix   string `json:"prefix,omitempty" jsonschema:"only packages under this path, e.g. internal/modules/admin"`
	Imports  bool   `json:"imports,omitempty" jsonschema:"list internal imports of every package"`
	Exported bool   `json:"exported,omitempty" jsonschema:"count only exported symbols"`
}

type showIn struct {
	Symbol   string `json:"symbol" jsonschema:"full ID or suffix, e.g. modules/game or Coordinator.ClaimSector"`
	Body     bool   `json:"body,omitempty" jsonschema:"include implementation source"`
	Exported bool   `json:"exported,omitempty" jsonschema:"hide unexported members"`
}

type treeIn struct {
	Symbol string `json:"symbol" jsonschema:"full ID or suffix of a function or method"`
	Depth  int    `json:"depth,omitempty" jsonschema:"levels to expand, default 2"`
}

type pathIn struct {
	From string `json:"from" jsonschema:"start function: full ID or suffix"`
	To   string `json:"to" jsonschema:"end function: full ID or suffix"`
	Body bool   `json:"body,omitempty" jsonschema:"include the source of every function on the chain"`
}

type symbolIn struct {
	Symbol string `json:"symbol" jsonschema:"full ID or suffix of a type or interface"`
}

func (s *Server) handleMap(_ context.Context, _ *mcp.CallToolRequest, in mapIn) (*mcp.CallToolResult, any, error) {
	return s.render(func(g *graph.Graph, w *bytes.Buffer) error {
		view.Map(w, g, view.Options{ExportedOnly: in.Exported, Imports: in.Imports, Prefix: in.Prefix})
		return nil
	})
}

func (s *Server) handleShow(_ context.Context, _ *mcp.CallToolRequest, in showIn) (*mcp.CallToolResult, any, error) {
	return s.render(func(g *graph.Graph, w *bytes.Buffer) error {
		n, err := g.Resolve(in.Symbol)
		if err != nil {
			return err
		}
		view.Show(w, g, n, view.Options{Body: in.Body, ExportedOnly: in.Exported})
		return nil
	})
}

func (s *Server) handleTree(callers bool) mcp.ToolHandlerFor[treeIn, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in treeIn) (*mcp.CallToolResult, any, error) {
		return s.render(func(g *graph.Graph, w *bytes.Buffer) error {
			n, err := g.Resolve(in.Symbol)
			if err != nil {
				return err
			}
			depth := in.Depth
			if depth <= 0 {
				depth = 2
			}
			view.Tree(w, g, n, callers, depth)
			return nil
		})
	}
}

func (s *Server) handleImpls(_ context.Context, _ *mcp.CallToolRequest, in symbolIn) (*mcp.CallToolResult, any, error) {
	return s.render(func(g *graph.Graph, w *bytes.Buffer) error {
		n, err := g.Resolve(in.Symbol)
		if err != nil {
			return err
		}
		view.Impls(w, g, n)
		return nil
	})
}

func (s *Server) handlePath(_ context.Context, _ *mcp.CallToolRequest, in pathIn) (*mcp.CallToolResult, any, error) {
	return s.render(func(g *graph.Graph, w *bytes.Buffer) error {
		from, err := g.Resolve(in.From)
		if err != nil {
			return err
		}
		to, err := g.Resolve(in.To)
		if err != nil {
			return err
		}
		view.Path(w, g, from, to, in.Body)
		return nil
	})
}

// render runs fn against a fresh graph; errors become tool errors the agent
// can read and act on (e.g. pick one of several ambiguous symbols).
func (s *Server) render(fn func(*graph.Graph, *bytes.Buffer) error) (*mcp.CallToolResult, any, error) {
	g, err := s.live.Get()
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := fn(g, &buf); err != nil {
		return nil, nil, err
	}
	if len(g.Warnings) > 0 {
		fmt.Fprintf(&buf, "\n(warning: %d package errors, graph may be incomplete; first: %s)\n",
			len(g.Warnings), g.Warnings[0])
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: buf.String()}}}, nil, nil
}
