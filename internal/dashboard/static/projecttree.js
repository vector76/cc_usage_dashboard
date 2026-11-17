'use strict';

// Folder rules and the folder tree for the /projects page.
//
// folderKey / makeResolver mirror internal/folder (Key, Resolver) so the page
// can show which project an intermediate tree node — one with no events of
// its own, which the server never reports on — would hand down to its
// children. userscript/test/projecttree.test.js runs the Go package's cases
// against this copy.

function isWindowsPath(p) {
  return p.startsWith('\\\\') || /^[A-Za-z]:/.test(p);
}

// folderKey: Windows paths case-folded with '\' separators; POSIX paths
// case-sensitive; trailing separator dropped (POSIX root stays "/"); an
// encoded directory name verbatim.
function folderKey(p) {
  p = String(p == null ? '' : p).trim();
  if (p === '') return '';
  if (isWindowsPath(p)) return p.replace(/\//g, '\\').toLowerCase().replace(/\\+$/, '');
  if (p.startsWith('/')) return p.replace(/\/+$/, '') || '/';
  return p;
}

function parentKey(k) {
  if (isWindowsPath(k)) {
    const i = k.lastIndexOf('\\');
    return i < 2 ? null : k.slice(0, i);
  }
  const i = k.lastIndexOf('/');
  if (i < 0 || k === '/') return null;
  return i === 0 ? '/' : k.slice(0, i);
}

// makeResolver returns resolve(path) -> {folder, project_id} for the nearest
// assigned folder at or above path, or null when unassigned.
function makeResolver(assignments) {
  const byKey = new Map();
  for (const a of assignments || []) {
    const k = folderKey(a.folder);
    if (k) byKey.set(k, a);
  }
  return function resolve(path) {
    let k = folderKey(path);
    while (k) {
      if (byKey.has(k)) return byKey.get(k);
      k = parentKey(k);
    }
    return null;
  };
}

// splitPath breaks a path into a root and its segments, keeping the original
// spelling: "C:\OCIS2\BNA" -> {root: "C:\", sep: "\", parts: ["OCIS2","BNA"]}.
// Encoded names and "" return null: they are not paths and are listed flat.
function splitPath(p) {
  if (isWindowsPath(p)) {
    const norm = p.replace(/\//g, '\\');
    if (norm.startsWith('\\\\')) {
      const bits = norm.slice(2).split('\\').filter(Boolean);
      const root = '\\\\' + bits.slice(0, 2).join('\\');
      return { root, sep: '\\', parts: bits.slice(2) };
    }
    return { root: norm.slice(0, 2) + '\\', sep: '\\', parts: norm.slice(2).split('\\').filter(Boolean) };
  }
  if (p.startsWith('/')) return { root: '/', sep: '/', parts: p.split('/').filter(Boolean) };
  return null;
}

function joinPath(base, sep, part) {
  return base.endsWith(sep) ? base + part : base + sep + part;
}

function newNode(path, label) {
  return {
    key: folderKey(path), path, label,
    ownEvents: 0, ownCost: 0, ownEstimated: 0,
    events: 0, cost: 0, estimated: 0, lastSeen: 0,
    children: [], assigned: null, effective: null, via: '',
    projectsBelow: [],
  };
}

// buildFolderTree turns the /api/folders rows plus the explicit assignments
// into {roots, loose}. roots is a forest keyed by drive / UNC share / "/";
// loose holds encoded directory names and the no-folder row, which cannot be
// placed in a tree. Every node carries rolled-up events, cost, estimated
// cost and last-seen time, its explicit assignment (assigned), the project it
// counts toward (effective, null = Unassigned) and the folder that decided
// it (via), plus projectsBelow: the distinct effective projects (null for
// Unassigned) among it and its descendants that have events.
//
// A chain of single-child folders with no events or assignment of their own
// collapses into one row ("docs\dsp-review"), so a deep tree of ~700 paths
// stays readable. Roots never collapse.
function buildFolderTree(folders, assignments) {
  const resolve = makeResolver(assignments);
  const roots = new Map();
  const loose = [];
  const nodes = new Map(); // key -> node

  const ensure = (p) => {
    const sp = splitPath(p);
    if (!sp) return null;
    let node = nodes.get(folderKey(sp.root));
    if (!node) {
      node = newNode(sp.root, sp.root);
      nodes.set(node.key, node);
      roots.set(node.key, node);
    }
    let path = sp.root;
    for (const part of sp.parts) {
      path = joinPath(path, sp.sep, part);
      const k = folderKey(path);
      let child = nodes.get(k);
      if (!child) {
        child = newNode(path, part);
        nodes.set(k, child);
        node.children.push(child);
      }
      node = child;
    }
    return node;
  };

  // Highest cost first, so a folder spelled two ways keeps its busier spelling.
  const rows = (folders || []).slice().sort((a, b) => (b.cost_usd || 0) - (a.cost_usd || 0));
  for (const f of rows) {
    const node = f.folder ? ensure(f.folder) : null;
    const target = node || (() => {
      const n = newNode(f.folder || '', f.folder || '(no folder recorded)');
      n.loose = true;
      loose.push(n);
      return n;
    })();
    target.ownEvents += f.events || 0;
    target.ownCost += f.cost_usd || 0;
    target.ownEstimated += f.estimated_usd || 0;
    const seen = Date.parse(f.last_seen);
    if (!isNaN(seen)) target.lastSeen = Math.max(target.lastSeen, seen);
  }
  for (const a of assignments || []) {
    const n = splitPath(a.folder) ? ensure(a.folder) : null;
    if (n) n.assigned = a.project_id;
  }
  for (const n of loose) {
    const a = (assignments || []).find((x) => folderKey(x.folder) === n.key && n.key !== '');
    if (a) n.assigned = a.project_id;
  }

  const finish = (n) => {
    const hit = n.key ? resolve(n.path) : null;
    n.effective = hit ? hit.project_id : null;
    n.via = hit ? hit.folder : '';
    n.events = n.ownEvents;
    n.cost = n.ownCost;
    n.estimated = n.ownEstimated;
    const below = new Set(n.ownEvents > 0 ? [n.effective] : []);
    for (const c of n.children) {
      finish(c);
      n.events += c.events;
      n.cost += c.cost;
      n.estimated += c.estimated;
      n.lastSeen = Math.max(n.lastSeen, c.lastSeen);
      for (const p of c.projectsBelow) below.add(p);
    }
    n.projectsBelow = [...below];
    n.children.sort((a, b) => b.cost - a.cost || a.label.localeCompare(b.label));
  };
  const collapse = (n) => {
    n.children = n.children.map((c) => {
      let cur = c;
      while (cur.children.length === 1 && cur.ownEvents === 0 && cur.assigned == null) {
        const only = cur.children[0];
        only.label = cur.label + (isWindowsPath(cur.path) ? '\\' : '/') + only.label;
        cur = only;
      }
      collapse(cur);
      return cur;
    });
  };

  const forest = [...roots.values()];
  for (const r of forest) { finish(r); collapse(r); }
  for (const n of loose) finish(n);
  forest.sort((a, b) => b.cost - a.cost || a.label.localeCompare(b.label));
  loose.sort((a, b) => b.cost - a.cost);
  return { roots: forest, loose };
}

if (typeof module !== 'undefined') {
  module.exports = { folderKey, makeResolver, buildFolderTree };
}
