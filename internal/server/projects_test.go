package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func doJSON(t *testing.T, srv *Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var got map[string]any
	if w.Body.Len() > 0 {
		_ = json.NewDecoder(w.Body).Decode(&got)
	}
	return w.Code, got
}

func insertFolderEvent(t *testing.T, s *store.Store, folder string, at time.Time, cost float64, source string) {
	t.Helper()
	id := folder + at.Format(time.RFC3339Nano)
	if _, err := s.InsertUsageEventRecord(store.UsageEventRecord{
		OccurredAt: at, Source: "tailer", SessionID: "s-" + id, MessageID: "m-" + id,
		ProjectPath: folder, Model: "claude-opus-5-5", InputTokens: 1, OutputTokens: 1,
		CostUSD: &cost, CostSource: source,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func TestProjectsAPI_Lifecycle(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()

	code, got := doJSON(t, srv, "POST", "/api/projects", map[string]string{"name": "BNA"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, got)
	}
	id := int64(got["id"].(float64))

	if code, _ := doJSON(t, srv, "POST", "/api/projects", map[string]string{"name": "bna"}); code != http.StatusConflict {
		t.Errorf("duplicate: %d, want 409", code)
	}
	if code, _ := doJSON(t, srv, "POST", "/api/projects", map[string]string{"name": " "}); code != http.StatusBadRequest {
		t.Errorf("blank name: %d, want 400", code)
	}

	path := "/api/projects/" + itoa(id)
	if code, got := doJSON(t, srv, "PATCH", path, map[string]string{"name": "BNA app"}); code != http.StatusOK {
		t.Errorf("rename: %d %v", code, got)
	}
	if code, _ := doJSON(t, srv, "PATCH", "/api/projects/9999", map[string]string{"name": "x"}); code != http.StatusNotFound {
		t.Errorf("rename missing: %d, want 404", code)
	}
	if code, _ := doJSON(t, srv, "PATCH", "/api/projects/abc", map[string]string{"name": "x"}); code != http.StatusBadRequest {
		t.Errorf("rename bad id: %d, want 400", code)
	}

	if code, got := doJSON(t, srv, "PUT", "/api/project-folders",
		map[string]any{"folder": `C:\OCIS2\BNA`, "project_id": id}); code != http.StatusOK {
		t.Errorf("assign: %d %v", code, got)
	}
	if code, _ := doJSON(t, srv, "PUT", "/api/project-folders",
		map[string]any{"folder": `C:\x`, "project_id": 9999}); code != http.StatusNotFound {
		t.Errorf("assign to missing project: %d, want 404", code)
	}
	if code, _ := doJSON(t, srv, "PUT", "/api/project-folders",
		map[string]any{"folder": "", "project_id": id}); code != http.StatusBadRequest {
		t.Errorf("assign blank folder: %d, want 400", code)
	}

	code, got = doJSON(t, srv, "GET", "/api/projects", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	ps := got["projects"].([]any)
	if len(ps) != 1 {
		t.Fatalf("projects = %v", ps)
	}
	p := ps[0].(map[string]any)
	if p["name"] != "BNA app" || len(p["folders"].([]any)) != 1 {
		t.Errorf("project = %v", p)
	}

	// Unassign with project_id null.
	if code, _ := doJSON(t, srv, "PUT", "/api/project-folders",
		map[string]any{"folder": `C:\OCIS2\BNA`, "project_id": nil}); code != http.StatusOK {
		t.Errorf("unassign: %d", code)
	}

	if code, _ := doJSON(t, srv, "DELETE", path, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d, want 204", code)
	}
	if code, _ := doJSON(t, srv, "DELETE", path, nil); code != http.StatusNotFound {
		t.Errorf("delete twice: %d, want 404", code)
	}
}

// Mutations need a JSON Content-Type, the same CSRF gate as every POST.
func TestProjectsAPI_RequiresJSONContentType(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/projects"},
		{"PATCH", "/api/projects/1"},
		{"PUT", "/api/project-folders"},
	} {
		req := httptest.NewRequest(c.method, c.path, bytes.NewReader([]byte(`name=x`)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s %s with a form body: %d, want 415", c.method, c.path, w.Code)
		}
	}
}

func TestFoldersAPI_EffectiveProject(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()

	now := time.Now().UTC()
	insertFolderEvent(t, st, `C:\OCIS2\BNA`, now, 2, "computed")
	insertFolderEvent(t, st, `C:\OCIS2\BNA\app`, now, 1, "computed")
	insertFolderEvent(t, st, `C:\Other`, now, 4, "ceiling")
	insertFolderEvent(t, st, ``, now, 0.5, "computed")

	p, _ := st.CreateProject("BNA")
	if err := st.AssignFolder(`C:\OCIS2\BNA`, &p.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}

	code, got := doJSON(t, srv, "GET", "/api/folders", nil)
	if code != http.StatusOK {
		t.Fatalf("folders: %d %v", code, got)
	}
	rows := map[string]map[string]any{}
	for _, r := range got["folders"].([]any) {
		m := r.(map[string]any)
		rows[m["folder"].(string)] = m
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %v", rows)
	}

	root := rows[`C:\OCIS2\BNA`]
	if root["assigned_project_id"] != float64(p.ID) || root["project_id"] != float64(p.ID) {
		t.Errorf("root = %v", root)
	}
	child := rows[`C:\OCIS2\BNA\app`]
	if child["assigned_project_id"] != nil || child["project_id"] != float64(p.ID) || child["via"] != `C:\OCIS2\BNA` {
		t.Errorf("child = %v", child)
	}
	other := rows[`C:\Other`]
	if other["project_id"] != nil || other["estimated_usd"] != 4.0 {
		t.Errorf("other = %v", other)
	}
	if none := rows[""]; none["project_id"] != nil || none["cost_usd"] != 0.5 {
		t.Errorf("no-folder row = %v", none)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestUsageByProjectAPI(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()

	d0 := time.Date(2026, 11, 15, 5, 0, 0, 0, time.UTC)
	insertFolderEvent(t, st, "/srv/a", d0.Add(time.Hour), 2, "computed")
	insertFolderEvent(t, st, "/srv/b", d0.Add(25*time.Hour), 3, "computed")
	p, _ := st.CreateProject("A")
	st.AssignFolder("/srv/a", &p.ID)

	b := func(t time.Time) string { return "b=" + t.Format(time.RFC3339) }
	code, got := doJSON(t, srv, "GET", "/api/usage/by-project?"+b(d0)+"&"+b(d0.Add(24*time.Hour))+"&"+b(d0.Add(48*time.Hour)), nil)
	if code != http.StatusOK {
		t.Fatalf("by-project: %d %v", code, got)
	}
	cols := got["projects"].([]any)
	if len(cols) != 2 || cols[0].(map[string]any)["name"] != "A" || cols[1].(map[string]any)["id"] != nil {
		t.Errorf("projects = %v", cols)
	}
	cells := got["cells"].([]any)
	if len(cells) != 2 {
		t.Fatalf("cells = %v", cells)
	}
	if c := cells[0].([]any)[0].(map[string]any); c["cost_usd"] != 2.0 {
		t.Errorf("day1 A = %v", c)
	}
	if c := cells[1].([]any)[1].(map[string]any); c["cost_usd"] != 3.0 {
		t.Errorf("day2 Unassigned = %v", c)
	}
	if tot := got["total"].(map[string]any); tot["cost_usd"] != 5.0 {
		t.Errorf("total = %v", tot)
	}

	for _, q := range []string{"", "?" + b(d0), "?b=yesterday&b=today", "?" + b(d0) + "&" + b(d0)} {
		if code, _ := doJSON(t, srv, "GET", "/api/usage/by-project"+q, nil); code != http.StatusBadRequest {
			t.Errorf("query %q: %d, want 400", q, code)
		}
	}
}
