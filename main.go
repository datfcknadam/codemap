// Command codemap shows a Go codebase as a zoomable graph: packages, then
// signatures, then call/implementation edges, then implementation source.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/datfcknadam/codemap/internal/graph"
	"github.com/datfcknadam/codemap/internal/server"
	"github.com/datfcknadam/codemap/internal/view"
	"github.com/datfcknadam/codemap/internal/web"
)

const usageText = `usage: codemap [-dir DIR] <command> [flags] [symbol]

commands:
  map                 packages, their docs and internal imports
  show <symbol>       one level down: a package's signatures, a type's
                      fields/methods/interfaces, a func's calls and callers
  callees <symbol>    call tree going down
  callers <symbol>    call tree going up
  impls <symbol>      interfaces a type satisfies, or types implementing one
  path <from> <to>    shortest call chain, through interfaces; -body adds sources
  serve               MCP server over stdio exposing the commands above
  web                 interactive zoomable graph in the browser (-addr)

flags:
  -exported           only exported symbols
  -body               include implementation source (show)
  -depth N            tree depth for callers/callees (default 2)
  -prefix PATH        map: only packages under PATH
  -imports            map: list internal imports
  -addr HOST:PORT     web: listen address (default 127.0.0.1:7777)

symbols are full IDs or any suffix: "graph.Load", "Graph.AddEdge", "Load".
`

func main() {
	dir := flag.String("dir", ".", "project directory to analyze")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*dir, flag.Arg(0), flag.Args()[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "codemap:", err)
		os.Exit(1)
	}
}

func run(dir, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	exported := fs.Bool("exported", false, "")
	body := fs.Bool("body", false, "")
	depth := fs.Int("depth", 2, "")
	imports := fs.Bool("imports", false, "")
	prefix := fs.String("prefix", "", "")
	addr := fs.String("addr", "127.0.0.1:7777", "")
	fs.Parse(args)
	// Allow flags after the symbol: "show Load -body".
	var query, query2 string
	if fs.NArg() > 0 {
		query = fs.Arg(0)
		fs.Parse(fs.Args()[1:])
	}
	if cmd == "path" && fs.NArg() > 0 {
		query2 = fs.Arg(0)
		fs.Parse(fs.Args()[1:])
	}

	if cmd == "serve" {
		return server.New(dir).Run(context.Background())
	}
	if cmd == "web" {
		srv := web.New(dir)
		fmt.Fprintln(os.Stderr, "codemap: loading packages…")
		if err := srv.Warm(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "codemap: open http://%s\n", *addr)
		return http.ListenAndServe(*addr, srv.Handler())
	}

	g, err := graph.Load(dir)
	if err != nil {
		return err
	}
	if len(g.Warnings) > 0 {
		fmt.Fprintf(os.Stderr, "codemap: %d package errors, graph may be incomplete (first: %s)\n",
			len(g.Warnings), g.Warnings[0])
	}
	opt := view.Options{ExportedOnly: *exported, Body: *body, Imports: *imports, Prefix: *prefix}
	w := os.Stdout

	if cmd == "map" {
		view.Map(w, g, opt)
		return nil
	}
	if query == "" {
		return fmt.Errorf("%s needs a symbol", cmd)
	}
	n, err := g.Resolve(query)
	if err != nil {
		return err
	}
	switch cmd {
	case "show":
		view.Show(w, g, n, opt)
	case "callees":
		view.Tree(w, g, n, false, *depth)
	case "callers":
		view.Tree(w, g, n, true, *depth)
	case "impls":
		view.Impls(w, g, n)
	case "path":
		if query2 == "" {
			return fmt.Errorf("path needs two symbols: <from> <to>")
		}
		to, err := g.Resolve(query2)
		if err != nil {
			return err
		}
		view.Path(w, g, n, to, *body)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}
