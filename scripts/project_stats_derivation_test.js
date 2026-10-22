'use strict';
// Regression test for in-memory derivation of project-scoped stats.
//
// When a specific project is selected, fetchAllTasks already loads every task of
// that project. The front end must derive the exact status distribution in memory
// instead of making a redundant HTTP request to /api/board/stats?project_path=...
// with 7 SQL aggregate functions.
//
// Usage: node scripts/project_stats_derivation_test.js [path/to/index.html]
//
// No external dependencies, no network, no files are written.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const indexPath = path.resolve(
  process.argv[2] || path.join(__dirname, '..', 'src', 'public', 'index.html')
);

const failures = [];
function check(condition, message) {
  if (!condition) failures.push(message);
}

// ---------------------------------------------------------------- source

const html = fs.readFileSync(indexPath, 'utf8');

const scriptRe = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g;
let inline = null;
let match;
while ((match = scriptRe.exec(html)) !== null) inline = match;
if (!inline) {
  console.error('FAIL: no inline script found in ' + indexPath);
  process.exit(1);
}

const source = inline[1];
const closer = '})();';
const closerAt = source.lastIndexOf(closer);
if (closerAt < 0) {
  console.error('FAIL: the app script is not an IIFE, cannot extract it');
  process.exit(1);
}

// Expose the render helpers and stats derivation to the test sandbox.
const patched =
  source.slice(0, closerAt) +
  'globalThis.__taskview = { ' +
  'computeStatsFromTasks: computeStatsFromTasks, ' +
  'computeTasksFingerprint: computeTasksFingerprint, ' +
  'loadAll: loadAll, ' +
  'refreshStats: refreshStats, ' +
  'renderStats: renderStats, ' +
  'state: state, ' +
  'boardEl: boardEl ' +
  '}; ' +
  source.slice(closerAt);

// ------------------------------------------------------------------ DOM Stub

function makeEl(tag) {
  return {
    tagName: String(tag || 'div').toUpperCase(),
    innerHTML: '',
    textContent: '',
    value: '',
    className: '',
    id: '',
    hidden: false,
    disabled: false,
    tabIndex: 0,
    style: {},
    dataset: {},
    attributes: {},
    children: [],
    parentNode: null,
    classList: {
      names: new Set(),
      add: function () {
        Array.prototype.forEach.call(arguments, function (c) { this.names.add(c); }, this);
      },
      remove: function () {
        Array.prototype.forEach.call(arguments, function (c) { this.names.delete(c); }, this);
      },
      toggle: function (name, force) {
        const on = force === undefined ? this.names.has(name) : !!force;
        if (on) this.names.add(name); else this.names.delete(name);
        return on;
      },
      contains: function (name) { return this.names.has(name); },
    },
    addEventListener: function () {},
    removeEventListener: function () {},
    dispatchEvent: function () {},
    appendChild: function (child) {
      if (child) child.parentNode = this;
      this.children.push(child);
      return child;
    },
    removeChild: function (child) {
      this.children = this.children.filter(function (c) { return c !== child; });
      return child;
    },
    insertBefore: function (node) {
      if (node) node.parentNode = this;
      this.children.unshift(node);
      return node;
    },
    setAttribute: function (key, value) { this.attributes[key] = String(value); },
    getAttribute: function (key) {
      return Object.prototype.hasOwnProperty.call(this.attributes, key) ? this.attributes[key] : null;
    },
    removeAttribute: function (key) { delete this.attributes[key]; },
    querySelector: function () { const child = makeEl('div'); child.parentNode = this; return child; },
    querySelectorAll: function () { return []; },
    closest: function () { return null; },
    contains: function () { return false; },
    focus: function () {},
    blur: function () {},
    scrollIntoView: function () {},
    remove: function () {},
  };
}

const byId = new Map();
function elementFor(key) {
  if (!byId.has(key)) {
    const el = makeEl('div');
    el.parentNode = makeEl('div');
    byId.set(key, el);
  }
  return byId.get(key);
}

const requestedUrls = [];
let mockTasks = [];
let mockProjects = [];
let mockGlobalStats = { total: 10, backlog: 2, todo: 3, in_progress: 2, review: 1, done: 2, projects: 3 };

const documentStub = {
  readyState: 'loading',
  visibilityState: 'visible',
  documentElement: makeEl('html'),
  body: makeEl('body'),
  getElementById: function (id) { return elementFor('id:' + id); },
  querySelector: function (sel) { return elementFor('sel:' + sel); },
  querySelectorAll: function () { return []; },
  createElement: function (tag) { return makeEl(tag); },
  addEventListener: function () {},
  removeEventListener: function () {},
};

