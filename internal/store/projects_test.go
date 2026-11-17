package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/folder"
)

// openMem opens a fully migrated in-memory store.
func openMem(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func projectPathOf(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var p sql.NullString
	if err := db.QueryRow(`SELECT project_path FROM usage_events WHERE id = ?`, id).Scan(&p); err != nil {
		t.Fatalf("select project_path %d: %v", id, err)
	}
	return p.String
}

// Migration v11 recovers the folder of every tailer row stored before the
// tailer learned to record it: the cwd survived in raw_json all along.
func TestMigrationV11BackfillsProjectPathFromRawJSON(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	saved := migrations
	defer func() { migrations = saved }()
	migrations = saved[:10]
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("v1-v10: %v", err)
	}

	insert := func(projectPath, raw string) int64 {
		t.Helper()
		res, err := db.Exec(`INSERT INTO usage_events (occurred_at, source, project_path,
			input_tokens, output_tokens, raw_json) VALUES (?, 'tailer', ?, 1, 1, ?)`,
			FormatTime(time.Now()), projectPath, raw)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	withCwd := insert("", `{"cwd":"C:\\OCIS2\\BNA","message":{}}`)
	nullPath := func() int64 {
		res, err := db.Exec(`INSERT INTO usage_events (occurred_at, source, input_tokens, output_tokens, raw_json)
			VALUES (?, 'tailer', 1, 1, ?)`, FormatTime(time.Now()), `{"cwd":"/home/u/p"}`)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}()
	alreadySet := insert("C--Jewel-agent", `{"cwd":"C:\\Jewel\\agent"}`)
	noCwd := insert("", `{"message":{}}`)
	malformed := insert("", `{"cwd": broken`)
	emptyRaw := insert("", ``)

	migrations = saved
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, c := range []struct {
		id   int64
		want string
	}{
		{withCwd, `C:\OCIS2\BNA`},
		{nullPath, `/home/u/p`},
		{alreadySet, `C--Jewel-agent`}, // never overwritten by the migration
		{noCwd, ``},
		{malformed, ``},
		{emptyRaw, ``},
	} {
		if got := projectPathOf(t, db, c.id); got != c.want {
			t.Errorf("row %d: project_path = %q, want %q", c.id, got, c.want)
		}
	}
}

var folderEventSeq int

func insertFolderEvent(t *testing.T, s *Store, projectPath string, at time.Time, cost *float64, source string) int64 {
	t.Helper()
	folderEventSeq++
	id, err := s.InsertUsageEventRecord(UsageEventRecord{
		OccurredAt: at, Source: "tailer", ProjectPath: projectPath,
		SessionID: "s", MessageID: fmt.Sprintf("m%d", folderEventSeq),
		InputTokens: 1, OutputTokens: 1, CostUSD: cost, CostSource: source,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

func f(v float64) *float64 { return &v }

// Hook rows carry Claude Code's encoded directory name. Each one that exactly
// one known real path encodes to is rewritten to that path; ambiguous and
// unknown names are left alone.
func TestResolveEncodedProjectPaths(t *testing.T) {
	s := openMem(t)
	now := time.Now()
	insertFolderEvent(t, s, `C:\WebApps\cc_usage_dashboard`, now, nil, "")
	insertFolderEvent(t, s, `C:\a_b`, now, nil, "")
	insertFolderEvent(t, s, `C:\a.b`, now, nil, "") // same encoding as C:\a_b
	resolvable := insertFolderEvent(t, s, `C--WebApps-cc-usage-dashboard`, now, nil, "")
	ambiguous := insertFolderEvent(t, s, `C--a-b`, now, nil, "")
	unknown := insertFolderEvent(t, s, `C--Nowhere`, now, nil, "")
	empty := insertFolderEvent(t, s, ``, now, nil, "")

	n, err := s.ResolveEncodedProjectPaths()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n != 1 {
		t.Errorf("rewrote %d rows, want 1", n)
	}
	db := s.DB()
	if got := projectPathOf(t, db, resolvable); got != `C:\WebApps\cc_usage_dashboard` {
		t.Errorf("resolvable: %q", got)
	}
	if got := projectPathOf(t, db, ambiguous); got != `C--a-b` {
		t.Errorf("ambiguous was rewritten to %q", got)
	}
	if got := projectPathOf(t, db, unknown); got != `C--Nowhere` {
		t.Errorf("unknown was rewritten to %q", got)
	}
	if got := projectPathOf(t, db, empty); got != `` {
		t.Errorf("empty was rewritten to %q", got)
	}

	// Idempotent: a second pass finds nothing more to do.
	if n, err := s.ResolveEncodedProjectPaths(); err != nil || n != 0 {
		t.Errorf("second pass = %d, %v; want 0, nil", n, err)
	}
}

// Two spellings of one Windows path (case) are one folder, not an ambiguity.
func TestResolveEncodedProjectPathsCaseVariantsAreOneFolder(t *testing.T) {
	s := openMem(t)
	now := time.Now()
	insertFolderEvent(t, s, `C:\WebApps\Foo`, now, nil, "")
	insertFolderEvent(t, s, `c:\webapps\foo`, now, nil, "")
	id := insertFolderEvent(t, s, `C--WebApps-Foo`, now, nil, "")
	if _, err := s.ResolveEncodedProjectPaths(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := projectPathOf(t, s.DB(), id); folder.Key(got) != `c:\webapps\foo` {
		t.Errorf("got %q, want a spelling of C:\\WebApps\\Foo", got)
	}
}

func TestProjectCRUD(t *testing.T) {
	s := openMem(t)

	a, err := s.CreateProject("  BNA  ")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Name != "BNA" || a.ID == 0 {
		t.Errorf("created %+v, want trimmed name and an id", a)
	}
	if _, err := s.CreateProject("bna"); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("duplicate (any case) = %v, want ErrDuplicateName", err)
	}
	for _, bad := range []string{"", "   ", string(make([]byte, 101))} {
		if _, err := s.CreateProject(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("CreateProject(%q) = %v, want ErrInvalidName", bad, err)
		}
	}

	b, err := s.CreateProject("Jewel")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.RenameProject(b.ID, "BNA"); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("rename onto existing = %v, want ErrDuplicateName", err)
	}
	if err := s.RenameProject(b.ID, "Jewel Agent"); err != nil {
		t.Errorf("rename: %v", err)
	}
	if err := s.RenameProject(9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing = %v, want ErrNotFound", err)
	}

	ps, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := []string{}
	for _, p := range ps {
		names = append(names, p.Name)
		if p.Folders == nil {
			t.Errorf("project %q: Folders is nil, want empty slice", p.Name)
		}
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"BNA", "Jewel Agent"}) {
		t.Errorf("names = %v", names)
	}

	if err := s.DeleteProject(a.ID); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := s.DeleteProject(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice = %v, want ErrNotFound", err)
	}
}

func TestAssignFolder(t *testing.T) {
	s := openMem(t)
	a, _ := s.CreateProject("A")
	b, _ := s.CreateProject("B")

	if err := s.AssignFolder(`C:\OCIS2\BNA`, &a.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// Reassigning the same folder (in another spelling) moves it, not
	// duplicates it.
	if err := s.AssignFolder(`c:\ocis2\bna\`, &b.ID); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	as, err := s.ListFolderAssignments()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(as) != 1 || as[0].ProjectID != b.ID {
		t.Fatalf("assignments = %+v, want one, to B", as)
	}

	if err := s.AssignFolder(`C:\x`, func() *int64 { v := int64(9999); return &v }()); !errors.Is(err, ErrNotFound) {
		t.Errorf("assign to missing project = %v, want ErrNotFound", err)
	}
	if err := s.AssignFolder("  ", &a.ID); !errors.Is(err, ErrInvalidFolder) {
		t.Errorf("assign empty folder = %v, want ErrInvalidFolder", err)
	}

	// Unassign.
	if err := s.AssignFolder(`C:\OCIS2\BNA`, nil); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if as, _ := s.ListFolderAssignments(); len(as) != 0 {
		t.Errorf("after unassign: %+v", as)
	}

	// Deleting a project releases its folders.
	if err := s.AssignFolder(`C:\Jewel`, &a.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := s.DeleteProject(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if as, _ := s.ListFolderAssignments(); len(as) != 0 {
		t.Errorf("after delete: %+v", as)
	}
}

func TestListProjectsIncludesFolders(t *testing.T) {
	s := openMem(t)
	a, _ := s.CreateProject("A")
	s.AssignFolder(`C:\two`, &a.ID)
	s.AssignFolder(`C:\one`, &a.ID)
	ps, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ps) != 1 || !reflect.DeepEqual(ps[0].Folders, []string{`C:\one`, `C:\two`}) {
		t.Errorf("projects = %+v", ps)
	}
}

func TestFolderStats(t *testing.T) {
	s := openMem(t)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	insertFolderEvent(t, s, `C:\A`, t0, f(1.5), "computed")
	insertFolderEvent(t, s, `C:\A`, t0.Add(48*time.Hour), f(2), "ceiling")
	insertFolderEvent(t, s, `C:\A`, t0.Add(time.Hour), nil, "")
	insertFolderEvent(t, s, ``, t0, f(0.25), "reported")

	stats, err := s.FolderStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	byFolder := map[string]FolderStat{}
	for _, st := range stats {
		byFolder[st.Folder] = st
	}
	a := byFolder[`C:\A`]
	if a.Events != 3 || a.CostUSD != 3.5 || a.EstimatedUSD != 2 {
		t.Errorf("C:\\A = %+v", a)
	}
	if !a.FirstSeen.Equal(t0) || !a.LastSeen.Equal(t0.Add(48*time.Hour)) {
		t.Errorf("C:\\A seen %v .. %v", a.FirstSeen, a.LastSeen)
	}
	if none, ok := byFolder[""]; !ok || none.Events != 1 || none.CostUSD != 0.25 {
		t.Errorf("no-folder row = %+v, %v", none, ok)
	}
}
