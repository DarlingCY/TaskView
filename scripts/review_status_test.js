'use strict';
// Regression test for the "待审查验证" (review) task status.
//
// The board has five columns: backlog → todo → in_progress → review → done.
// The review state sits between "in progress" and "done": an implementation is
// finished but still waits for a verification pass.
//
// The test covers the whole front end contract of the new state:
//   * the status order and the Chinese label
//   * the advance chain in_progress → review → done, and done reopens as todo
//   * the board keeps a review column and does not drop review tasks
//   * the status select of the task editor offers all five states
//   * the sidebar, the stats legend and the list view know about review
//   * a review card is not struck through (only done is)
//
// The front end is a single inline script, so the test loads it into a Node
// VM with a small DOM stub and asserts on the generated markup.
//
// Usage: node scripts/review_status_test.js [path/to/index.html]
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

// Expose the render helpers to the test without touching the app itself.
const patched =
  source.slice(0, closerAt) +
  'globalThis.__taskview = { renderSidebar: renderSidebar, renderStats: renderStats, ' +
  'renderBoard: renderBoard, renderList: renderList, cardHtml: cardHtml, ' +
  'statusSelect: statusSelect, nextStatusOf: nextStatusOf, openTaskModal: openTaskModal, ' +
  'state: state, boardEl: boardEl, listEl: listEl, ' +
  'STATUS_ORDER: STATUS_ORDER, STATUS_META: STATUS_META }; ' +
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
    hidden: false,
    disabled: false,
    tabIndex: 0,
    style: {},
    dataset: {},
    attributes: {},
    children: [],
    options: [],
    selectedIndex: -1,
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
    appendChild: function (child) { this.children.push(child); return child; },
    removeChild: function (child) {
      this.children = this.children.filter(function (c) { return c !== child; });
      return child;
    },
    insertBefore: function (node) { this.children.unshift(node); return node; },
    insertAdjacentHTML: function () {},
    setAttribute: function (key, value) { this.attributes[key] = String(value); },
    getAttribute: function (key) {
      return Object.prototype.hasOwnProperty.call(this.attributes, key) ? this.attributes[key] : null;
    },
    removeAttribute: function (key) { delete this.attributes[key]; },
    querySelector: function () { const child = makeEl('div'); child.parentNode = this; return child; },
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

// ------------------------------------------------------ order and labels

// The pipeline order is fixed: review sits between in_progress and done.
const order = ['backlog', 'todo', 'in_progress', 'review', 'done'];
const actual = tv.STATUS_ORDER;
check(
  JSON.stringify(actual) === JSON.stringify(order),
  'the status order is ' + JSON.stringify(actual) + ', want ' + JSON.stringify(order)
);

const meta = tv.STATUS_META;
check(!!meta.review, 'the review status has no metadata');
check(meta.review && meta.review.label === '待审查验证', 'the review label is not 待审查验证');
check(meta.review && meta.review.short, 'the review status has no short label');
check(meta.review && !!meta.review.icon, 'the review status has no icon');
check(meta.review && !!meta.review.tone, 'the review status has no tone');
check(meta.review && !!meta.review.empty, 'the review status has no empty-state text');
check(meta.review && !!meta.review.hint, 'the review status has no hint text');
// The existing status must not be renamed.
check(meta.in_progress && meta.in_progress.label === '进行中', 'the in_progress label was changed');
check(meta.todo && meta.todo.label === '待办', 'the todo label was changed');
check(meta.backlog && meta.backlog.label === '待整理', 'the backlog label was changed');
check(meta.done && meta.done.label === '已完成', 'the done label was changed');

