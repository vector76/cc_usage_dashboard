package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vector76/cc_usage_dashboard/internal/folder"
)

// Errors the projects API maps to HTTP statuses.
var (
	ErrNotFound      = errors.New("not found")
	ErrDuplicateName = errors.New("a project with that name already exists")
	ErrInvalidName   = errors.New("project name must be 1-100 printable characters")
	ErrInvalidFolder = errors.New("folder must be non-empty")
)

const maxProjectNameLen = 100

// Project is a user-defined grouping of folders for cost accounting.
type Project struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Folders are the explicitly assigned folders, sorted. Never nil.
	Folders []string `json:"folders"`
}

func cleanProjectName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxProjectNameLen {
		return "", ErrInvalidName
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "", ErrInvalidName
		}
	}
	return name, nil
}

// CreateProject adds a project. Names are unique ignoring case.
func (s *Store) CreateProject(name string) (Project, error) {
	name, err := cleanProjectName(name)
	if err != nil {
		return Project{}, err
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(`INSERT INTO projects (name, created_at) VALUES (?, ?)`, name, FormatTime(now))
	if IsUniqueConstraintViolation(err) {
		return Project{}, ErrDuplicateName
	}
	if err != nil {
		return Project{}, fmt.Errorf("failed to create project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Project{}, fmt.Errorf("failed to read project id: %w", err)
	}
	return Project{ID: id, Name: name, CreatedAt: now, Folders: []string{}}, nil
}

// RenameProject changes a project's name.
func (s *Store) RenameProject(id int64, name string) error {
	name, err := cleanProjectName(name)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE projects SET name = ? WHERE id = ?`, name, id)
	if IsUniqueConstraintViolation(err) {
		return ErrDuplicateName
	}
	if err != nil {
		return fmt.Errorf("failed to rename project: %w", err)
	}
	return requireOneRow(res)
}

// DeleteProject removes a project; its folders return to Unassigned. No
// usage_events row is touched.
func (s *Store) DeleteProject(id int64) error {
	res, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete project: %w", err)
	}
	return requireOneRow(res)
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to read rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListProjects returns every project, ordered by name, with its folders.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM projects ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	defer rows.Close()

	out := []Project{}
	index := map[int64]int{}
	for rows.Next() {
		var p Project
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &created); err != nil {
			return nil, fmt.Errorf("failed to scan project: %w", err)
		}
		p.CreatedAt = parseStoredTime(created)
		p.Folders = []string{}
		index[p.ID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate projects: %w", err)
	}

	as, err := s.ListFolderAssignments()
	if err != nil {
		return nil, err
	}
	for _, a := range as {
		if i, ok := index[a.ProjectID]; ok {
			out[i].Folders = append(out[i].Folders, a.Folder)
		}
	}
	for i := range out {
		sort.Strings(out[i].Folders)
	}
	return out, nil
}

// ListFolderAssignments returns every folder → project link. Never nil.
func (s *Store) ListFolderAssignments() ([]folder.Assignment, error) {
	rows, err := s.db.Query(`SELECT folder, project_id FROM project_folders ORDER BY folder`)
	if err != nil {
		return nil, fmt.Errorf("failed to list folder assignments: %w", err)
	}
	defer rows.Close()
	out := []folder.Assignment{}
	for rows.Next() {
		var a folder.Assignment
		if err := rows.Scan(&a.Folder, &a.ProjectID); err != nil {
			return nil, fmt.Errorf("failed to scan folder assignment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AssignFolder links a folder to a project, replacing any existing link for
// the same folder (compared by folder.Key, so C:\X and c:\x\ are one folder).
// A nil projectID unassigns it.
func (s *Store) AssignFolder(path string, projectID *int64) error {
	path = strings.TrimSpace(path)
	key := folder.Key(path)
	if key == "" {
		return ErrInvalidFolder
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin folder assignment: %w", err)
	}
	defer tx.Rollback()

	if projectID != nil {
		var exists int
		err := tx.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, *projectID).Scan(&exists)
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("failed to look up project: %w", err)
		}
	}

	rows, err := tx.Query(`SELECT folder FROM project_folders`)
	if err != nil {
		return fmt.Errorf("failed to read folder assignments: %w", err)
	}
	var stale []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan folder assignment: %w", err)
		}
		if folder.Key(f) == key {
			stale = append(stale, f)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to iterate folder assignments: %w", err)
	}
	for _, f := range stale {
		if _, err := tx.Exec(`DELETE FROM project_folders WHERE folder = ?`, f); err != nil {
			return fmt.Errorf("failed to clear folder assignment: %w", err)
		}
	}

	if projectID != nil {
		if _, err := tx.Exec(`INSERT INTO project_folders (folder, project_id, created_at) VALUES (?, ?, ?)`,
			path, *projectID, FormatTime(time.Now())); err != nil {
			return fmt.Errorf("failed to assign folder: %w", err)
		}
	}
	return tx.Commit()
}

// FolderStat is one distinct project_path's all-time usage. Folder "" collects
// events with no folder recorded.
type FolderStat struct {
	Folder       string    `json:"folder"`
	Events       int64     `json:"events"`
	CostUSD      float64   `json:"cost_usd"`
	EstimatedUSD float64   `json:"estimated_usd"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
}

// FolderStats returns usage per distinct project_path, highest cost first.
// CostUSD includes ceiling-priced dollars; EstimatedUSD is that share.
func (s *Store) FolderStats() ([]FolderStat, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(project_path, ''),
		       COUNT(*),
		       COALESCE(SUM(cost_usd_equivalent), 0),
		       COALESCE(SUM(CASE WHEN cost_source = 'ceiling' THEN cost_usd_equivalent END), 0),
		       MIN(occurred_at), MAX(occurred_at)
		FROM usage_events
		GROUP BY COALESCE(project_path, '')
		ORDER BY 3 DESC, 1
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate folders: %w", err)
	}
	defer rows.Close()
	out := []FolderStat{}
	for rows.Next() {
		var st FolderStat
		var first, last string
		if err := rows.Scan(&st.Folder, &st.Events, &st.CostUSD, &st.EstimatedUSD, &first, &last); err != nil {
			return nil, fmt.Errorf("failed to scan folder stat: %w", err)
		}
		st.FirstSeen = parseStoredTime(first)
		st.LastSeen = parseStoredTime(last)
		out = append(out, st)
	}
	return out, rows.Err()
}

// ResolveEncodedProjectPaths rewrites encoded directory names in
// usage_events.project_path ("C--WebApps-cc-usage-dashboard", written by
// older clusage-cli hooks) to the real path they came from, when exactly one
// folder seen elsewhere in the table encodes to that name (spellings with
// the same folder.Key count as one folder). Ambiguous and unknown names are
// left as they are; they still show up in the folder list and can be
// assigned as-is. Returns the number of rows rewritten.
//
// Run at every startup: an older hook binary can keep sending encoded names,
// and a path seen for the first time today can resolve names stored months
// ago.
func (s *Store) ResolveEncodedProjectPaths() (int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT project_path FROM usage_events WHERE COALESCE(project_path, '') != ''`)
	if err != nil {
		return 0, fmt.Errorf("failed to list project paths: %w", err)
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, fmt.Errorf("failed to scan project path: %w", err)
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("failed to iterate project paths: %w", err)
	}

	// encoded name -> folder key -> one spelling of that folder
	candidates := map[string]map[string]string{}
	var encoded []string
	for _, p := range paths {
		if folder.IsEncoded(p) {
			encoded = append(encoded, p)
			continue
		}
		e := folder.Encode(p)
		if candidates[e] == nil {
			candidates[e] = map[string]string{}
		}
		k := folder.Key(p)
		if prev, ok := candidates[e][k]; !ok || p < prev {
			candidates[e][k] = p // deterministic choice among spellings
		}
	}

	// One UPDATE over a VALUES mapping rather than one per name: project_path
	// has no index, so each statement is a full scan, and a first run on a
	// large history maps a hundred names.
	var values []string
	var args []any
	for _, name := range encoded {
		c := candidates[name]
		if len(c) != 1 {
			continue
		}
		for _, real := range c {
			values = append(values, "(?, ?)")
			args = append(args, name, real)
		}
	}
	if len(values) == 0 {
		return 0, nil
	}
	res, err := s.db.Exec(`
		WITH m(old, new) AS (VALUES `+strings.Join(values, ", ")+`)
		UPDATE usage_events
		SET project_path = (SELECT new FROM m WHERE m.old = usage_events.project_path)
		WHERE project_path IN (SELECT old FROM m)
	`, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to rewrite encoded project paths: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to read rows rewritten: %w", err)
	}
	return int(n), nil
}
