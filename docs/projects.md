# Projects (per-project cost accounting)

The user defines named *projects* and assigns working folders to them. Every
usage event is then attributed to exactly one project, or to **Unassigned**,
and the `/projects` page shows cost per project per calendar day or calendar
week, above the screens for managing projects and folder assignments.

Where the code lives:

| Piece | File |
|-------|------|
| Path rules (key, encoding, longest-prefix resolver) | `internal/folder/folder.go` |
| Folder backfill, projects tables | `internal/store/migrations.go` (v11, v12) |
| Project CRUD, assignments, folder stats, encoded-name repair | `internal/store/projects.go` |
| Per-bucket, per-project aggregation | `internal/consumption/byproject.go` |
| HTTP endpoints | `internal/server/projects.go` |
| Page | `internal/dashboard/static/projects.html` |
| Calendar periods (shared with `/report`) | `internal/dashboard/static/calendar.js` |
| Browser copy of the path rules, folder tree | `internal/dashboard/static/projecttree.js` |

This answers "where did the dollars go, by line of work". It does **not**
split the session/weekly quota percentages: those are account-wide snapshots
with no per-event attribution (see the roadmap's per-project quota item).

## Concepts

- **Folder** — the working directory an event ran in, as a full path
  (`C:\OCIS2\BNA\app`). Derived from data; never typed by hand.
- **Project** — a user-defined name owning one or more folders.
- **Assignment** — a folder → project link. An assigned folder covers
  itself *and everything beneath it*.
- **Unassigned** — every event whose folder no assignment covers, plus
  events with no folder recorded. Project totals plus Unassigned always
  equal the overall total; nothing is dropped.

## Recording each event's folder

Before this feature, events did not carry a usable folder. Profiled against
the live DB on 2026-11-17:

| source | rows | cost | what `project_path` held |
|--------|-----:|-----:|---------------------------|
| tailer | 123,917 | $13,234 | **empty** — the tailer never set it. The real path survived only in `raw_json.cwd` (all but 637 rows). |
| hook   | 22,734 | $2,750 | Claude Code's *encoded* directory name (`C--WebApps-cc-usage-dashboard`) — lossy: `\`, `_`, `.`, spaces all became `-`. |
| uplink | 294 | $20 | empty (the sender's tailer never set it either). |

`docs/data-model.md` already defined `project_path` as "decoded path the
session ran in", so the fix made that true rather than adding a column:

1. **Tailer** (`ingest.Parser`) sets `project_path` from the transcript
   line's top-level `cwd`.
2. **Hook** (`cmd/clusage-cli/hook.go`) sends the line's `cwd`, falling back
   to the encoded directory segment only for a line without one.
3. **Migration v11** copies `json_extract(raw_json, '$.cwd')` into every row
   whose `project_path` is empty, with the same `LIKE` + `json_valid`
   prefilter as v9/v10. Rows that already hold a value are left alone.
4. **`store.ResolveEncodedProjectPaths`**, run at every trayapp start after
   the cost backfill, maps encoded names back to real paths: it encodes
   every decoded `project_path` in the table (`folder.Encode`: every
   character that is not an ASCII letter or digit becomes `-`) and rewrites
   each encoded name that exactly one folder produces (spellings with the
   same `folder.Key` count as one folder). It is a single `UPDATE` over a
   `VALUES` mapping: `project_path` has no index, so one statement per name
   meant one full scan per name, and the first start on the live DB took
   35 s instead of 8 s. It runs every start because older `clusage-cli`
   binaries keep sending encoded names, and a path seen for the first time
   today can resolve names stored months ago.

On the live DB the repair mapped 19,399 hook events. 25 encoded names
($332) stay unresolved — mostly worktree and container folders the tailer
never saw. They are **not** matched by prefix: `C--Jewel-agent` is a
prefix of `C--Jewel-agent-in-docker`, which is `C:\Jewel\agent-in-docker`,
not a subfolder. They appear at the bottom of the folder list under their
encoded name and can be assigned exactly as they are.

Uplinked events carry the sender's folder automatically, since `uplink.go`
already forwards `project_path`.

### Per-event folder, not per-session

`cwd` can change mid-session (332 sessions have more than one). Attribution
uses each event's own folder. Measured: only $160 of $13,190 lands outside
the session's starting folder, and most of that is Claude stepping into a
sibling repo — arguably *correct* to attribute there. Per-event also needs no
session-level lookup and works the same for hook and uplink rows.

## Data model

Migration v12 adds `projects` (`id`, `name` UNIQUE COLLATE NOCASE,
`created_at`) and `project_folders` (`folder` PK, `project_id` →
`projects.id` ON DELETE CASCADE, `created_at`); see `docs/data-model.md`.

`folder` is stored as the user picked it. `store.AssignFolder` removes any
row with the same `folder.Key` before inserting, so a folder belongs to at
most one project however it is spelled. Deleting a project releases its
folders back to Unassigned; no event rows are touched. Assignments are
**retroactive** — they apply to all history, which is what makes "re-run
last month's report after tidying up the assignments" work. There are no
effective dates.

### Attribution rule: longest matching prefix

An event belongs to the project whose assigned folder is the **longest**
path-prefix of the event's folder, matched on whole path segments
(`folder.Resolver`).

- Assign `C:\OCIS2\BNA` → BNA covers `C:\OCIS2\BNA\app` and every
  `C:\OCIS2\BNA\.claude\worktrees\*`. Worktrees need no special casing.
- Assign `C:\Jewel\agent` → Jewel, *and* `C:\Jewel\agent\docs\dsp-review` →
  DSP Review: the deeper assignment wins for that subtree.
- `C:\Foo` does **not** cover `C:\Foobar`.
- An encoded name matches only an assignment of exactly that name.

`folder.Key` normalizes before comparing: for drive-letter and UNC paths,
`/` becomes `\` and the path is **case-folded** (Windows is
case-insensitive; `C:\webapps` and `C:\WebApps` both occur in real data);
POSIX paths from a forwarding container stay case-sensitive; a trailing
separator is dropped.

Resolution happens in Go, not SQL: queries group by distinct folder (≈700),
each folder is resolved once, and the results are folded into projects. SQL
`LIKE` prefix matching can't express "longest wins" or the case rules
cleanly, and the folder count is small.

The page needs the same rule for tree nodes that have no events of their
own (the server never reports those), so `projecttree.js` carries a copy:
`folderKey` / `makeResolver`. `userscript/test/projecttree.test.js` runs the
Go package's test cases against it so the two copies can't drift silently.

Not built: assigning a subfolder back to "nobody" (an exclusion). The
workaround is to give it its own project.

## Calendar buckets: days and Sunday-start weeks

Days run local midnight to local midnight; weeks run **Sunday 00:00 through
Saturday 24:00**, local time.

Following the rule in `docs/usage-report.md` (the client owns the time
zone), the **browser computes the bucket boundaries** (`calendar.js`) and
sends them as instants. The server counts events in each half-open
`[bᵢ, bᵢ₊₁)`. That gets DST right for free (a 23- or 25-hour day is just two
instants farther or closer apart), and adjacent buckets partition the data
exactly. Boundaries are built with `new Date(y, m, d)` (local midnight),
never by adding 86,400,000 ms.

`/report`'s "This week" preset used to start on Monday. It now uses the same
`startOfWeek()` from `calendar.js`, so both pages agree on Sunday.

## API

All mutating endpoints sit behind the same JSON `Content-Type` gate as every
other POST (`requireJSONPOST`, the CSRF defence) and the token rule for
non-loopback callers. `DELETE` has no body; not being a "simple" method, it
cannot be sent cross-origin without a CORS preflight the server never grants.

| Method & path | Purpose |
|---------------|---------|
| `GET /api/projects` | Projects with their assigned folders. |
| `POST /api/projects` `{name}` | Create (201). 409 on a duplicate name in any case; 400 on a blank or over-long name. |
| `PATCH /api/projects/{id}` `{name}` | Rename. 404 if missing. |
| `DELETE /api/projects/{id}` | Delete (204); its folders return to Unassigned. |
| `PUT /api/project-folders` `{folder, project_id}` | Assign or move a folder; `project_id: null` unassigns. 404 for an unknown project. |
| `GET /api/folders` | Every folder seen in `usage_events`, all time: events, cost, estimated cost, first/last seen, the explicit assignment if any, and the effective project with the folder it comes from (`via`). Folder `""` collects events with no folder. |
| `GET /api/usage/by-project?b=t0&b=t1…&b=tn` | Cost per bucket per project. |

`by-project` response:

```json
{
  "buckets": [{"start": "...", "end": "..."}],
  "projects": [{"id": 3, "name": "BNA"}, {"id": null, "name": "Unassigned"}],
  "cells": [[{"cost_usd": 41.2, "estimated_usd": 0, "events": 812, "events_without_cost": 0}, ...]],
  "bucket_totals": [...], "project_totals": [...], "total": {...}
}
```

- `cells` is `[bucket][project]`, parallel to `buckets` and `projects`.
- Columns are the projects with any events in the range, by total cost
  descending, then **Unassigned**, which is always present (even with no
  events), so the client always gets a well-formed matrix.
- `cost_usd` includes ceiling-priced dollars and `estimated_usd` is that
  share, so the page can mark it "est." as `/report` does.
- Boundaries: RFC3339, at least two, strictly increasing, at most 400
  buckets (a year of days plus slack). Anything else is 400, and the
  boundaries are not echoed back.
- One query joins `usage_events` to a `VALUES` table of the boundaries on
  `occurred_at` (indexed), grouped by bucket, folder and `cost_source`.
  On the live DB: 8 weeks in ~0.3 s, 365 days in ~0.8 s.
- Verified against the live DB: the by-project total over a range equals
  `/api/usage/breakdown`'s total for the same range to the cent and to the
  event.

## The `/projects` page

One page, top to bottom: the report, the project list, the folder tree. The
report sits on the same page as the assignments because changing an
assignment changes the report, and the page reloads it after every change.
It is linked from the dashboard's Consumption heading and from `/report`.
Like `/report`, it fetches on demand and runs nothing on a timer.

### Cost by project

- **Days / Weeks** toggle and a "Show last" picker: 7, 14, 30 or 90 days;
  4, 8, 13, 26 or 52 weeks. Defaults: 8 weeks, 14 days. The choice is kept
  in `localStorage` (`cc-projects-unit`, `cc-projects-count-<unit>`).
- The last bucket is the current, partial period: its bars are drawn paler
  and its table row says "in progress".
- A stacked SVG bar chart (drawn by hand, like the burn-down charts; no
  library) answers "how is this trending"; the table below it answers "what
  was the number", newest period first, with per-project and overall totals.
- Colours come from a fixed palette by project id, so a project keeps its
  colour when the range changes which projects appear; Unassigned is gray.
- A cell with ceiling-priced dollars is gray with an "est." flag; a note
  under the table explains it, and also counts events with no cost.

### Projects

Each project shows its all-time cost (folder costs resolved through the
assignments in the browser), its assigned folders, and Rename (inline) /
Delete (confirmed) buttons. Unassigned's all-time cost is listed last. A
text box adds a project.

### Folders

- A **tree**, not a flat list: ~700 distinct folders, but only a handful of
  roots need assigning. Roots are drives, UNC shares and `/`; encoded names
  and the no-folder row are listed flat at the bottom.
- Each row shows rolled-up all-time cost (gray when part of it is
  estimated), when the subtree was last used, and a project picker. An
  explicit assignment shows the project in bold; an inherited one shows
  "(inherits BNA)" in the picker with "via C:\OCIS2\BNA" beside it; an
  unassigned folder whose subfolders have projects shows "BNA below" or
  "mixed below".
- Chains of single-child folders with no events or assignment of their own
  collapse into one row (`OCIS2\BNA`, `.claude\worktrees`). Roots start
  expanded, everything else collapsed; a text filter expands to matches.
- "Hide folders idle for 90 days" (on by default) hides a subtree whose
  newest event is older than that, unless it holds an assignment.

## Tests

- **Folder capture** (`internal/ingest/parser_cwd_test.go`,
  `cmd/clusage-cli/hook_cwd_test.go`, `internal/store/projects_test.go`):
  the parser and tailer store `cwd`; the hook sends `cwd` and falls back to
  the encoded name; v11 backfills from `raw_json`, skips malformed or empty
  JSON and never overwrites a value; the repair rewrites only unambiguous
  encodings, treats case variants as one folder, and is idempotent.
- **Attribution** (`internal/folder/folder_test.go`, mirrored in
  `userscript/test/projecttree.test.js`): longest prefix wins; segment
  boundary; drive-letter case-folding; POSIX case-sensitivity; mixed
  separators; drive and POSIX roots; encoded names exact-only.
- **Store/API** (`internal/store/projects_test.go`,
  `internal/server/projects_test.go`): create/rename/delete; duplicate names
  in any case → 409; reassigning moves a folder; deleting a project releases
  its folders; form-encoded mutations → 415; the folder list's effective and
  inherited projects.
- **Buckets** (`internal/consumption/byproject_test.go`): an event exactly on
  a boundary lands in the later bucket and one on the end bound is
  excluded; unequal (DST) buckets; daily buckets sum to the same range as one
  bucket; column order; Unassigned always present; boundary validation.
- **Client** (`userscript/test/calendar.test.js`, `projecttree.test.js`,
  `dashboard-pages.test.js`): `startOfWeek` is Sunday for every day of the
  week; day boundaries across both US DST changes stay on local midnight;
  the tree rolls up, merges case variants, collapses chains and resolves
  inheritance; the page loads the default 8-week report, switches to days
  and remembers it, shows a failed report request, and PUTs an assignment
  then reloads both the folders and the report.