// Every status needs its own colour token in both themes.
const root = html.slice(html.indexOf(':root {'), html.indexOf('.dark {'));
const dark = html.slice(html.indexOf('.dark {'), html.indexOf('html, body'));
check(/--c-review:\s*\d+\s+\d+\s+\d+;/.test(root), 'the light theme has no --c-review token');
check(/--c-review:\s*\d+\s+\d+\s+\d+;/.test(dark), 'the dark theme has no --c-review token');
check(/review: 'rgb\(var\(--c-review\)/.test(html), 'the tailwind theme has no review colour');

// ------------------------------------------------------------- advancing

check(tv.nextStatusOf('backlog') === 'todo', 'backlog does not advance to todo');
check(tv.nextStatusOf('todo') === 'in_progress', 'todo does not advance to in_progress');
check(tv.nextStatusOf('in_progress') === 'review', 'in_progress does not advance to review');
check(tv.nextStatusOf('review') === 'done', 'review does not advance to done');
check(tv.nextStatusOf('done') === 'todo', 'done does not reopen as todo');
check(tv.nextStatusOf('bogus') === 'todo', 'an unknown status does not fall back to todo');

// ------------------------------------------------------------------ board

function makeTask(id, status, title) {
  return {
    id: id,
    title: title || ('任务 ' + id),
    description: '',
    status: status,
    priority: 'medium',
    project_name: 'TaskView',
    project_path: 'C:/workspace/taskview',
    created_at: 1700000000000,
    updated_at: 1700000000000,
  };
}

tv.state.tasks = [
  makeTask('T1', 'backlog', '待整理任务'),
  makeTask('T2', 'todo', '待办任务'),
  makeTask('T3', 'in_progress', '进行中任务'),
  makeTask('T4', 'review', '待审查任务'),
  makeTask('T5', 'done', '已完成任务'),
];
tv.state.loading = false;
tv.state.error = null;
tv.state.projectPath = null;
tv.state.priorities.clear();
tv.state.search = '';

let board = '';
try {
  tv.renderBoard();
  board = tv.boardEl.innerHTML;
} catch (err) {
  failures.push('the board failed to render: ' + err.message);
}

// Every status owns a column, in the pipeline order.
const columnOrder = [];
const columnRe = /data-status="([a-z_]+)"/g;
let columnMatch;
while ((columnMatch = columnRe.exec(board)) !== null) columnOrder.push(columnMatch[1]);
check(
  JSON.stringify(columnOrder) === JSON.stringify(order),
  'the board columns are ' + JSON.stringify(columnOrder) + ', want ' + JSON.stringify(order)
);

// The review column carries the full Chinese label.
check(board.indexOf('待审查验证') >= 0, 'the board has no 待审查验证 column heading');

// A review task must land in the review column and not in another one.
const reviewColumn = board.slice(board.indexOf('data-status="review"'));
const reviewListStart = reviewColumn.indexOf('data-cardlist="review"');
const reviewListEnd = reviewColumn.indexOf('data-cardlist="done"');
const reviewColumnHtml = reviewColumn.slice(reviewListStart, reviewListEnd);
check(reviewColumnHtml.indexOf('待审查任务') >= 0, 'the review task is not in the review column');
check(reviewColumnHtml.indexOf('T4') >= 0, 'the review task lost its id in the review column');
// The other tasks must not have leaked into the review column.
['待整理任务', '待办任务', '进行中任务', '已完成任务'].forEach(function (title) {
  check(reviewColumnHtml.indexOf(title) < 0, 'the review column swallowed the task ' + title);
});

// Every task is rendered exactly once on the board.
['T1', 'T2', 'T3', 'T4', 'T5'].forEach(function (id) {
  const hits = board.split('data-task-id="' + id + '"').length - 1;
  check(hits === 1, 'the task ' + id + ' is rendered ' + hits + ' times instead of once');
});

// The column counter of the review column is right.
const reviewCount = /data-count-for="review"[^>]*>(\d+)</.exec(board);
check(!!reviewCount, 'the review column has no counter');
check(reviewCount && reviewCount[1] === '1', 'the review column counts ' + (reviewCount && reviewCount[1]) + ' instead of 1');

// ----------------------------------------------------------- board cards

const reviewCard = tv.cardHtml(makeTask('T9', 'review', '待审查卡片'), 0);

// A task that waits for verification is not finished yet, so it is not struck
// through. Only done carries the line-through.
check(reviewCard.indexOf('line-through') < 0, 'the review card is struck through');

const doneCard = tv.cardHtml(makeTask('T10', 'done', '已完成卡片'), 0);
check(doneCard.indexOf('line-through') >= 0, 'the done card lost its line-through');

// The advance button of a review card points at done.
check(
  reviewCard.indexOf('推进至 已完成') >= 0,
  'the review card does not offer the advance to 已完成'
);
check(
  reviewCard.indexOf('data-act="advance"') >= 0,
  'the review card lost the advance action'
);

// The review card announces its status to screen readers.
check(
  reviewCard.indexOf('aria-label="待审查卡片 · 待审查验证 · 中优先级"') >= 0,
  'the review card does not announce 待审查验证'
);

// ------------------------------------------------------------ list view

tv.renderList();
const list = tv.listEl.innerHTML;
check(list.indexOf('待审查验证') >= 0, 'the list view has no 待审查验证 status chip');
check(list.indexOf('待审查任务') >= 0, 'the list view lost the review task');
const reviewChip = /data-status="review"[\s\S]{0,400}?待审查验证/.test(list);
check(reviewChip || list.indexOf('待审查验证') >= 0, 'the list view has no review status chip');

// ---------------------------------------------------------- status select

const statusSelect = tv.statusSelect('review');

// All five states are selectable.
order.forEach(function (status) {
  check(
    statusSelect.indexOf('value="' + status + '"') >= 0,
    'the status select has no option for ' + status
  );
});
check(statusSelect.indexOf('value="in_progress"') >= 0, 'the status select lost in_progress');
check(statusSelect.indexOf('value="review"') >= 0, 'the status select lost review');
check(statusSelect.indexOf('value="done"') >= 0, 'the status select lost done');

// The select is a native one, hidden behind the custom listbox control.
check(/<select id="f-status" data-field="status"/.test(statusSelect), 'the status select lost its id or its data-field');
check(/class="hidden"/.test(statusSelect), 'the status select is not hidden');
check(/tabindex="-1"/.test(statusSelect), 'the status select is still focusable');

// The active state is the one that was asked for.
check(
  /<option value="review" selected>/.test(statusSelect),
  'the review option is not marked as selected'
);
check(
  !/<option value="todo" selected>/.test(statusSelect),
  'the todo option is marked as selected although review is selected'
);

// The long label is rendered in full, not shortened.
check(statusSelect.indexOf('待审查验证') >= 0, 'the status select does not show the full 待审查验证 label');
check(statusSelect.indexOf('>待审查验证</option>') >= 0, 'the review option does not end with the full label');

// The other statuses keep their labels.
['待整理', '待办', '进行中', '已完成'].forEach(function (label) {
  check(statusSelect.indexOf('>' + label + '</option>') >= 0, 'the status select lost the label ' + label);
});

// The label is never truncated in the markup: the option holds the whole
// label, so the custom listbox can render it in full.
const reviewOption = /<option value="review"([^>]*)>([\s\S]*?)<\/option>/.exec(statusSelect);
check(!!reviewOption, 'the review option is not in the status select');
if (reviewOption) {
  check(reviewOption[2].trim() === '待审查验证', 'the review option shows "' + reviewOption[2].trim() + '" instead of the full label');
}

// The task editor renders the select and preselects the review status.
tv.openTaskModal(makeTask('T11', 'review', '编辑器任务'));
const modalRoot = byId.get('id:modal-root');
const editor = modalRoot.children[0] ? modalRoot.children[0].innerHTML : '';
check(editor.indexOf('data-field="status"') >= 0, 'the task editor has no status select');
check(editor.indexOf('<select id="f-status"') >= 0, 'the task editor has no #f-status select');
check(editor.indexOf('待审查验证') >= 0, 'the task editor has no 待审查验证 option');
check(
  /<option value="review" selected>/.test(editor),
  'the task editor does not preselect the review status'
);
check(
  !/<option value="todo" selected>/.test(editor),
  'the task editor preselects todo although the task waits for a review'
);

// The segmented control is gone for good.
check(editor.indexOf('data-segment') < 0, 'the task editor still renders a segmented control');
check(source.indexOf('statusSegment') < 0, 'the source still defines statusSegment');
check(source.indexOf('prioritySegment') < 0, 'the source still defines prioritySegment');

// --------------------------------------------------------------- sidebar

tv.state.projects = [
  {
    path: 'C:/work/alpha',
    name: 'alpha',
    total_tasks: 6,
    open_tasks: 4,
    backlog_tasks: 1,
    todo_tasks: 1,
    in_progress_tasks: 1,
    review_tasks: 1,
  },
];
tv.renderSidebar();
const sidebar = byId.get('sel:#project-list').innerHTML;
check(sidebar.indexOf('rgb(var(--c-review)') >= 0, 'the project row has no review counter dot');
check(sidebar.indexOf('待审查验证：1') >= 0, 'the project row has no review counter tooltip');

// ----------------------------------------------------------------- stats

tv.state.stats = {
  total: 6,
  backlog: 1,
  todo: 1,
  in_progress: 1,
  review: 2,
  done: 1,
  projects: 1,
};
tv.renderStats();
const legend = byId.get('sel:#stats-legend').innerHTML;
check(legend.indexOf('data-status="review"') >= 0, 'the stats legend has no review entry');
check(legend.indexOf('待审查') >= 0, 'the stats legend has no review label');
check(legend.indexOf('rgb(var(--c-review)') >= 0, 'the stats legend has no review dot');

const bar = byId.get('sel:#stats-bar').innerHTML;
check(bar.indexOf('rgb(var(--c-review)') >= 0, 'the stats bar has no review segment');
check(
  /background:rgb\(var\(--c-review\)\)" title="待审查验证: 2"/.test(bar),
  'the stats bar does not show the review share'
);

// The legend entry of the review state can be used to jump to its column:
// the board renders a .col-wrap[data-status="review"] for it.
check(
  /<div class="flex min-w-\[\d+px\] flex-1 flex-col[^"]*col-wrap" data-status="review"/.test(board),
  'the board has no review column to jump to'
);

// --------------------------------------------------------------- result

if (failures.length) {
  console.error('FAIL: ' + failures.length + ' assertion(s) failed');
  failures.forEach(function (failure) { console.error('  - ' + failure); });
  process.exit(1);
}

console.log('OK: the review status sits between in_progress and done across the board, list, editor, sidebar and stats');
