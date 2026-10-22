'use strict';
// Regression test for the rejection (驳回) mechanism in the web board.
//
// A rejected task carries a `rejection_reason` and is sent one step back:
//   review -> in_progress, in_progress -> backlog, todo -> backlog.
// The board has to make that visible:
//
//   * a card in `in_progress` or `backlog` that carries a reason is marked
//     with a warning badge and a tooltip that holds the full reason
//   * a card without a reason, or a card in any other state, is not marked
//   * the task editor shows the full reason in a block of its own
//   * a task in `review`, `in_progress` or `todo` offers a reject button
//     that asks for a reason and posts it to
//     /api/board/tasks/{id}/reject
//
// The front end is a single inline script, so the test loads it into a Node
// VM with a small DOM stub and asserts on the generated markup and on the
// requests the app would send.
//
// Usage: node scripts/rejection_test.js [path/to/index.html]
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
  'globalThis.__taskview = { cardHtml: cardHtml, renderList: renderList, renderBoard: renderBoard, ' +
  'openTaskModal: openTaskModal, openRejectModal: openRejectModal, performReject: performReject, ' +
  'rejectionReason: rejectionReason, canReject: canReject, showsRejectionBadge: showsRejectionBadge, ' +
  'rejectTargetOf: rejectTargetOf, rejectionBadgeHtml: rejectionBadgeHtml, ' +
  'state: state, listEl: listEl, boardEl: boardEl }; ' +
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

