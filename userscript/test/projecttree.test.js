'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { folderKey, makeResolver, buildFolderTree } =
    require('../../internal/dashboard/static/projecttree');

// Same cases as internal/folder/folder_test.go TestKey: the page and the
// server must agree on which spellings are one folder.
test('folderKey matches internal/folder.Key', () => {
    const cases = [
        ['', ''],
        ['  ', ''],
        ['C:\\WebApps\\Foo', 'c:\\webapps\\foo'],
        ['c:\\webapps\\foo\\', 'c:\\webapps\\foo'],
        ['C:/WebApps/Foo', 'c:\\webapps\\foo'],
        ['C:\\', 'c:'],
        ['\\\\server\\share\\Dir', '\\\\server\\share\\dir'],
        ['/home/User/Proj/', '/home/User/Proj'],
        ['/', '/'],
        ['C--WebApps-cc-usage-dashboard', 'C--WebApps-cc-usage-dashboard'],
    ];
    for (const [input, want] of cases) assert.strictEqual(folderKey(input), want, input);
});

// Same cases as TestResolverLongestPrefixWins.
test('resolver: longest assigned prefix wins, on segment boundaries', () => {
    const resolve = makeResolver([
        { folder: 'C:\\Jewel\\agent', project_id: 1 },
        { folder: 'C:\\Jewel\\agent\\docs\\dsp-review', project_id: 2 },
        { folder: 'C:\\OCIS2\\BNA', project_id: 3 },
        { folder: '/home/u/proj', project_id: 4 },
        { folder: 'C--Legacy-encoded', project_id: 5 },
    ]);
    const cases = [
        ['C:\\Jewel\\agent\\src', 1],
        ['C:\\Jewel\\agent\\docs\\dsp-review\\fix-campaign', 2],
        ['c:\\ocis2\\bna\\.claude\\worktrees\\Alice\\app', 3],
        ['C:\\OCIS2\\BNA-other', null],
        ['C:\\Jewel', null],
        ['/home/u/proj/sub', 4],
        ['/home/U/proj/sub', null],
        ['C--Legacy-encoded', 5],
        ['C--Legacy-encoded-more', null],
        ['', null],
    ];
    for (const [p, want] of cases) {
        const hit = resolve(p);
        assert.strictEqual(hit ? hit.project_id : null, want, p);
    }
});

const row = (folder, cost, last = '2026-11-17T00:00:00Z', est = 0) =>
    ({ folder, events: 1, cost_usd: cost, estimated_usd: est, last_seen: last });

test('tree rolls up cost and resolves inherited projects', () => {
    const { roots, loose } = buildFolderTree([
        row('C:\\OCIS2\\BNA', 10),
        row('C:\\OCIS2\\BNA\\app', 5),
        row('C:\\OCIS2\\BNA\\.claude\\worktrees\\Alice', 3),
        row('C:\\Other', 1, '2026-01-01T00:00:00Z', 1),
        row('C--Encoded', 2),
        row('', 0.5),
    ], [{ folder: 'C:\\OCIS2\\BNA', project_id: 7 }]);

    assert.strictEqual(roots.length, 1);
    const c = roots[0];
    assert.strictEqual(c.label, 'C:\\');
    assert.strictEqual(c.cost, 19);
    assert.strictEqual(c.estimated, 1);
    assert.deepStrictEqual(c.projectsBelow.sort(), [7, null].sort());

    // C:\OCIS2 has one child and no events of its own: it collapses.
    const bna = c.children[0];
    assert.strictEqual(bna.label, 'OCIS2\\BNA');
    assert.strictEqual(bna.path, 'C:\\OCIS2\\BNA');
    assert.strictEqual(bna.assigned, 7);
    assert.strictEqual(bna.cost, 18);

    const app = bna.children.find((n) => n.label === 'app');
    assert.strictEqual(app.assigned, null);
    assert.strictEqual(app.effective, 7);
    assert.strictEqual(app.via, 'C:\\OCIS2\\BNA');

    const wt = bna.children.find((n) => n.label.startsWith('.claude'));
    assert.strictEqual(wt.label, '.claude\\worktrees\\Alice');
    assert.strictEqual(wt.effective, 7);

    const other = c.children.find((n) => n.label === 'Other');
    assert.strictEqual(other.effective, null);
    assert.strictEqual(other.lastSeen, Date.parse('2026-01-01T00:00:00Z'));

    assert.deepStrictEqual(loose.map((n) => n.label), ['C--Encoded', '(no folder recorded)']);
});

test('spellings that differ only in case merge into one node', () => {
    const { roots } = buildFolderTree([row('C:\\WebApps\\Foo', 5), row('c:\\webapps\\foo', 1)], []);
    const foo = roots[0].children[0];
    assert.strictEqual(foo.label, 'WebApps\\Foo'); // busier spelling wins
    assert.strictEqual(foo.cost, 6);
});

test('an assigned folder with no events still appears and stops collapsing', () => {
    const { roots } = buildFolderTree(
        [row('C:\\Jewel\\agent\\src', 4)],
        [{ folder: 'C:\\Jewel', project_id: 2 }],
    );
    const jewel = roots[0].children[0];
    assert.strictEqual(jewel.label, 'Jewel');
    assert.strictEqual(jewel.assigned, 2);
    assert.strictEqual(jewel.children[0].label, 'agent\\src');
    assert.strictEqual(jewel.children[0].effective, 2);
});

test('POSIX paths build their own root', () => {
    const { roots } = buildFolderTree([row('/home/u/a', 1), row('/home/u/b', 2)], []);
    assert.strictEqual(roots[0].label, '/');
    assert.strictEqual(roots[0].children[0].label, 'home/u');
    assert.deepStrictEqual(roots[0].children[0].children.map((n) => n.label), ['b', 'a']);
});
