package server_test

// Browsing by id, which is what a grant needs to be usable.
//
// Every read route takes a `path`, and a path only ever names the caller's own tree. A folder shared
// with a second account is therefore reachable only through the share-token routes: the recipient
// is granted permissions on a node it has no way to name. That is also why a group space cannot be
// built on the current API — a member would be granted access to something it cannot address.
//
// These routes take the id, so authorization is by node and a grant is enough. The tests below check
// the three answers that matter: the owner sees its own node, a stranger gets the same 404 as an
// absent id (an id is not a capability), and a grantee can actually list what it was granted.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// nodeListing is the response shape of GET /api/nodes/{id}/children.
type nodeListing struct {
	ID       string `json:"id"`
	Children []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"children"`
	Truncated bool   `json:"truncated"`
	Cursor    string `json:"cursor"`
}

func TestNodesCanBeBrowsedByID(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "browse-me.txt", "bytes")
	root, err := h.meta.Root(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("root: %v", err)
	}

	// The node itself, by id.
	resp := h.do(http.MethodGet, "/api/nodes/"+root.ID.String(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node by id: %d (%s)", resp.StatusCode, readBody(resp))
	}
	var meta struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		IsDir bool   `json:"is_dir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if meta.ID != root.ID.String() || !meta.IsDir || meta.Kind != "dir" {
		t.Fatalf("node by id returned %+v, want the root directory", meta)
	}

	// Its children, by id.
	resp = h.do(http.MethodGet, "/api/nodes/"+root.ID.String()+"/children", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("children by id: %d (%s)", resp.StatusCode, readBody(resp))
	}
	var list nodeListing
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(list.Children) != 1 || list.Children[0].Name != "browse-me.txt" {
		t.Fatalf("children by id returned %+v, want the uploaded file", list.Children)
	}
}

// An id is not a capability: a stranger must not be able to tell an existing node from one that was
// never created, so both answer the same 404.
func TestANodeBelongingToSomeoneElseLooksAbsent(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "private.txt", "secret")
	root := h.rootNode(t)

	// An account with no grant on anything of the owner's.
	mate, _ := h.aclSetup(t)
	mateSession, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	for _, path := range []string{
		"/api/nodes/" + root.ID.String(),
		"/api/nodes/" + root.ID.String() + "/children",
		"/api/nodes/" + uuid.New().String(),
	} {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateSession})
		resp, derr := h.srv.Client().Do(req)
		if derr != nil {
			t.Fatalf("request %s: %v", path, derr)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s answered %d for a caller with no grant, want 404", path, resp.StatusCode)
		}
	}
}

// And the point of the whole thing: a grant makes a node in another account's tree reachable,
// without a path and without a share token.
func TestAGranteeCanBrowseTheNodeItWasGranted(t *testing.T) {
	h := newHarness(t)
	mate, folder := h.aclSetup(t)
	if code, _ := h.grantAs(t, folder, mate.Username, []string{"read", "list"}, nil); code != http.StatusCreated {
		t.Fatalf("seed grant: %d", code)
	}

	nodes, err := h.meta.Resolve(context.Background(), h.user.ID, folder)
	if err != nil {
		t.Fatalf("resolve %q: %v", folder, err)
	}
	folderID := nodes[len(nodes)-1].ID

	mateSession, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet,
		h.srv.URL+"/api/nodes/"+folderID.String()+"/children", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateSession})
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the grantee could not list the folder it was granted by id: %d (%s)",
			resp.StatusCode, readBody(resp))
	}
	var list nodeListing
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.ID != folderID.String() {
		t.Fatalf("listing came back for %q, want %q", list.ID, folderID)
	}
}