// Requests the app made during the test.
const requests = [];

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
  // The render helpers must never reach the network; only performReject does,
  // and it records the request instead of sending it.
  fetch: function (url, config) {
    const target = String(url);
    requests.push({ url: target, config: config || {} });
    let payload = {};
    const taskMatch = /\/tasks\/([^/]+)\/reject/.exec(target);
    if (taskMatch) {
      payload = { id: taskMatch[1], status: 'backlog', rejection_reason: '缺少测试' };
    } else if (target.indexOf('/stats') >= 0) {
      payload = { total: 1, backlog: 1, todo: 0, in_progress: 0, review: 0, done: 0, projects: 1 };
    } else if (target.indexOf('/projects') >= 0) {
      payload = [];
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
  console.error('FAIL: the app script did not expose its render helpers');
  process.exit(1);
}

// ------------------------------------------------------------------ task

function makeTask(id, status, reason, title) {
  return {
    id: id,
    title: title || ('任务 ' + id),
    description: '',
    status: status,
    priority: 'medium',
    rejection_reason: reason === undefined ? null : reason,
    project_name: 'TaskView',
    project_path: 'C:/workspace/taskview',
    created_at: 1700000000000,
    updated_at: 1700000000000,
  };
}

const REASON = '缺少单元测试，且接口签名与文档不一致';

// ------------------------------------------------------------- predicates

check(tv.rejectionReason(makeTask('T1', 'backlog', REASON)) === REASON, 'the rejection reason is not read');
check(tv.rejectionReason(makeTask('T1', 'backlog', null)) === '', 'a missing rejection reason is not empty');
check(tv.rejectionReason(makeTask('T1', 'backlog', '   ')) === '', 'a blank rejection reason is not empty');

// The two states a rejected task waits in carry the badge.
check(tv.showsRejectionBadge(makeTask('T1', 'in_progress', REASON)) === true, 'an in_progress task with a reason has no badge');
check(tv.showsRejectionBadge(makeTask('T1', 'backlog', REASON)) === true, 'a backlog task with a reason has no badge');
// Every other state stays clean.
['todo', 'review', 'done'].forEach(function (status) {
  check(tv.showsRejectionBadge(makeTask('T1', status, REASON)) === false, 'a ' + status + ' task with a reason is marked');
});
// A task without a reason is never marked.
check(tv.showsRejectionBadge(makeTask('T1', 'in_progress', null)) === false, 'a task without a reason is marked');
check(tv.showsRejectionBadge(makeTask('T1', 'in_progress', '  ')) === false, 'a task with a blank reason is marked');

// The reject button belongs to the three states the server accepts.
['todo', 'in_progress', 'review'].forEach(function (status) {
  check(tv.canReject(makeTask('T1', status, null)) === true, 'a ' + status + ' task cannot be rejected');
});
['backlog', 'done'].forEach(function (status) {
  check(tv.canReject(makeTask('T1', status, null)) === false, 'a ' + status + ' task can be rejected');
});

// The preview of the state transition matches the server.
check(tv.rejectTargetOf('review') === 'in_progress', 'review does not fall back to in_progress');
check(tv.rejectTargetOf('in_progress') === 'backlog', 'in_progress does not fall back to backlog');
check(tv.rejectTargetOf('todo') === 'backlog', 'todo does not fall back to backlog');
check(tv.rejectTargetOf('backlog') === null, 'backlog has a reject target');
check(tv.rejectTargetOf('done') === null, 'done has a reject target');

// ----------------------------------------------------------- board cards

const rejectedCard = tv.cardHtml(makeTask('T1', 'in_progress', REASON), 0);

check(rejectedCard.indexOf('reject-badge') >= 0, 'the rejected card has no warning badge');
check(rejectedCard.indexOf('已驳回') >= 0, 'the rejected card has no warning label');
check(rejectedCard.indexOf('is-rejected') >= 0, 'the rejected card is not highlighted');
check(rejectedCard.indexOf('title="' + REASON + '"') >= 0, 'the badge has no tooltip with the rejection reason');
check(rejectedCard.indexOf(REASON) >= 0, 'the rejected card does not show the reason');

// The reason is escaped, never injected as markup.
const nasty = tv.cardHtml(makeTask('T2', 'backlog', '<img src=x onerror=alert(1)>'), 0);
check(nasty.indexOf('<img src=x') < 0, 'the rejection reason is not escaped');
check(nasty.indexOf('&lt;img src=x') >= 0, 'the escaped rejection reason is missing');

// A card without a reason stays clean.
const cleanCard = tv.cardHtml(makeTask('T3', 'in_progress', null), 0);
check(cleanCard.indexOf('reject-badge') < 0, 'a task without a reason shows the warning badge');
check(cleanCard.indexOf('is-rejected') < 0, 'a task without a reason is highlighted');

// A task in another state keeps no badge even when it carries a reason.
['todo', 'review', 'done'].forEach(function (status) {
  const card = tv.cardHtml(makeTask('T4', status, REASON), 0);
  check(card.indexOf('reject-badge') < 0, 'a ' + status + ' card shows the warning badge');
  check(card.indexOf('is-rejected') < 0, 'a ' + status + ' card is highlighted');
});

// The reject button is offered for the three rejectable states.
['todo', 'in_progress', 'review'].forEach(function (status) {
  const card = tv.cardHtml(makeTask('T5', status, null), 0);
  check(card.indexOf('data-act="reject"') >= 0, 'the ' + status + ' card has no reject action');
});
// And for no other state.
['backlog', 'done'].forEach(function (status) {
  const card = tv.cardHtml(makeTask('T6', status, null), 0);
  check(card.indexOf('data-act="reject"') < 0, 'the ' + status + ' card offers a reject action');
});

// The other card actions survive.
check(rejectedCard.indexOf('data-act="advance"') >= 0, 'the rejected card lost the advance action');
check(rejectedCard.indexOf('data-act="edit"') >= 0, 'the rejected card lost the edit action');
check(rejectedCard.indexOf('data-act="delete"') >= 0, 'the rejected card lost the delete action');
check(rejectedCard.indexOf('draggable="true"') >= 0, 'the rejected card lost drag support');

// ----------------------------------------------------------- the board

tv.state.tasks = [
  makeTask('B1', 'in_progress', REASON, '被驳回的任务'),
  makeTask('B2', 'backlog', REASON, '在待整理里被驳回'),
  makeTask('B3', 'todo', null, '普通待办'),
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
check(board.indexOf('reject-badge') >= 0, 'the board renders no rejection badge at all');
check(board.split('reject-badge').length - 1 === 2, 'the board does not mark exactly the two rejected cards');

// --------------------------------------------------------- list view

tv.renderList();
const list = tv.listEl.innerHTML;
check(list.indexOf('reject-badge') >= 0, 'the list view renders no rejection badge');
check(list.indexOf('驳回：') >= 0, 'the list view does not show the rejection reason');
check(list.indexOf('data-act="reject"') >= 0, 'the list view has no reject action');

// -------------------------------------------------------- task editor

tv.openTaskModal(makeTask('E1', 'in_progress', REASON, '被驳回的任务'));
const modalRoot = byId.get('id:modal-root');
const editor = modalRoot.children[0] ? modalRoot.children[0].innerHTML : '';

check(editor.indexOf('data-rejection-block') >= 0, 'the task editor has no rejection block');
check(editor.indexOf('data-rejection-reason') >= 0, 'the task editor has no rejection reason element');
check(editor.indexOf('驳回原因') >= 0, 'the task editor has no rejection heading');
check(editor.indexOf(REASON) >= 0, 'the task editor does not show the full rejection reason');
check(editor.indexOf('data-reject-task') >= 0, 'the task editor has no reject button');

// The editor of a task without a reason carries neither the block nor the button.
tv.openTaskModal(makeTask('E2', 'in_progress', null));
const cleanEditor = modalRoot.children[1] ? modalRoot.children[1].innerHTML : '';
check(cleanEditor.indexOf('data-rejection-block') < 0, 'the task editor shows a rejection block without a reason');

// The reject button of the editor follows the same three states.
tv.openTaskModal(makeTask('E3', 'review', null));
const reviewEditor = modalRoot.children[2] ? modalRoot.children[2].innerHTML : '';
check(reviewEditor.indexOf('data-reject-task') >= 0, 'the editor of a review task has no reject button');

// --------------------------------------------------------- reject dialog

tv.openRejectModal(makeTask('R1', 'review', null, '待审查的任务'));
const dialog = modalRoot.children[3] ? modalRoot.children[3].innerHTML : '';

check(dialog.indexOf('data-reject-reason') >= 0, 'the reject dialog has no reason field');
check(dialog.indexOf('data-confirm-reject') >= 0, 'the reject dialog has no confirm button');
check(dialog.indexOf('驳回原因') >= 0, 'the reject dialog has no reason label');
check(dialog.indexOf('待审查验证') >= 0, 'the reject dialog does not show the current state');
check(dialog.indexOf('进行中') >= 0, 'the reject dialog does not show the target state');
check(dialog.indexOf('已驳回') < 0, 'the reject dialog shows the warning badge of a card');

// The dialog refuses an empty reason before it reaches the network.
tv.openRejectModal(makeTask('R2', 'todo', null));
const todoDialog = modalRoot.children[4] ? modalRoot.children[4].innerHTML : '';
check(todoDialog.indexOf('待办') >= 0, 'the reject dialog of a todo task does not show the current state');
check(todoDialog.indexOf('待整理') >= 0, 'the reject dialog of a todo task does not show the target state');

// A task that cannot be rejected never opens the dialog.
const before = modalRoot.children.length;
tv.openRejectModal(makeTask('R3', 'done', null));
check(modalRoot.children.length === before, 'the reject dialog opens for a task that cannot be rejected');

// ------------------------------------------------------------- the request

tv.state.tasks = [makeTask('P1', 'review', null, '待审查的任务')];
requests.length = 0;

const settled = tv.performReject(tv.state.tasks[0], '接口签名与文档不一致');

settled.then(function () {
  const rejectRequest = requests.filter(function (request) {
    return request.url.indexOf('/reject') >= 0;
  });
  check(rejectRequest.length === 1, 'the reject sent ' + rejectRequest.length + ' reject requests instead of one');
  const request = rejectRequest[0] || { url: '', config: {} };
  check(
    request.url === '/api/board/tasks/P1/reject',
    'the reject posted to ' + request.url + ' instead of /api/board/tasks/P1/reject'
  );
  check(request.config.method === 'POST', 'the reject did not use POST');
  check(
    request.config.body === JSON.stringify({ reason: '接口签名与文档不一致' }),
    'the reject body is ' + request.config.body
  );

  // The rejected task lands in the backlog and keeps the reason.
  const stored = tv.state.tasks[0];
  check(stored.status === 'backlog', 'the rejected task is not in the backlog');
  check(stored.rejection_reason === '缺少测试', 'the rejected task did not keep the reason from the server');

  if (failures.length) {
    console.error('FAIL: ' + failures.length + ' assertion(s) failed');
    failures.forEach(function (failure) { console.error('  - ' + failure); });
    process.exit(1);
  }
  console.log('OK: rejected tasks are marked on the board, listed in the editor and can be rejected through the API');
}).catch(function (err) {
  console.error('FAIL: the reject request failed: ' + err.message);
  process.exit(1);
});
