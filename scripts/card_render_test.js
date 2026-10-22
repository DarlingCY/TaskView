'use strict';
// Regression test for the board card rendering.
//
// The board card must no longer show the code-context link or the agent
// insight block, while the list view, the task editor and the code modal keep
// rendering both. The front end is a single inline script, so the test loads
// it into a Node VM with a small DOM stub and asserts on the generated markup.
//
// Usage: node scripts/card_render_test.js [path/to/index.html]
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

// The app script is the last inline <script> block without a src attribute.
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
  'globalThis.__taskview = { cardHtml: cardHtml, renderList: renderList, ' +
  'state: state, listEl: listEl, openTaskModal: openTaskModal, openCodeModal: openCodeModal }; ' +
  source.slice(closerAt);

// ------------------------------------------------------------------ DOM

// A DOM stub that is just rich enough to run the render helpers. It records
// the markup it is given so the assertions can look at it.
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
    // Descendants are stubbed: every lookup returns a fresh throwaway element
    // that is parented to this one, so wiring code can insert siblings.
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
  return el;
}

const byId = new Map();
const bySelector = new Map();
function elementFor(key, factory) {
  if (!byId.has(key)) byId.set(key, factory());
  return byId.get(key);
}

const documentStub = {
  readyState: 'loading',
  visibilityState: 'visible',
  activeElement: null,
  body: makeEl('body'),
  getElementById: function (id) { return elementFor('id:' + id, function () { return makeEl('div'); }); },
  querySelector: function (sel) { return elementFor('sel:' + sel, function () { return makeEl('div'); }); },
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
  // The render helpers must never reach the network.
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

// ------------------------------------------------------------------ task

const task = {
  id: 'T202610220002',
  title: '精简看板卡片渲染',
  description: '卡片只保留标题、描述与操作。',
  status: 'in_progress',
  priority: 'urgent',
  project_name: 'TaskView',
  project_path: 'C:/workspace/taskview',
  file_path: 'src/public/index.html',
  line_start: 120,
  line_end: 140,
  code_snippet: 'const a = 1;\nconst b = 2;',
  agent_notes: '这段洞察只应出现在编辑器与代码弹窗里。',
  created_at: 1700000000000,
  updated_at: 1700000000000,
};

// ----------------------------------------------------------- board card

const card = tv.cardHtml(task, 0);

check(card.indexOf('data-act="code"') < 0, 'board card still renders a code-context link');
check(card.indexOf('fileCode') < 0, 'board card still renders the file-code icon');
check(card.indexOf(task.file_path) < 0, 'board card still renders the file path');
check(card.indexOf('智能体洞察') < 0, 'board card still renders the agent insight heading');
check(card.indexOf(task.agent_notes) < 0, 'board card still renders the agent notes');

check(card.indexOf(task.title) >= 0, 'board card lost the title');
check(card.indexOf(task.description) >= 0, 'board card lost the description');
check(card.indexOf(task.id) >= 0, 'board card lost the task id');
check(card.indexOf(task.project_name) >= 0, 'board card lost the project name');
check(card.indexOf('draggable="true"') >= 0, 'board card lost drag support');
check(card.indexOf('role="button"') >= 0, 'board card lost its button role');
check(card.indexOf('tabindex="0"') >= 0, 'board card lost keyboard focus');
check(card.indexOf('data-act="advance"') >= 0, 'board card lost the advance action');
check(card.indexOf('data-act="edit"') >= 0, 'board card lost the edit action');
check(card.indexOf('data-act="delete"') >= 0, 'board card lost the delete action');

// A card without a file path or notes must render exactly the same way, so
// the removed blocks leave no empty spacing behind: the description slot is
// skipped and the card ends straight with the meta row.
const bareCard = tv.cardHtml({
  id: 'T202610220003',
  title: '没有上下文的卡片',
  status: 'todo',
  priority: 'medium',
  updated_at: 1700000000000,
}, 1);
check(bareCard.indexOf('</h3><div class="mt-2.5 flex items-center gap-1.5">') >= 0, 'board card grew an empty spacing slot');
check(bareCard.indexOf('智能体洞察') < 0, 'board card renders an agent insight without notes');

// ---------------------------------------------------------- list view

tv.state.tasks = [task];
tv.renderList();
const list = tv.listEl.innerHTML;

check(list.indexOf(task.file_path) >= 0, 'list view lost the file context column');
check(list.indexOf('data-act="code"') >= 0, 'list view lost the code action button');
check(list.indexOf('包含智能体备注') >= 0, 'list view lost the agent notes marker');
check(list.indexOf(task.title) >= 0, 'list view lost the title');

// ------------------------------------------------------- code modal

tv.openCodeModal(task);
const modalRoot = byId.get('id:modal-root');
const codeModal = modalRoot.children[0] ? modalRoot.children[0].innerHTML : '';

check(codeModal.indexOf('data-copy-code') >= 0, 'code modal lost the copy button');
check(codeModal.indexOf(task.file_path) >= 0, 'code modal lost the file path');
check(codeModal.indexOf('智能体备注') >= 0, 'code modal lost the agent notes block');
check(codeModal.indexOf(task.agent_notes) >= 0, 'code modal lost the agent notes text');
check(codeModal.indexOf(task.code_snippet.split('\n')[0]) >= 0, 'code modal lost the code snippet');

// -------------------------------------------------------- task editor

tv.openTaskModal(task);
const editor = modalRoot.children[1] ? modalRoot.children[1].innerHTML : '';

check(editor.indexOf('data-field="file_path"') >= 0, 'task editor lost the file path field');
check(editor.indexOf('data-field="agent_notes"') >= 0, 'task editor lost the agent notes field');
check(editor.indexOf(task.title) >= 0, 'task editor lost the title');

// ------------------------------------------------------------- result

if (failures.length) {
  console.error('FAIL: ' + failures.length + ' assertion(s) failed');
  failures.forEach(function (failure) { console.error('  - ' + failure); });
  process.exit(1);
}

console.log('OK: board card drops the code link and the agent insight; list, editor and code modal intact');
