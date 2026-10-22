'use strict';
// Regression test for data fingerprinting and DOM re-render skipping in TaskView.
//
// The client maintains a fingerprint of the task collection (based on id,
// updated_at, status, and priority). During background 60s periodic polling or
// window tab refocusing (loadAll({ silent: true })), if the fetched task data
// has not changed, renderBoard() and renderList() must be skipped to avoid
// rebuilding the DOM tree, preventing layout reflow, loss of focus, and card flicker.
//
// Usage: node scripts/dom_fingerprint_test.js [path/to/index.html]
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

// Expose the render helpers and fingerprinting to the test sandbox.
const patched =
  source.slice(0, closerAt) +
  'globalThis.__taskview = { ' +
  'computeTasksFingerprint: computeTasksFingerprint, ' +
  'loadAll: loadAll, ' +
  'renderAll: renderAll, ' +
  'renderBoard: renderBoard, ' +
  'renderList: renderList, ' +
  'state: state, ' +
  'boardEl: boardEl, ' +
  'listEl: listEl ' +
  '}; ' +
  source.slice(closerAt);

// ------------------------------------------------------------------ DOM Stub

function makeEl(tag) {
  const el = {
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
  return el;
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

let mockTasks = [];
let mockProjects = [];
let mockNetworkFailure = false;
let mockStats = { total: 0, backlog: 0, todo: 0, in_progress: 0, review: 0, done: 0 };

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
    if (mockNetworkFailure) return Promise.reject(new Error('connection refused'));
    const target = String(url);
    let payload = [];
    if (target.indexOf('/api/board/projects') >= 0) {
      payload = mockProjects;
    } else if (target.indexOf('/api/board/stats') >= 0) {
      payload = mockStats;
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

// -------------------------------------------------- 1. Fingerprint Unit Tests

const t1 = { id: 'T1', updated_at: 1000, status: 'todo', priority: 'medium' };
const t2 = { id: 'T2', updated_at: 2000, status: 'in_progress', priority: 'high' };
const t3 = { id: 'T3', updated_at: 3000, status: 'review', priority: 'urgent' };

const fpEmpty = tv.computeTasksFingerprint([]);
const fp1 = tv.computeTasksFingerprint([t1, t2]);
const fp1Reversed = tv.computeTasksFingerprint([t2, t1]);

check(fpEmpty === '0:empty', 'empty task list did not produce 0:empty');
check(fp1 === fp1Reversed, 'fingerprint is not order-independent: ' + fp1 + ' vs ' + fp1Reversed);

// Mutation: status change
const t1StatusChanged = Object.assign({}, t1, { status: 'in_progress' });
const fpStatus = tv.computeTasksFingerprint([t1StatusChanged, t2]);
check(fpStatus !== fp1, 'fingerprint failed to detect status mutation');

// Mutation: updated_at change
const t1TimestampChanged = Object.assign({}, t1, { updated_at: 1001 });
const fpTime = tv.computeTasksFingerprint([t1TimestampChanged, t2]);
check(fpTime !== fp1, 'fingerprint failed to detect updated_at timestamp mutation');

// Mutation: priority change
const t1PriorityChanged = Object.assign({}, t1, { priority: 'urgent' });
const fpPrio = tv.computeTasksFingerprint([t1PriorityChanged, t2]);
check(fpPrio !== fp1, 'fingerprint failed to detect priority mutation');

// Mutation: item added
const fpAdded = tv.computeTasksFingerprint([t1, t2, t3]);
check(fpAdded !== fp1, 'fingerprint failed to detect added item');

// ------------------------------------------------ 2. loadAll Skipping Behavior

mockTasks = [t1, t2];
mockProjects = [{ path: 'C:/test', name: 'Test', total_tasks: 2 }];
mockStats = { total: 2, backlog: 0, todo: 1, in_progress: 1, review: 0, done: 0 };

let boardRenderCount = 0;
let currentHtml = '';
Object.defineProperty(tv.boardEl, 'innerHTML', {
  get: function () { return currentHtml; },
  set: function (v) {
    boardRenderCount += 1;
    currentHtml = v;
  },
  configurable: true,
});

// First load: initial render sets skeleton loader then renders board
tv.loadAll().then(function () {
  check(boardRenderCount === 2, 'initial loadAll did not set skeleton and board (count: ' + boardRenderCount + ')');
  check(tv.state.viewRendered === true, 'state.viewRendered was not set to true after initial load');
  check(tv.state.tasksFingerprint === fp1, 'state.tasksFingerprint did not match tasks fingerprint');

  const countBeforeSilent = boardRenderCount;
  // Second load: silent refresh with identical tasks -> MUST SKIP renderBoard (count must not change)
  return tv.loadAll({ silent: true }).then(function () {
    return countBeforeSilent;
  });
}).then(function (prevCount) {
  check(boardRenderCount === prevCount, 'silent loadAll with unchanged data did NOT skip renderBoard (count: ' + boardRenderCount + ' vs ' + prevCount + ')');

  const countBeforeUpdate = boardRenderCount;
  // Third load: silent refresh with modified task (status updated) -> MUST TRIGGER renderBoard (count + 1)
  mockTasks = [Object.assign({}, t1, { status: 'in_progress', updated_at: 1005 }), t2];
  return tv.loadAll({ silent: true }).then(function () {
    return countBeforeUpdate;
  });
}).then(function (prevCount) {
  check(boardRenderCount === prevCount + 1, 'silent loadAll with modified task did NOT trigger renderBoard (count: ' + boardRenderCount + ' vs ' + (prevCount + 1) + ')');

  const countBeforeForce = boardRenderCount;
  // Fourth load: silent refresh with force: true -> MUST TRIGGER renderBoard even with unchanged data (count + 1)
  return tv.loadAll({ silent: true, force: true }).then(function () {
    return countBeforeForce;
  });
}).then(function (prevCount) {
  check(boardRenderCount === prevCount + 1, 'loadAll({ force: true }) did NOT force renderBoard (count: ' + boardRenderCount + ' vs ' + (prevCount + 1) + ')');
}).then(async function () {
  // An error panel replaces the rendered view even if task data stays unchanged.
  for (const view of ['board', 'list']) {
    tv.state.view = view;
    tv.renderAll();
    const element = view === 'board' ? tv.boardEl : tv.listEl;
    const expectedHtml = element.innerHTML;

    mockNetworkFailure = true;
    await tv.loadAll({ silent: true });
    check(element.innerHTML.indexOf('连接断开') >= 0, view + ' did not show the network error panel');
    check(tv.state.viewRendered === false, view + ' error panel was treated as rendered task data');

    mockNetworkFailure = false;
    await tv.loadAll({ silent: true });
    check(tv.state.error === null, view + ' did not clear the network error after recovery');
    check(element.innerHTML === expectedHtml, view + ' did not restore unchanged tasks after silent recovery');
    check(tv.state.viewRendered === true, view + ' was not marked rendered after recovery');
  }

  if (failures.length) {
    console.error('FAIL: ' + failures.length + ' assertion(s) failed');
    failures.forEach(function (f) { console.error('  - ' + f); });
    process.exit(1);
  }
  console.log('OK: data fingerprinting correctly identifies changes and skips redundant board DOM re-renders');
}).catch(function (err) {
  console.error('FAIL: error during fingerprint test:', err);
  process.exit(1);
});