const sandbox = {
  document: documentStub,
  console: console,
  localStorage: {
    getItem: function () { return null; },
    setItem: function () {},
    removeItem: function () {},
  },
  CSS: { escape: function (v) { return String(v); } },
  fetch: function (url) {
    const target = String(url);
    requestedUrls.push(target);
    let payload = [];
    if (target.indexOf('/api/board/projects') >= 0) {
      payload = mockProjects;
    } else if (target.indexOf('/api/board/stats') >= 0) {
      payload = mockGlobalStats;
    } else if (target.indexOf('/api/board/tasks') >= 0) {
      payload = mockTasks;
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      text: function () { return Promise.resolve(JSON.stringify(payload)); },
    });
  },
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  setInterval: function () { return 0; },
  clearInterval: clearInterval,
  requestAnimationFrame: function () { return 0; },
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;

vm.createContext(sandbox);
vm.runInContext(patched, sandbox, { filename: 'index.html' });

const tv = sandbox.__taskview;
if (!tv) {
  console.error('FAIL: app script did not expose render helpers');
  process.exit(1);
}

// ------------------------------------------------- 1. In-memory derivation unit test

const sampleTasks = [
  { id: 'T1', status: 'backlog', priority: 'low' },
  { id: 'T2', status: 'todo', priority: 'medium' },
  { id: 'T3', status: 'todo', priority: 'high' },
  { id: 'T4', status: 'in_progress', priority: 'high' },
  { id: 'T5', status: 'review', priority: 'urgent' },
  { id: 'T6', status: 'done', priority: 'medium' },
  { id: 'T7', status: 'done', priority: 'low' },
];

const derived = tv.computeStatsFromTasks(sampleTasks, 'C:/project/test');

check(derived.total === 7, 'derived total expected 7, got ' + derived.total);
check(derived.backlog === 1, 'derived backlog expected 1, got ' + derived.backlog);
check(derived.todo === 2, 'derived todo expected 2, got ' + derived.todo);
check(derived.in_progress === 1, 'derived in_progress expected 1, got ' + derived.in_progress);
check(derived.review === 1, 'derived review expected 1, got ' + derived.review);
check(derived.done === 2, 'derived done expected 2, got ' + derived.done);
check(derived.open === 5, 'derived open expected 5 (7 - 2), got ' + derived.open);
check(derived.projects === 1, 'derived projects expected 1, got ' + derived.projects);
check(derived.project_path === 'C:/project/test', 'derived project_path mismatch');

// Empty tasks case
const emptyDerived = tv.computeStatsFromTasks([], 'C:/project/empty');
check(emptyDerived.total === 0, 'empty total expected 0');
check(emptyDerived.open === 0, 'empty open expected 0');
check(emptyDerived.projects === 0, 'empty projects expected 0');

// ------------------------------------------- 2. Network Request Isolation Test

mockTasks = sampleTasks;
mockProjects = [{ path: 'C:/project/test', name: 'Test', total_tasks: 7 }];

// Test Case A: Scoped project view (state.projectPath is set)
tv.state.projectPath = 'C:/project/test';
requestedUrls.length = 0;

tv.loadAll().then(function () {
  const statsRequests = requestedUrls.filter(function (u) { return u.indexOf('/api/board/stats') >= 0; });
  check(
    statsRequests.length === 0,
    'loadAll in project view made redundant stats request(s): ' + statsRequests.join(', ')
  );
  check(tv.state.stats.total === 7, 'state.stats.total expected 7, got ' + tv.state.stats.total);
  check(tv.state.stats.open === 5, 'state.stats.open expected 5, got ' + tv.state.stats.open);
  check(tv.state.stats.done === 2, 'state.stats.done expected 2, got ' + tv.state.stats.done);

  // refreshStats in project view also must not call API
  requestedUrls.length = 0;
  tv.refreshStats();
  const refreshStatsReqs = requestedUrls.filter(function (u) { return u.indexOf('/api/board/stats') >= 0; });
  check(
    refreshStatsReqs.length === 0,
    'refreshStats in project view made redundant stats request(s): ' + refreshStatsReqs.join(', ')
  );

  // Test Case B: Global all-projects view (state.projectPath is null)
  tv.state.projectPath = null;
  requestedUrls.length = 0;
  return tv.loadAll();
}).then(function () {
  const globalStatsRequests = requestedUrls.filter(function (u) { return u.indexOf('/api/board/stats') >= 0; });
  check(
    globalStatsRequests.length === 1,
    'loadAll in global view should make exactly 1 stats request, made: ' + globalStatsRequests.length
  );
  check(tv.state.stats.total === 10, 'global state.stats.total expected 10, got ' + tv.state.stats.total);

  if (failures.length) {
    console.error('FAIL: ' + failures.length + ' assertion(s) failed');
    failures.forEach(function (f) { console.error('  - ' + f); });
    process.exit(1);
  }
  console.log('OK: project-scoped stats are derived in memory with zero redundant HTTP requests');
}).catch(function (err) {
  console.error('FAIL: unexpected error during stats derivation test:', err);
  process.exit(1);
});
