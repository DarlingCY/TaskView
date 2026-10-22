'use strict';
// Regression test for the project list in the sidebar.
//
// Every project row has to show the number of its backlog, todo and
// in-progress tasks, each next to a dot in the colour of that status. The
// former single total/open counter is gone.
//
// The front end is a single inline script, so the test loads it into a Node
// VM with a small DOM stub and asserts on the generated markup.
//
// Usage: node scripts/sidebar_counts_test.js [path/to/index.html]
//
// No dependencies, no network, no files are written.

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

const patched =
  source.slice(0, closerAt) +
  'globalThis.__taskview = { renderSidebar: renderSidebar, state: state, STATUS_META: STATUS_META }; ' +
  source.slice(closerAt);

// ------------------------------------------------------------------ DOM

function makeEl(tag) {
  return {
    tagName: String(tag || 'div').toUpperCase(),
    innerHTML: '',
    textContent: '',
    value: '',
    className: '',
    id: '',
    style: {},
    dataset: {},
    attributes: {},
    children: [],
    classList: {
      names: new Set(),
      add: function () {},
      remove: function () {},
      toggle: function () {},
      contains: function () { return false; },
    },
    addEventListener: function () {},
    removeEventListener: function () {},
    dispatchEvent: function () {},
    appendChild: function (child) { this.children.push(child); return child; },
    removeChild: function (child) { return child; },
    insertBefore: function (node) { this.children.unshift(node); return node; },
    insertAdjacentHTML: function () {},
    setAttribute: function (key, value) { this.attributes[key] = String(value); },
    getAttribute: function (key) { return null; },
    removeAttribute: function () {},
    querySelector: function () { return makeEl('div'); },
    querySelectorAll: function () { return []; },
    closest: function () { return null; },
    contains: function () { return false; },
    getBoundingClientRect: function () {
      return { top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0 };
    },
    focus: function () {},
    blur: function () {},
    scrollIntoView: function () {},
    remove: function () {},
  };
}

const byId = new Map();
function elementFor(key) {
  if (!byId.has(key)) byId.set(key, makeEl('div'));
  return byId.get(key);
}

const documentStub = {
  readyState: 'loading',
  visibilityState: 'visible',
  activeElement: null,
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
  CSS: { escape: function (value) { return String(value); } },
  fetch: function () { throw new Error('unexpected network call in the render test'); },
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
  console.error('FAIL: the app script did not expose its render helpers');
  process.exit(1);
}

// --------------------------------------------------------------- projects

tv.state.projects = [
  {
    path: 'C:/work/alpha',
    name: 'alpha',
    total_tasks: 7,
    open_tasks: 5,
    backlog_tasks: 2,
    todo_tasks: 3,
    in_progress_tasks: 1,
  },
  {
    path: 'C:/work/beta',
    name: 'beta',
    total_tasks: 1,
    open_tasks: 1,
    backlog_tasks: 0,
    todo_tasks: 0,
    in_progress_tasks: 1,
  },
];
tv.state.loading = false;

tv.renderSidebar();
const list = byId.get('sel:#project-list').innerHTML;

// The three per-status counters of the first project, each with its dot.
const expected = [
  { tone: 'backlog', count: 2 },
  { tone: 'todo', count: 3 },
  { tone: 'progress', count: 1 },
];
expected.forEach(function (entry) {
  const pattern = 'rgb(var(--c-' + entry.tone + ')';
  if (list.indexOf(pattern) < 0) {
    failures.push('the project row lost the ' + entry.tone + ' dot');
  }
  if (list.indexOf('>' + entry.count + '</span>') < 0) {
    failures.push('the project row lost the ' + entry.tone + ' count ' + entry.count);
  }
});

// The old single counter is gone: no rounded pill with the open or total
// number next to the project name.
if (list.indexOf('bg-surface3 px-1.5 py-[1px] text-[10px] font-600 tabular-nums text-ink2') >= 0) {
  failures.push('the project row still shows the old single open-count badge');
}

// The zero counters stay visible but muted.
if (list.indexOf('rgb(var(--c-backlog) / .3)') < 0) {
  failures.push('a zero counter is not rendered muted');
}

// The rows keep their data attributes and the project names.
if (list.indexOf('data-project="C:/work/alpha"') < 0) {
  failures.push('the project row lost its data-project attribute');
}
if (list.indexOf('alpha') < 0 || list.indexOf('beta') < 0) {
  failures.push('the project row lost a project name');
}

// The "all projects" entry is untouched.
if (list.indexOf('全部项目') < 0) {
  failures.push('the "all projects" entry disappeared');
}

if (failures.length) {
  console.error('FAIL: ' + failures.length + ' assertion(s) failed');
  failures.forEach(function (failure) { console.error('  - ' + failure); });
  process.exit(1);
}

console.log('OK: the project list shows a coloured dot and a number per status');
