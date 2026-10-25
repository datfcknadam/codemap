package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const fixture = `package shop

// Store persists orders.
type Store interface {
	Save(id int) error
}

type memStore struct{ ids []int }

func (m *memStore) Save(id int) error {
	m.ids = append(m.ids, id)
	return nil
}

// Checkout saves the order.
func Checkout(s Store, id int) error {
	return s.Save(id)
}

func Handle(id int) error {
	return Checkout(&memStore{}, id)
}
`

const fixtureTest = `package shop

type fakeStore struct{}

func (fakeStore) Save(int) error { return nil }
`

func TestTools(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module shop\n\ngo 1.22\n")
	write(t, dir, "shop.go", fixture)
	write(t, dir, "shop_test.go", fixtureTest)
	cs := connect(t, dir)

	cases := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"map", nil, []string{"module shop", "shop  (2 types, 2 funcs)"}},
		{"show", map[string]any{"symbol": "shop"}, []string{"1 symbols from _test.go hidden"}},
		{"impls", map[string]any{"symbol": "Store"}, []string{"shop.memStore", "shop.fakeStore"}},
		{"path", map[string]any{"from": "Handle", "to": "memStore.Save"}, []string{
			"1. shop.Handle", "2. → shop.Checkout", "3. → shop.Store.Save", "4. ⇢ shop.memStore.Save"}},
		{"path", map[string]any{"from": "memStore.Save", "to": "Handle", "body": true}, []string{
			"showing the reverse direction", "return Checkout(&memStore{}, id)"}},
		{"callees", map[string]any{"symbol": "Checkout"}, []string{"⇢ shop.fakeStore.Save", "⇢ shop.memStore.Save"}},
		{"show", map[string]any{"symbol": "shop"}, []string{"type Store interface", "Save(id int) error", "func Checkout(s Store, id int) error"}},
		{"show", map[string]any{"symbol": "Checkout"}, []string{"calls:", "shop.Store.Save"}},
		{"show", map[string]any{"symbol": "Store.Save"}, []string{"dispatches to:", "shop.memStore.Save"}},
		{"show", map[string]any{"symbol": "memStore.Save", "body": true}, []string{"m.ids = append(m.ids, id)"}},
		{"impls", map[string]any{"symbol": "memStore"}, []string{"implements:", "shop.Store"}},
		{"callers", map[string]any{"symbol": "Store.Save"}, []string{"shop.Checkout"}},
	}
	for _, c := range cases {
		got, isErr := call(t, cs, c.tool, c.args)
		if isErr {
			t.Errorf("%s %v: unexpected tool error: %s", c.tool, c.args, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s %v: missing %q in:\n%s", c.tool, c.args, w, got)
			}
		}
	}

	// Ambiguous suffix: both Store.Save and memStore.Save end in ".Save".
	got, isErr := call(t, cs, "show", map[string]any{"symbol": "Save"})
	if !isErr || !strings.Contains(got, "ambiguous") {
		t.Errorf("ambiguous symbol: want tool error listing candidates, got (err=%v):\n%s", isErr, got)
	}
}

const asyncFixture = `package app

type Command string

const CmdPay Command = "pay"

type Bus struct{ handlers map[Command]func() }

func (b *Bus) On(c Command, h func()) { b.handlers[c] = h }
func (b *Bus) Emit(c Command)         { b.handlers[c]() }

func Register(b *Bus) {
	b.On(CmdPay, func() { charge() })
}

func charge() {}

func Pay(b *Bus) { b.Emit(CmdPay) }

type job struct{ id int }

func Enqueue(q chan job, id int) { q <- job{id} }

func Consume(q chan job, handle func(job)) {
	go func() {
		for j := range q {
			handle(j)
		}
	}()
}

func Start(q chan job) { Consume(q, process) }

func process(job) {}
`

// TestAsync checks that paths continue across a keyed handler registry and a
// channel consumed by a goroutine, where static calls alone break off.
func TestAsync(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module app\n\ngo 1.22\n")
	write(t, dir, "app.go", asyncFixture)
	cs := connect(t, dir)

	cases := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"path", map[string]any{"from": "Pay", "to": "charge"}, []string{
			"1. app.Pay", "2. ⇝ app.Register.func1", "(via key app.CmdPay)", "3. → app.charge"}},
		{"path", map[string]any{"from": "Enqueue", "to": "process"}, []string{
			"1. app.Enqueue", "2. ⇝ app.Consume.func1", "(via chan app.job)",
			"3. ⇝ app.process", "(via callback app.Consume(handle))"}},
		{"show", map[string]any{"symbol": "Enqueue"}, []string{"hands off to (async):", "app.Consume.func1"}},
		{"show", map[string]any{"symbol": "Register.func1", "body": true}, []string{"closure in Register", "charge()"}},
		{"callers", map[string]any{"symbol": "charge", "depth": 3}, []string{"⇝ app.Pay"}},
	}
	for _, c := range cases {
		got, isErr := call(t, cs, c.tool, c.args)
		if isErr {
			t.Errorf("%s %v: unexpected tool error: %s", c.tool, c.args, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s %v: missing %q in:\n%s", c.tool, c.args, w, got)
			}
		}
	}
}

// TestReload checks the agent sees its own edits without restarting the server.
func TestReload(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module shop\n\ngo 1.22\n")
	write(t, dir, "shop.go", fixture)
	cs := connect(t, dir)

	if got, _ := call(t, cs, "show", map[string]any{"symbol": "shop"}); strings.Contains(got, "Refund") {
		t.Fatalf("Refund present before it was written:\n%s", got)
	}
	write(t, dir, "refund.go", "package shop\n\nfunc Refund(id int) {}\n")
	// Ensure the new file's mtime differs even on coarse-grained filesystems.
	future := time.Now().Add(time.Second)
	os.Chtimes(filepath.Join(dir, "refund.go"), future, future)

	got, _ := call(t, cs, "show", map[string]any{"symbol": "shop"})
	if !strings.Contains(got, "func Refund(id int)") {
		t.Errorf("edit not picked up:\n%s", got)
	}
}

func connect(t *testing.T, dir string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := New(dir).mcpServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
