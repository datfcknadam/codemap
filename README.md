# codemap

> **Status: beta (v0.1.0-beta).** It works on real codebases, but output formats, flags and MCP tool
> names may still change. See [Known issues](#known-issues) and [Benchmarks](#benchmarks--when-it-helps)
> before you rely on it.

codemap is a zoomable graph of a Go codebase, built mainly for AI coding agents (it also has a CLI and a web UI for humans).
Instead of grepping and reading whole files, the agent moves down through levels of detail:

```
map                → packages, doc lines, sizes
show <package>     → type and func signatures, no bodies
show <type|func>   → fields/methods, callers, callees, interface implementations
show -body / path  → source, or a whole call chain with sources
```

Edges follow static calls, **interface dispatch** (`⇢`), and the indirect hand-offs where static
analysis usually stops (`⇝`):

- **channels**: a send on `chan T` links to every receiver of `chan T` (named local types only);
- **key registries / event buses**: `bus.Subscribe(TopicX, h)` + `bus.Publish(TopicX, …)`, keyed by enum-like constants;
- **callbacks**: a function calling its func-typed parameter links to every function passed for it;
- **closures** are nodes too (`Owner.funcN`), since handlers and goroutine bodies are usually anonymous.

Test files are indexed, so fakes and mocks show up in `impls` and `callers`.

## Install

Requires Go 1.27+.

```sh
go install github.com/datfcknadam/codemap@latest
```

## Use with an AI agent (MCP)

```sh
# Claude Code
claude mcp add codemap -- codemap -dir /path/to/your/go/repo serve
```

Any MCP client works: the server speaks stdio and exposes `map`, `show`, `callers`, `callees`,
`impls` and `path`. The graph is reloaded when files change, so answers include the agent's own edits.

Tip: agents default to grep/Read. If you want them to actually use codemap for flow questions, add a
line to your `CLAUDE.md` / `AGENTS.md`, e.g. *"For cross-package or async flows use codemap `path`
before grepping."*

## CLI

```sh
codemap -dir . map -prefix internal/
codemap -dir . show game.Coordinator
codemap -dir . callers Coordinator.ClaimSector -depth 3
codemap -dir . impls Store
codemap -dir . path HandleWS SaveResult -body
```

Symbols are full IDs or any unique suffix: `graph.Load`, `Graph.AddEdge`, `Load`.

## Web UI

```sh
codemap -dir . web        # http://127.0.0.1:7777
```

Directory scope → package cards with signatures → function view with callers, source and callees.
All JS is embedded, so it works offline.

## Benchmarks: when it helps

These are small, honest numbers: headless `claude -p`, with and without codemap, n=3 per task, on two private
Go backends. All answers were correct in both modes.

| Task type | Cost with codemap | Notes |
|---|---|---|
| Orientation / "what does X do" / simple impact | ±15% (noise) | grep + targeted Read is already efficient |
| Async flows (tasks the async edges were tuned on) | −8…9%, 10–17% faster | more complete chains |
| Held-out: websocket flow across packages | **−35%** | one `path` replaced many grep/Read hops |
| Held-out: interface-change impact | ≈ same, slower | |
| Held-out: SMS receipt flow | ≈ same | agent mostly ignored codemap |

**Why isn't it always cheaper if the graph is a compressed view?** Agents still have to read function
bodies to answer most questions, so the graph adds steps instead of replacing reads. Cost grows
with the number of turns, because the whole context is resent each turn. And `grep -n` + reading a line range is
already very cheap. codemap saves tokens when it **cuts the number of hops**: long cross-package,
interface and async chains, where one `path` call replaces 6–10 grep/Read rounds.

## Known issues

- **Blind spots in async edges:**
  - callbacks stored in struct fields (`s.OnFlush = f` … `s.OnFlush()`);
  - timers (`time.AfterFunc`, `time.NewTimer` + select);
  - channels of unnamed types (`chan []byte`, `chan error`, `chan struct{}`) are skipped on purpose to avoid
    everything-to-everything links;
  - reflection, `go:linkname`, plugin/RPC boundaries.
- Rendezvous with too many pairs are dropped (fan-out limit), so very generic buses may show no `⇝` edges.
- Packages that fail to type-check are loaded partially. You'll see a warning and the graph may be incomplete.
- Large monorepos: the first load takes a few seconds and memory grows with the repo (no on-disk cache yet).
- Go only.
- Agents often don't call codemap unless told to (see the tip above).
- No tests yet for `graph` and `view` packages.

Issues and PRs welcome, especially minimal repros of missed edges.

## License

MIT. The bundled `cytoscape.js`, `dagre` and `cytoscape-dagre` are MIT-licensed by their authors.
