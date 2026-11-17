package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// The /projects page must render and reach every endpoint and script it
// depends on; a page that loads but cannot fetch fails silently.
func TestProjectsPageHTML(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()

	w := get(t, srv, "/projects")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /projects: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, want := range []string{
		"/api/usage/by-project", "/api/projects", "/api/folders", "/api/project-folders",
		`src="/calendar.js"`, `src="/projecttree.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("projects page does not reference %q", want)
		}
	}
}

func TestProjectsScriptsServed(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	for path, want := range map[string]string{
		"/calendar.js":    "startOfWeek",
		"/projecttree.js": "buildFolderTree",
	} {
		w := get(t, srv, path)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/javascript") {
			t.Errorf("GET %s: %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s does not define %q", path, want)
		}
	}
}

// Nothing else advertises /projects, so the dashboard and the report must
// link to it.
func TestPagesLinkToProjects(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	for _, page := range []string{"/dashboard", "/report"} {
		if body := get(t, srv, page).Body.String(); !strings.Contains(body, `href="/projects"`) {
			t.Errorf("%s does not link to /projects", page)
		}
	}
}

// /report shares calendar.js with /projects, so its "This week" preset starts
// on Sunday like the per-project weeks do.
func TestReportUsesSharedCalendar(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	body := get(t, srv, "/report").Body.String()
	if !strings.Contains(body, `src="/calendar.js"`) || !strings.Contains(body, "startOfWeek(") {
		t.Error("report page does not use calendar.js startOfWeek")
	}
}
