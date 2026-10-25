package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var fixture = map[string]string{
	"go.mod": "module shop\n\ngo 1.22\n",
	"shop.go": `package shop

import "shop/store"

// Checkout saves the order.
func Checkout(s store.Store, id int) error { return s.Save(id) }

func Handle(id int) error { return Checkout(&store.Mem{}, id) }
`,
	"store/store.go": `package store

// Store persists orders.
type Store interface{ Save(id int) error }

type Mem struct{ ids []int }

func (m *Mem) Save(id int) error { m.ids = append(m.ids, id); return nil }
`,
	"store/store_test.go": `package store

type fake struct{}

func (fake) Save(int) error { return nil }
`,
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	for name, content := range fixture {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(New(dir).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get[T any](t *testing.T, ts *httptest.Server, path string, params url.Values) T {
	t.Helper()
	res, err := http.Get(ts.URL + path + "?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d", path, res.StatusCode)
	}
	var v T
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestScope(t *testing.T) {
	ts := newTestServer(t)
	s := get[scopeResp](t, ts, "/api/scope", nil)
	if s.Module != "shop" || len(s.Nodes) != 2 {
		t.Fatalf("want root package and store, got %+v", s.Nodes)
	}
	if len(s.Edges) != 1 || s.Edges[0].From != "" || s.Edges[0].To != "store" {
		t.Errorf("want import edge root -> store, got %+v", s.Edges)
	}
}

func TestPackage(t *testing.T) {
	ts := newTestServer(t)
	p := get[packageResp](t, ts, "/api/package", url.Values{"id": {"shop/store"}})
	names := map[string]bool{}
	for _, c := range p.Cards {
		names[c.Name] = true
	}
	if !names["Store"] || !names["Mem"] || names["fake"] {
		t.Errorf("cards without tests: got %v", names)
	}
	var dispatch bool
	for _, e := range p.Edges {
		dispatch = dispatch || (e.From == "shop/store.Store" && e.To == "shop/store.Mem" && e.Kind == "interface")
	}
	if !dispatch {
		t.Errorf("want interface edge Store -> Mem, got %+v", p.Edges)
	}

	root := get[packageResp](t, ts, "/api/package", url.Values{"id": {"shop"}})
	if len(root.Externals) != 1 || root.Externals[0].ID != "shop/store" {
		t.Errorf("calls into store should fold into one external node, got %+v", root.Externals)
	}

	withTests := get[packageResp](t, ts, "/api/package", url.Values{"id": {"shop/store"}, "tests": {"1"}})
	if len(withTests.Cards) != len(p.Cards)+1 {
		t.Errorf("tests=1 should add the fake card")
	}
}

func TestNode(t *testing.T) {
	ts := newTestServer(t)
	n := get[nodeResp](t, ts, "/api/node", url.Values{"id": {"shop/store.Store.Save"}})
	if len(n.Callers) != 1 || n.Callers[0].ID != "shop.Checkout" {
		t.Errorf("callers: %+v", n.Callers)
	}
	if len(n.Callees) != 1 || n.Callees[0].ID != "shop/store.Mem.Save" || n.Callees[0].Via != "interface" {
		t.Errorf("callees should dispatch to Mem.Save: %+v", n.Callees)
	}
	m := get[nodeResp](t, ts, "/api/node", url.Values{"id": {"shop/store.Mem.Save"}})
	if !strings.Contains(m.Source, "append(m.ids, id)") || m.Parent == nil || m.Parent.ID != "shop/store.Mem" {
		t.Errorf("method view: source %q parent %+v", m.Source, m.Parent)
	}
}

func TestSearch(t *testing.T) {
	ts := newTestServer(t)
	hits := get[[]hit](t, ts, "/api/search", url.Values{"q": {"save"}})
	if len(hits) < 2 {
		t.Fatalf("want Store.Save and Mem.Save, got %+v", hits)
	}
	for _, h := range hits {
		if strings.Contains(h.ID, "fake") {
			t.Errorf("test symbols should be hidden by default: %s", h.ID)
		}
	}
}

func TestStatic(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/", "/app.js", "/vendor/cytoscape.min.js"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, res.StatusCode)
		}
	}
}
