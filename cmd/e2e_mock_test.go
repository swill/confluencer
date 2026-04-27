package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
)

// mockConfluence is an in-memory stand-in for the Confluence REST API,
// enough to exercise the cmd-package orchestration code under realistic
// flows (init / push / pull / merge cycles).
//
// Coverage:
//
//   GET  /rest/api/content/{id}                       → page detail
//   GET  /rest/api/content/{id}/child/page            → direct children
//   GET  /rest/api/content/{id}/child/attachment      → attachments list
//   POST /rest/api/content                            → create page
//   PUT  /rest/api/content/{id}                       → update page
//   DELETE /rest/api/content/{id}                     → delete page
//
// Things the mock intentionally does NOT do: pagination cursors (we always
// return Size=results-len with empty next), child-of-child cascade-delete
// (we delete recursively in handleDelete instead), attachment download
// content (returns empty bytes).
type mockConfluence struct {
	mu       sync.Mutex
	server   *httptest.Server
	spaceKey string

	pages  map[string]*mockPage
	nextID int
}

type mockPage struct {
	ID       string
	Title    string
	ParentID string // "" for the root
	Body     string
	Version  int
}

// newMockConfluence returns a server pre-populated with a tree of pages.
// `tree` is a slice of (id, parent_id, title, body) triples; the first must
// be the root. Page IDs in the tree should all be numeric strings (the
// auto-assigned IDs for new POSTs continue from max+1).
func newMockConfluence(spaceKey string, tree [][4]string) *mockConfluence {
	m := &mockConfluence{
		spaceKey: spaceKey,
		pages:    make(map[string]*mockPage),
	}
	maxID := 0
	for _, t := range tree {
		m.pages[t[0]] = &mockPage{
			ID: t[0], ParentID: t[1], Title: t[2], Body: t[3], Version: 1,
		}
		if n, err := strconv.Atoi(t[0]); err == nil && n > maxID {
			maxID = n
		}
	}
	m.nextID = maxID + 1
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

func (m *mockConfluence) URL() string {
	return m.server.URL
}

func (m *mockConfluence) Close() {
	m.server.Close()
}

// PageByID returns a copy of the named page, or nil if absent. Tests use
// this to assert post-condition state ("page X has body Y").
func (m *mockConfluence) PageByID(id string) *mockPage {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pages[id]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// AllPages returns a snapshot of every page, keyed by ID.
func (m *mockConfluence) AllPages() map[string]mockPage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]mockPage, len(m.pages))
	for k, v := range m.pages {
		out[k] = *v
	}
	return out
}

func (m *mockConfluence) handle(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/child/page"):
		m.handleChildren(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/child/attachment"):
		m.handleAttachments(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/rest/api/content/"):
		m.handleGetPage(w, r)
	case r.Method == http.MethodPost && p == "/rest/api/content":
		m.handleCreate(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/rest/api/content/"):
		m.handleUpdate(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/rest/api/content/"):
		m.handleDelete(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *mockConfluence) handleGetPage(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	m.mu.Lock()
	p, ok := m.pages[id]
	m.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	resp := m.pageJSON(p)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (m *mockConfluence) handleChildren(w http.ResponseWriter, r *http.Request) {
	// .../content/{parentID}/child/page
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/rest/api/content/"), "/")
	if len(parts) < 1 {
		http.NotFound(w, r)
		return
	}
	parentID := parts[0]

	m.mu.Lock()
	var children []*mockPage
	for _, p := range m.pages {
		if p.ParentID == parentID {
			children = append(children, p)
		}
	}
	m.mu.Unlock()

	out := struct {
		Results []map[string]any `json:"results"`
		Size    int              `json:"size"`
		Links   struct {
			Next string `json:"next"`
		} `json:"_links"`
	}{
		Results: make([]map[string]any, 0, len(children)),
		Size:    len(children),
	}
	for _, c := range children {
		out.Results = append(out.Results, m.pageJSON(c))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (m *mockConfluence) handleAttachments(w http.ResponseWriter, r *http.Request) {
	// We don't model attachments yet; return empty.
	out := struct {
		Results []any `json:"results"`
		Size    int   `json:"size"`
		Links   struct {
			Next string `json:"next"`
		} `json:"_links"`
	}{Results: []any{}, Size: 0}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (m *mockConfluence) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title     string `json:"title"`
		Ancestors []struct {
			ID string `json:"id"`
		} `json:"ancestors"`
		Body struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	parentID := ""
	if len(body.Ancestors) > 0 {
		parentID = body.Ancestors[len(body.Ancestors)-1].ID
	}

	m.mu.Lock()
	id := strconv.Itoa(m.nextID)
	m.nextID++
	page := &mockPage{
		ID: id, ParentID: parentID, Title: body.Title,
		Body: body.Body.Storage.Value, Version: 1,
	}
	m.pages[id] = page
	resp := m.pageJSON(page)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func (m *mockConfluence) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	var body struct {
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
		Title     string `json:"title"`
		Ancestors []struct {
			ID string `json:"id"`
		} `json:"ancestors,omitempty"`
		Body struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Confluence requires version to be exactly current+1.
	if body.Version.Number != p.Version+1 {
		http.Error(w, fmt.Sprintf("version conflict: expected %d, got %d", p.Version+1, body.Version.Number), http.StatusConflict)
		return
	}
	p.Title = body.Title
	p.Body = body.Body.Storage.Value
	p.Version = body.Version.Number
	if len(body.Ancestors) > 0 {
		p.ParentID = body.Ancestors[len(body.Ancestors)-1].ID
	}
	w.WriteHeader(http.StatusOK)
}

func (m *mockConfluence) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pages[id]; !ok {
		http.NotFound(w, r)
		return
	}
	// Cascade: delete the target and all descendants.
	m.deleteRecursive(id)
	w.WriteHeader(http.StatusNoContent)
}

func (m *mockConfluence) deleteRecursive(id string) {
	for cid, p := range m.pages {
		if p.ParentID == id {
			m.deleteRecursive(cid)
		}
	}
	delete(m.pages, id)
}

// pageJSON produces the response shape that api/content.go's pageResponse
// decodes — including a synthetic ancestors array (just the direct parent,
// since that's all the client uses).
func (m *mockConfluence) pageJSON(p *mockPage) map[string]any {
	ancestors := []map[string]any{}
	if p.ParentID != "" {
		ancestors = append(ancestors, map[string]any{"id": p.ParentID})
	}
	return map[string]any{
		"id":    p.ID,
		"type":  "page",
		"title": p.Title,
		"version": map[string]any{
			"number": p.Version,
		},
		"space": map[string]any{
			"key": m.spaceKey,
		},
		"ancestors": ancestors,
		"body": map[string]any{
			"storage": map[string]any{
				"value": p.Body,
			},
		},
	}
}
