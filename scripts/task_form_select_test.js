'use strict';
// Regression test for the task editor.
//
// It covers two contracts of the task form:
//
// 1. The status / priority selects.
//
// The new/edit task modal used to offer the status and the priority as two
// segmented controls (a row of buttons per field). Both were replaced by two
// native <select> elements that sit next to each other in a single row:
//
//   <div class="grid grid-cols-2 gap-4" data-task-selects>
//     <div class="min-w-0">
//       <label class="label" for="f-status-trigger">状态</label>
//       <select id="f-status" data-field="status" class="hidden" tabindex="-1" aria-label="状态">…</select>
//     </div>
//     <div class="min-w-0">
//       <label class="label" for="f-priority-trigger">优先级</label>
//       <select id="f-priority" data-field="priority" class="hidden" tabindex="-1" aria-label="优先级">…</select>
//     </div>
//   </div>
//
// The test pins the whole contract down:
//   * a new task still defaults to todo / medium
//   * both selects live in one row, side by side
//   * the status select offers all five states, the priority select all four
//   * editing a task preselects its own status and priority
//   * picking another state (mouse or keyboard) reaches the POST / PATCH body
//   * saving without touching the selects keeps the original values
//   * the segmented control is really gone from the source
//
// 2. The code context row.
//
// The file path and the two line numbers used to sit in two separate blocks.
// They now share one row, so a long path can no longer push the line numbers
// out of the card:
//
//   <div class="task-code-fields" data-code-fields>
//     <div class="min-w-0">… <input id="f-file" data-field="file_path" /></div>
//     <div class="min-w-0">… <input id="f-line-start" data-field="line_start" type="number" min="0" /></div>
//     <div class="min-w-0">… <input id="f-line-end" data-field="line_end" type="number" min="0" /></div>
//   </div>
//
// The path comes first, the two numbers follow. The test pins down:
//   * the new task and the edit share the very same row
//   * the row holds three min-w-0 cells in the order path, start, end
//   * the labels and the numeric attributes (type="number" min="0") are kept
//   * a new task starts with an empty code context, an edit shows its own
//   * the code context reaches the POST / PATCH body
//   * empty fields clear the code context to null
//   * a negative start / end line and a reversed range write nothing
//   * the stylesheet still carries the grid of the row
//
// The front end is a single inline script, so the test loads it into a Node
// VM with a small DOM implementation and drives the *real* wireTaskForm and
// initTselect. Every request is answered by a stub, so the test never talks
// to a TaskView server and never reads a real task.
//
// Usage: node scripts/task_form_select_test.js [path/to/index.html]
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

// Expose the editor helpers to the test without touching the app itself.
const patched =
  source.slice(0, closerAt) +
  'globalThis.__taskview = { openTaskModal: openTaskModal, closeModal: closeModal, ' +
  'closeAllModals: closeAllModals, wireTaskForm: wireTaskForm, initTselect: initTselect, ' +
  'performDelete: performDelete, ' +
  'state: state, ' +
  'STATUS_ORDER: STATUS_ORDER, STATUS_META: STATUS_META, ' +
  'PRIORITY_ORDER: PRIORITY_ORDER, PRIORITY_META: PRIORITY_META }; ' +
  source.slice(closerAt);

// ------------------------------------------------------------- mini DOM
//
// The editor is a plain HTML form, but the app wires it up through a custom
// select (initTselect) that moves the native <select> into a wrapper and
// drives it through a listbox with real events. Testing that wiring needs a
// DOM that understands selectors, attributes, options and events. This is a
// deliberately small implementation of exactly that — no dependencies.

const VOID_TAGS = new Set([
  'area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input',
  'link', 'meta', 'param', 'source', 'track', 'wbr',
]);

function decodeEntities(text) {
  return String(text)
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&apos;/g, "'")
    .replace(/&amp;/g, '&');
}

function escapeAttribute(value) {
  return String(value)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function createTextNode(text) {
  return {
    nodeType: 3,
    nodeName: '#text',
    textContent: String(text),
    childNodes: [],
    parentNode: null,
  };
}

function makeClassList(el) {
  const list = {
    _names: function () {
      return el.className.split(/\s+/).filter(Boolean);
    },
    add: function () {
      const names = list._names();
      Array.prototype.forEach.call(arguments, function (name) {
        if (names.indexOf(name) < 0) names.push(name);
      });
      el.className = names.join(' ');
    },
    remove: function () {
      const drop = Array.prototype.slice.call(arguments);
      el.className = list._names().filter(function (name) { return drop.indexOf(name) < 0; }).join(' ');
    },
    toggle: function (name, force) {
      const on = force === undefined ? !list.contains(name) : !!force;
      if (on) list.add(name); else list.remove(name);
      return on;
    },
    contains: function (name) { return list._names().indexOf(name) >= 0; },
  };
  return list;
}

function makeDataset(el) {
  const toAttribute = function (prop) {
    return 'data-' + String(prop).replace(/[A-Z]/g, function (m) { return '-' + m.toLowerCase(); });
  };
  return new Proxy({}, {
    get: function (target, prop) {
      if (typeof prop !== 'string') return undefined;
      const value = el.getAttribute(toAttribute(prop));
      return value === null ? undefined : value;
    },
    set: function (target, prop, value) {
      el.setAttribute(toAttribute(prop), value);
      return true;
    },
    has: function (target, prop) {
      return typeof prop === 'string' && el.hasAttribute(toAttribute(prop));
    },
  });
}

class MiniElement {
  constructor(tag) {
    this.nodeType = 1;
    this.nodeName = String(tag).toLowerCase();
    this.tagName = this.nodeName.toUpperCase();
    this.attributes = Object.create(null);
    this.childNodes = [];
    this.parentNode = null;
    this.style = {};
    this._listeners = Object.create(null);
    this._value = undefined;
    this._selectedIndex = undefined;
    this.classList = makeClassList(this);
    this.dataset = makeDataset(this);
  }

  get children() { return this.childNodes.filter(function (n) { return n.nodeType === 1; }); }
  get firstChild() { return this.childNodes[0] || null; }

  get id() { return this.getAttribute('id') || ''; }
  set id(value) { this.setAttribute('id', value); }

  get className() { return this.getAttribute('class') || ''; }
  set className(value) { this.setAttribute('class', value); }

  get type() { return this.getAttribute('type') || ''; }
  set type(value) { this.setAttribute('type', value); }

  get title() { return this.getAttribute('title') || ''; }
  set title(value) { this.setAttribute('title', value); }

  get disabled() { return this.hasAttribute('disabled'); }
  set disabled(value) {
    if (value) this.setAttribute('disabled', ''); else this.removeAttribute('disabled');
  }

  get hidden() { return this.hasAttribute('hidden'); }
  set hidden(value) {
    if (value) this.setAttribute('hidden', ''); else this.removeAttribute('hidden');
  }

  get tabIndex() {
    const raw = this.getAttribute('tabindex');
    return raw === null ? -1 : Number(raw);
  }
  set tabIndex(value) { this.setAttribute('tabindex', String(value)); }

  get options() {
    return this.childNodes.filter(function (n) { return n.nodeType === 1 && n.nodeName === 'option'; });
  }

  get selectedIndex() {
    if (this._selectedIndex !== undefined) return this._selectedIndex;
    const opts = this.options;
    for (let i = 0; i < opts.length; i++) {
      if (opts[i].hasAttribute('selected')) return i;
    }
    return -1;
  }
  set selectedIndex(value) { this._selectedIndex = Number(value); }

  get value() {
    if (this.nodeName === 'select') {
      const option = this.options[this.selectedIndex];
      return option ? option.value : '';
    }
    if (this.nodeName === 'textarea') return this.textContent;
    if (this._value !== undefined) return this._value;
    const raw = this.getAttribute('value');
    return raw === null ? '' : raw;
  }
  set value(value) {
    if (this.nodeName === 'select') {
      const wanted = String(value);
      const opts = this.options;
      let index = -1;
      for (let i = 0; i < opts.length; i++) {
        if (opts[i].value === wanted) { index = i; break; }
      }
      this._selectedIndex = index;
      return;
    }
    this._value = String(value);
  }

  get textContent() {
    let out = '';
    this.childNodes.forEach(function (child) { out += child.textContent; });
    return out;
  }
  set textContent(value) {
    this.childNodes = [];
    if (value !== '' && value !== null && value !== undefined) {
      this.appendChild(createTextNode(String(value)));
    }
  }

  get innerHTML() {
    return this.childNodes.map(serializeNode).join('');
  }
  set innerHTML(value) {
    this.childNodes = [];
    const self = this;
    parseFragment(String(value)).forEach(function (node) { self.appendChild(node); });
  }

  getAttribute(name) {
    const key = String(name).toLowerCase();
    return this.hasAttribute(key) ? this.attributes[key] : null;
  }
  setAttribute(name, value) { this.attributes[String(name).toLowerCase()] = String(value); }
  hasAttribute(name) {
    return Object.prototype.hasOwnProperty.call(this.attributes, String(name).toLowerCase());
  }
  removeAttribute(name) { delete this.attributes[String(name).toLowerCase()]; }

  appendChild(node) {
    if (node.parentNode) node.parentNode.removeChild(node);
    this.childNodes.push(node);
    node.parentNode = this;
    return node;
  }
  removeChild(node) {
    const index = this.childNodes.indexOf(node);
    if (index >= 0) this.childNodes.splice(index, 1);
    node.parentNode = null;
    return node;
  }
  insertBefore(node, ref) {
    if (node.parentNode) node.parentNode.removeChild(node);
    const index = ref ? this.childNodes.indexOf(ref) : -1;
    if (index < 0) this.childNodes.push(node); else this.childNodes.splice(index, 0, node);
    node.parentNode = this;
    return node;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }

  contains(node) {
    let cursor = node;
    while (cursor) {
      if (cursor === this) return true;
      cursor = cursor.parentNode;
    }
    return false;
  }

  closest(selector) {
    const groups = parseSelector(selector);
    let cursor = this;
    while (cursor && cursor.nodeType === 1) {
      if (matchesAny(cursor, groups)) return cursor;
      cursor = cursor.parentNode;
    }
    return null;
  }

  querySelector(selector) {
    const found = findAll(this, parseSelector(selector));
    return found.length ? found[0] : null;
  }
  querySelectorAll(selector) { return findAll(this, parseSelector(selector)); }

  addEventListener(type, handler) {
    if (!this._listeners[type]) this._listeners[type] = [];
    this._listeners[type].push(handler);
  }
  removeEventListener(type, handler) {
    const list = this._listeners[type];
    if (!list) return;
    const index = list.indexOf(handler);
    if (index >= 0) list.splice(index, 1);
  }
  dispatchEvent(event) { return dispatchEvent(this, event); }

  insertAdjacentHTML(position, value) {
    const nodes = parseFragment(String(value));
    const self = this;
    nodes.forEach(function (node) {
      if (position === 'beforeend') self.appendChild(node);
      else self.insertBefore(node, self.firstChild);
    });
  }

  focus() {}
  blur() {}
  scrollIntoView() {}
  getBoundingClientRect() {
    return { top: 100, bottom: 130, left: 20, right: 200, width: 180, height: 30 };
  }
  get offsetHeight() { return 200; }
  get scrollHeight() { return 200; }
}

function serializeNode(node) {
  if (node.nodeType === 3) return node.textContent;
  let out = '<' + node.nodeName;
  Object.keys(node.attributes).forEach(function (key) {
    out += ' ' + key + '="' + escapeAttribute(node.attributes[key]) + '"';
  });
  if (VOID_TAGS.has(node.nodeName)) return out + ' />';
  out += '>';
  node.childNodes.forEach(function (child) { out += serializeNode(child); });
  return out + '</' + node.nodeName + '>';
}

// --------------------------------------------------------------- selectors

function parseSelector(selector) {
  return String(selector)
    .split(',')
    .map(function (part) { return part.trim(); })
    .filter(Boolean)
    .map(function (part) {
      const parts = [];
      const re = /([a-zA-Z][\w-]*)|\.([\w-]+)|#([\w-]+)|\[([\w-]+)(?:=(?:"([^"]*)"|'([^']*)'|([^\]]*)))?\]/g;
      let hit;
      while ((hit = re.exec(part)) !== null) {
        if (hit[1]) parts.push({ kind: 'tag', value: hit[1].toLowerCase() });
        else if (hit[2]) parts.push({ kind: 'class', value: hit[2] });
        else if (hit[3]) parts.push({ kind: 'id', value: hit[3] });
        else if (hit[4]) {
          let value = null;
          if (hit[5] !== undefined) value = hit[5];
          else if (hit[6] !== undefined) value = hit[6];
          else if (hit[7] !== undefined) value = hit[7];
          parts.push({ kind: 'attr', name: hit[4].toLowerCase(), value: value });
        }
      }
      return parts;
    });
}

function matchesGroup(el, group) {
  for (let i = 0; i < group.length; i++) {
    const part = group[i];
    if (part.kind === 'tag') {
      if (el.nodeName !== part.value) return false;
    } else if (part.kind === 'id') {
      if (el.getAttribute('id') !== part.value) return false;
    } else if (part.kind === 'class') {
      if (!el.classList.contains(part.value)) return false;
    } else if (part.kind === 'attr') {
      const value = el.getAttribute(part.name);
      if (value === null) return false;
      if (part.value !== null && value !== part.value) return false;
    }
  }
  return true;
}

function matchesAny(el, groups) {
  for (let i = 0; i < groups.length; i++) {
    if (matchesGroup(el, groups[i])) return true;
  }
  return false;
}

function findAll(root, groups) {
  const found = [];
  const walk = function (node) {
    node.childNodes.forEach(function (child) {
      if (child.nodeType !== 1) return;
      if (matchesAny(child, groups)) found.push(child);
      walk(child);
    });
  };
  walk(root);
  return found;
}

// ----------------------------------------------------------------- parser

function parseOpenTag(src, start) {
  let i = start + 1;
  const nameMatch = /^[a-zA-Z][^\s/>]*/.exec(src.slice(i));
  if (!nameMatch) return null;
  const tag = nameMatch[0].toLowerCase();
  i += nameMatch[0].length;

  const attrs = Object.create(null);
  while (i < src.length) {
    while (i < src.length && /\s/.test(src[i])) i += 1;
    if (i >= src.length) break;
    if (src[i] === '>') { i += 1; break; }
    if (src[i] === '/' && src[i + 1] === '>') { i += 2; return { tag: tag, attrs: attrs, selfClosing: true, end: i }; }

    const attrMatch = /^[^\s=/>]+/.exec(src.slice(i));
    if (!attrMatch) { i += 1; continue; }
    const attrName = attrMatch[0].toLowerCase();
    i += attrMatch[0].length;
    while (i < src.length && /\s/.test(src[i])) i += 1;

    let value = '';
    if (src[i] === '=') {
      i += 1;
      while (i < src.length && /\s/.test(src[i])) i += 1;
      const quote = src[i];
      if (quote === '"' || quote === "'") {
        const close = src.indexOf(quote, i + 1);
        value = decodeEntities(close < 0 ? src.slice(i + 1) : src.slice(i + 1, close));
        i = close < 0 ? src.length : close + 1;
      } else {
        const bare = /^[^\s>]*/.exec(src.slice(i))[0];
        value = decodeEntities(bare);
        i += bare.length;
      }
    }
    attrs[attrName] = value;
  }
  return { tag: tag, attrs: attrs, selfClosing: false, end: i };
}

function parseFragment(html) {
  const src = String(html);
  const root = new MiniElement('div');
  const stack = [root];
  let i = 0;

  while (i < src.length) {
    const lt = src.indexOf('<', i);
    if (lt < 0) {
      pushText(stack[stack.length - 1], src.slice(i));
      break;
    }
    if (lt > i) pushText(stack[stack.length - 1], src.slice(i, lt));

    if (src.substr(lt, 4) === '<!--') {
      const end = src.indexOf('-->', lt);
      i = end < 0 ? src.length : end + 3;
      continue;
    }
    if (src.substr(lt, 2) === '</') {
      const end = src.indexOf('>', lt);
      const name = (end < 0 ? src.slice(lt + 2) : src.slice(lt + 2, end)).trim().toLowerCase();
      for (let s = stack.length - 1; s > 0; s -= 1) {
        if (stack[s].nodeName === name) { stack.length = s; break; }
      }
      i = end < 0 ? src.length : end + 1;
      continue;
    }

    const parsed = parseOpenTag(src, lt);
    if (!parsed) { pushText(stack[stack.length - 1], '<'); i = lt + 1; continue; }
    const el = new MiniElement(parsed.tag);
    Object.keys(parsed.attrs).forEach(function (key) { el.setAttribute(key, parsed.attrs[key]); });
    stack[stack.length - 1].appendChild(el);
    if (!parsed.selfClosing && !VOID_TAGS.has(parsed.tag)) stack.push(el);
    i = parsed.end;
  }

  return root.children;
}

function pushText(parent, text) {
  if (!text) return;
  parent.appendChild(createTextNode(decodeEntities(text)));
}

// ----------------------------------------------------------------- events

function makeEvent(type, options) {
  const opts = options || {};
  return {
    type: type,
    bubbles: !!opts.bubbles,
    cancelable: !!opts.cancelable,
    target: null,
    currentTarget: null,
    defaultPrevented: false,
    _stopped: false,
    key: opts.key,
    button: opts.button === undefined ? 0 : opts.button,
    altKey: !!opts.altKey,
    ctrlKey: !!opts.ctrlKey,
    metaKey: !!opts.metaKey,
    shiftKey: !!opts.shiftKey,
    preventDefault: function () { this.defaultPrevented = true; },
    stopPropagation: function () { this._stopped = true; },
    stopImmediatePropagation: function () { this._stopped = true; },
  };
}

function dispatchEvent(el, event) {
  if (!event.target) event.target = el;
  event.currentTarget = el;
  const handlers = (el._listeners[event.type] || []).slice();
  for (let i = 0; i < handlers.length; i++) {
    handlers[i].call(el, event);
  }
  if (event.bubbles && !event._stopped && el.parentNode && el.parentNode.nodeType === 1) {
    dispatchEvent(el.parentNode, event);
  }
  return !event.defaultPrevented;
}

// ----------------------------------------------------------------- document

const documentEl = new MiniElement('html');
const body = new MiniElement('body');
documentEl.appendChild(body);

const mainScroll = new MiniElement('div');
mainScroll.setAttribute('id', 'main-scroll');
const boardView = new MiniElement('section');
boardView.setAttribute('id', 'board-view');
const listView = new MiniElement('section');
listView.setAttribute('id', 'list-view');
mainScroll.appendChild(boardView);
mainScroll.appendChild(listView);

const modalRoot = new MiniElement('div');
modalRoot.setAttribute('id', 'modal-root');
const toastRoot = new MiniElement('div');
toastRoot.setAttribute('id', 'toast-root');

body.appendChild(mainScroll);
body.appendChild(modalRoot);
body.appendChild(toastRoot);

const idRegistry = new Map();
const documentListeners = Object.create(null);

function documentStub() {}

documentStub.readyState = 'loading';
documentStub.visibilityState = 'visible';
documentStub.activeElement = null;
documentStub.documentElement = documentEl;
documentStub.body = body;
documentStub.execCommand = function () { return true; };

documentStub.createElement = function (tag) { return new MiniElement(tag); };
documentStub.createTextNode = function (text) { return createTextNode(text); };

documentStub.getElementById = function (id) {
  if (idRegistry.has(id)) return idRegistry.get(id);
  const found = findAll(documentEl, [[{ kind: 'id', value: id }]])[0];
  if (found) { idRegistry.set(id, found); return found; }
  // The app renders into a handful of fixed containers; anything it asks for
  // that is not in the tree yet is an empty placeholder.
  const created = new MiniElement('div');
  created.setAttribute('id', id);
  body.appendChild(created);
  idRegistry.set(id, created);
  return created;
};

documentStub.querySelector = function (selector) {
  const groups = parseSelector(selector);
  const found = findAll(documentEl, groups)[0];
  if (found) return found;
  if (groups.length === 1 && groups[0].length === 1 && groups[0][0].kind === 'id') {
    return documentStub.getElementById(groups[0][0].value);
  }
  return null;
};

documentStub.querySelectorAll = function (selector) {
  return findAll(documentEl, parseSelector(selector));
};

documentStub.addEventListener = function (type, handler) {
  if (!documentListeners[type]) documentListeners[type] = [];
  documentListeners[type].push(handler);
};
documentStub.removeEventListener = function (type, handler) {
  const list = documentListeners[type];
  if (!list) return;
  const index = list.indexOf(handler);
  if (index >= 0) list.splice(index, 1);
};

// ------------------------------------------------------------- the network
//
// Every request is answered by this stub. The test never reaches a TaskView
// server: an absolute URL is a hard error, the relative URLs are answered
// from memory.

const requests = [];

// A bounded request budget. A broken code path can settle into an endless
// promise chain (e.g. re-fetching a task body forever); because that chain
// never yields to the event loop, a timer-based guard would never fire. The
// budget turns it into a plain failure instead of a hung test run.
const MAX_REQUESTS = 1000;

function stubFetch(url, config) {
  const target = String(url);
  if (requests.length >= MAX_REQUESTS) {
    throw new Error('runaway request loop: more than ' + MAX_REQUESTS + ' requests (last: ' + target + ')');
  }
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(target)) {
    throw new Error('the test must not reach the network: ' + target);
  }
  const method = (config && config.method) || 'GET';
  requests.push({
    url: target,
    method: method,
    body: config && config.body !== undefined ? config.body : null,
  });

  let payload = [];
  const single = /^\/api\/board\/tasks\/([^/?]+)$/.exec(target);
  if (method === 'POST' || method === 'PATCH') {
    const fields = config && config.body ? JSON.parse(config.body) : {};
    const id = method === 'PATCH' ? target.split('/').pop() : 'T-NEW';
    payload = { task: Object.assign({ id: id }, fields) };
  } else if (single) {
    // GET /api/board/tasks/{id}: the single-task endpoint always answers with
    // the full task, body fields included. This is what fills in a slim task.
    payload = makeTask(decodeURIComponent(single[1]), 'todo', 'medium');
    payload.file_path = 'src/server/routes/tasks.js';
    payload.line_start = 42;
    payload.line_end = 58;
    payload.code_snippet = 'const fetched = true;';
    payload.agent_notes = '从单条接口补全的备注。';
  } else if (target.indexOf('/api/board/projects') >= 0) {
    payload = [];
  } else if (target.indexOf('/api/board/stats') >= 0) {
    payload = { total: 0, backlog: 0, todo: 0, in_progress: 0, review: 0, done: 0, projects: 0 };
  } else {
    payload = [];
  }

  return Promise.resolve({
    ok: true,
    status: 200,
    text: function () { return Promise.resolve(JSON.stringify(payload)); },
  });
}

// ----------------------------------------------------------------- sandbox

const sandbox = {
  document: documentStub,
  console: console,
  localStorage: {
    getItem: function () { return null; },
    setItem: function () {},
    removeItem: function () {},
  },
  CSS: { escape: function (value) { return String(value); } },
  fetch: stubFetch,
  Event: function (type, options) { return makeEvent(type, options); },
  navigator: { clipboard: null },
  // The timers of the app are capped: the test must not wait for the toast
  // timers of the app to expire.
  setTimeout: function (fn, delay) { return setTimeout(fn, Math.min(Number(delay) || 0, 5)); },
  clearTimeout: clearTimeout,
  setInterval: function () { return 0; },
  clearInterval: clearInterval,
  requestAnimationFrame: function () { return 0; },
  addEventListener: function () {},
  removeEventListener: function () {},
  innerHeight: 900,
  innerWidth: 1280,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;

vm.createContext(sandbox);
vm.runInContext(patched, sandbox, { filename: 'index.html' });

const tv = sandbox.__taskview;
if (!tv) {
  console.error('FAIL: the app script did not expose the editor helpers');
  process.exit(1);
}

// ------------------------------------------------------------------ helpers

function makeTask(id, status, priority, title) {
  return {
    id: id,
    title: title || ('任务 ' + id),
    description: '',
    status: status,
    priority: priority,
    project_name: 'TaskView',
    project_path: 'C:/workspace/taskview',
    created_at: 1700000000000,
    updated_at: 1700000000000,
  };
}

function openEditor(task) {
  tv.openTaskModal(task || null);
  const layers = modalRoot.children;
  return layers[layers.length - 1];
}

function closeEditor() {
  tv.closeModal();
}

function statusSelectOf(layer) { return layer.querySelector('[data-field="status"]'); }
function prioritySelectOf(layer) { return layer.querySelector('[data-field="priority"]'); }

// initTselect moves the native select into a wrapper and puts the visible
// trigger next to it, so the trigger is a sibling of the select.
function triggerOf(select) {
  const wrapper = select.parentNode;
  if (!wrapper) return null;
  const kids = wrapper.children;
  for (let i = 0; i < kids.length; i++) {
    const kid = kids[i];
    if (kid === select) continue;
    if (kid.classList && kid.classList.contains('tselect-trigger')) return kid;
  }
  return null;
}

function openMenuOf(select) {
  const trigger = triggerOf(select);
  if (!trigger) return null;
  trigger.dispatchEvent(makeEvent('click', { bubbles: true }));
  const menus = body.children.filter(function (child) {
    return child.classList && child.classList.contains('tselect-menu');
  });
  return menus.length ? menus[menus.length - 1] : null;
}

// The visible trigger shows the label of the option that is selected.
function triggerLabel(select) {
  const trigger = triggerOf(select);
  if (!trigger) return null;
  const value = trigger.querySelector('.tselect-value');
  return value ? value.textContent.trim() : null;
}

// Pick an option the way a mouse does: open the listbox, click the entry.
function chooseWithMouse(select, value) {
  const menu = openMenuOf(select);
  if (!menu) return false;
  const index = select.options.map(function (option) { return option.value; }).indexOf(value);
  if (index < 0) return false;
  const items = menu.querySelectorAll('.tselect-opt');
  if (items.length !== select.options.length) return false;
  items[index].dispatchEvent(makeEvent('click', { bubbles: true }));
  return true;
}

// Pick an option the way a keyboard does: open the listbox, arrow down to
// the entry, confirm with Enter.
function chooseWithKeyboard(select, value) {
  const trigger = triggerOf(select);
  if (!trigger) return false;
  const index = select.options.map(function (option) { return option.value; }).indexOf(value);
  if (index < 0) return false;
  const steps = (index - select.selectedIndex + select.options.length) % select.options.length;
  trigger.dispatchEvent(makeEvent('keydown', { key: 'ArrowDown', bubbles: true }));
  for (let i = 0; i < steps; i++) {
    trigger.dispatchEvent(makeEvent('keydown', { key: 'ArrowDown', bubbles: true }));
  }
  trigger.dispatchEvent(makeEvent('keydown', { key: 'Enter', bubbles: true }));
  return true;
}

function setField(layer, field, value) {
  const el = layer.querySelector('[data-field="' + field + '"]');
  if (!el) return false;
  el.value = value;
  return true;
}

function submitEditor(layer) {
  const form = layer.querySelector('form');
  if (!form) return false;
  form.dispatchEvent(makeEvent('submit', { bubbles: true, cancelable: true }));
  return true;
}

function writes() {
  return requests.filter(function (request) {
    return request.method === 'POST' || request.method === 'PATCH';
  });
}

// flush drains the micro-task queue (and the app's own 0 ms timers) a few
// rounds deep. It is bounded on purpose: a broken async path must fail the
// test quickly instead of hanging, so the whole flush rejects after
// FLUSH_TIMEOUT_MS. The guard timer is deliberately NOT unref'd - an unref'd
// timer would let Node exit silently (status 0) when the async chain never
// settles, which is exactly the failure this guard has to make loud. On a
// passing run the timer is cleared as soon as the chain resolves.
const FLUSH_TIMEOUT_MS = 2000;

function flush(rounds) {
  let promise = Promise.resolve();
  const count = rounds || 4;
  for (let i = 0; i < count; i++) {
    promise = promise.then(function () {
      return new Promise(function (resolve) { setTimeout(resolve, 0); });
    });
  }

  let timer = null;
  const guard = new Promise(function (_, reject) {
    timer = setTimeout(function () {
      reject(new Error('flush() did not settle within ' + FLUSH_TIMEOUT_MS + ' ms - an async path is stuck'));
    }, FLUSH_TIMEOUT_MS);
  });

  return Promise.race([promise, guard]).then(function (value) {
    clearTimeout(timer);
    return value;
  }, function (err) {
    clearTimeout(timer);
    throw err;
  });
}

// ------------------------------------------------------- the code context
//
// The file path and the two line numbers share one row of the editor:
//
//   <div class="task-code-fields" data-code-fields>
//     <div class="min-w-0">… <input id="f-file" data-field="file_path" /></div>
//     <div class="min-w-0">… <input id="f-line-start" data-field="line_start" type="number" min="0" /></div>
//     <div class="min-w-0">… <input id="f-line-end" data-field="line_end" type="number" min="0" /></div>
//   </div>
//
// The path comes first, the two numbers follow. The grid hands the free space
// to the path and keeps the numbers narrow, so a long path never pushes the
// line numbers out of the row.

function codeRowOf(layer) {
  return layer.querySelector('[data-code-fields]');
}

function fieldElOf(layer, field) {
  return layer.querySelector('[data-field="' + field + '"]');
}

// The shape of the row, without the values: the same row has to be rendered
// for a new task and for an edit.
function codeRowSignature(row) {
  if (!row) return null;
  const cells = row.children.map(function (cell) {
    const input = cell.querySelector('input');
    if (!input) return 'empty';
    return [
      cell.className.trim(),
      input.getAttribute('data-field'),
      input.id,
      input.getAttribute('type') || 'text',
      input.getAttribute('min') || '',
    ].join(',');
  });
  return row.className.trim() + '|' + cells.join('|');
}

// The three cells of the code row, in the order the editor shows them.
const CODE_FIELDS = [
  { field: 'file_path', id: 'f-file', label: '文件路径', type: null, min: null },
  { field: 'line_start', id: 'f-line-start', label: '起始行号', type: 'number', min: '0' },
  { field: 'line_end', id: 'f-line-end', label: '结束行号', type: 'number', min: '0' },
];

function assertCodeRow(layer, what) {
  const row = codeRowOf(layer);
  check(!!row, 'the ' + what + ' editor has no row for the code context');
  if (!row) return null;

  check(row.classList.contains('task-code-fields'),
    'the code row of the ' + what + ' editor is not a .task-code-fields grid');

  const cells = row.children;
  check(cells.length === 3,
    'the code row of the ' + what + ' editor holds ' + cells.length + ' cells instead of three');
  if (cells.length !== 3) return row;

  CODE_FIELDS.forEach(function (spec, index) {
    const cell = cells[index];
    check(cell.classList.contains('min-w-0'),
      'the ' + spec.field + ' cell of the ' + what + ' editor is not min-w-0');

    const input = cell.querySelector('[data-field="' + spec.field + '"]');
    check(!!input, 'the ' + spec.field + ' cell of the ' + what + ' editor holds no input');
    if (!input) return;

    check(input.getAttribute('data-field') === spec.field,
      'the ' + spec.field + ' input of the ' + what + ' editor has the data-field ' + input.getAttribute('data-field'));
    check(input.id === spec.id,
      'the ' + spec.field + ' input of the ' + what + ' editor is #' + input.id + ' instead of #' + spec.id);
    check(input.getAttribute('type') === spec.type,
      'the ' + spec.field + ' input of the ' + what + ' editor is of the type ' + input.getAttribute('type') +
      ' instead of ' + spec.type);
    check(input.getAttribute('min') === spec.min,
      'the ' + spec.field + ' input of the ' + what + ' editor has the min ' + input.getAttribute('min') +
      ' instead of ' + spec.min);

    const label = cell.querySelector('label[for="' + spec.id + '"]');
    check(!!label, 'the ' + spec.field + ' cell of the ' + what + ' editor has no label for #' + spec.id);
    if (label) {
      check(label.getAttribute('for') === spec.id,
        'the label of the ' + spec.field + ' field of the ' + what + ' editor points at #' + label.getAttribute('for'));
      check(label.textContent.trim() === spec.label,
        'the label of the ' + spec.field + ' field of the ' + what + ' editor is "' + label.textContent.trim() +
        '" instead of ' + spec.label);
    }
  });

  // The path comes first, the two numbers follow.
  const inputs = row.querySelectorAll('input');
  check(inputs.length === 3,
    'the code row of the ' + what + ' editor holds ' + inputs.length + ' inputs instead of three');
  CODE_FIELDS.forEach(function (spec, index) {
    const input = inputs[index];
    if (!input) return;
    check(input.getAttribute('data-field') === spec.field,
      'the input number ' + (index + 1) + ' of the code row of the ' + what + ' editor is the ' +
      input.getAttribute('data-field') + ' field instead of the ' + spec.field + ' field');
  });

  return row;
}

// The stylesheet carries the layout of the row: a grid with a shrinkable
// path column, two narrow number columns and fields that may shrink.
function assertCodeRowCss() {
  const styleMatch = html.match(/<style[^>]*>([\s\S]*?)<\/style>/);
  const style = styleMatch ? styleMatch[1] : '';
  check(!!style, 'the page has no inline stylesheet');

  check(/\.task-code-fields\s*\{[^}]*display:\s*grid/.test(style),
    'the .task-code-fields rule is not a grid');
  check(/\.task-code-fields\s*\{[^}]*grid-template-columns:[^;}]*minmax\(\s*0\s*,\s*1fr\s*\)/.test(style),
    'the .task-code-fields rule does not give the file path a shrinkable column');
  check(/\.task-code-fields\s*\{[^}]*grid-template-columns:[^;}]*repeat\(\s*2\s*,/.test(style),
    'the .task-code-fields rule does not hold the two line number columns');
  check(/\.task-code-fields\s+\.field\s*\{[^}]*min-width:\s*0/.test(style),
    'the .task-code-fields .field rule does not let the fields shrink');
  check(/\.task-code-fields\s+input\[type="number"\]/.test(style),
    'the .task-code-fields rule does not tighten the padding of the number inputs');
}

// A task that carries a code context.
function makeCodeTask(id) {
  const task = makeTask(id, 'todo', 'medium');
  task.file_path = 'src/server/routes/tasks.js';
  task.line_start = 42;
  task.line_end = 58;
  return task;
}

// The error of the line range: the editor shows it under the row.
function linesErrorOf(layer) {
  return layer.querySelector('[data-error-for="lines"]');
}

// The editor has to complain about the line range and name the field that is
// wrong, so the user knows which of the two numbers to fix.
function checkLinesError(layer, what, about) {
  const error = linesErrorOf(layer);
  check(!!error, 'the ' + what + ' showed no error for the line range');
  if (!error) return;
  check(!error.classList.contains('hidden'), 'the ' + what + ' hid the error of the line range');
  check(error.textContent.trim().indexOf('行号') >= 0,
    'the ' + what + ' showed the error "' + error.textContent.trim() + '" instead of one about the line numbers');
  if (about) {
    check(error.textContent.trim().indexOf(about) >= 0,
      'the ' + what + ' showed the error "' + error.textContent.trim() + '" instead of one about the ' + about);
  }
}

// ------------------------------------------------------------------ tests

async function run() {
  const STATUS_ORDER = tv.STATUS_ORDER;
  const PRIORITY_ORDER = tv.PRIORITY_ORDER;

  // ------------------------------------------------- the source has no segment

  check(source.indexOf('statusSegment') < 0, 'the source still defines statusSegment');
  check(source.indexOf('prioritySegment') < 0, 'the source still defines prioritySegment');
  check(source.indexOf('SEGMENT_BTN') < 0, 'the source still carries the segment button styles');
  check(source.indexOf('data-segment') < 0, 'the source still renders a segmented control');
  check(source.indexOf('statusSelect') >= 0, 'the source has no statusSelect helper');
  check(source.indexOf('prioritySelect') >= 0, 'the source has no prioritySelect helper');

  // ----------------------------------------------------------- create defaults

  let layer = openEditor(null);
  let statusEl = statusSelectOf(layer);
  let priorityEl = prioritySelectOf(layer);

  check(!!statusEl, 'the editor has no status select');
  check(!!priorityEl, 'the editor has no priority select');
  if (!statusEl || !priorityEl) return;

  check(statusEl.value === 'todo', 'a new task does not default to todo but to ' + statusEl.value);
  check(priorityEl.value === 'medium', 'a new task does not default to medium but to ' + priorityEl.value);
  check(statusEl.id === 'f-status', 'the status select is not #f-status but ' + statusEl.id);
  check(priorityEl.id === 'f-priority', 'the priority select is not #f-priority but ' + priorityEl.id);
  check(statusEl.getAttribute('data-field') === 'status', 'the status select has no data-field="status"');
  check(priorityEl.getAttribute('data-field') === 'priority', 'the priority select has no data-field="priority"');
  check(statusEl.getAttribute('aria-label') === '状态', 'the status select has no aria-label="状态"');
  check(priorityEl.getAttribute('aria-label') === '优先级', 'the priority select has no aria-label="优先级"');
  check(statusEl.classList.contains('hidden'), 'the status select is not hidden');
  check(priorityEl.classList.contains('hidden'), 'the priority select is not hidden');

  // ------------------------------------------------------------ one single row

  const row = layer.querySelector('[data-task-selects]');
  check(!!row, 'the editor has no row for the two selects');
  if (row) {
    check(row.classList.contains('grid'), 'the row of the two selects is not a grid');
    check(row.classList.contains('grid-cols-2'), 'the row of the two selects is not a two column grid');
    check(row.classList.contains('gap-4'), 'the row of the two selects has no gap-4');
    const cells = row.children;
    check(cells.length === 2, 'the row of the two selects holds ' + cells.length + ' cells instead of two');
    if (cells.length === 2) {
      check(cells[0].classList.contains('min-w-0'), 'the status cell is not min-w-0');
      check(cells[1].classList.contains('min-w-0'), 'the priority cell is not min-w-0');
      check(cells[0].querySelector('[data-field="status"]') === statusEl,
        'the status select does not sit in the first cell of the row');
      check(cells[1].querySelector('[data-field="priority"]') === priorityEl,
        'the priority select does not sit in the second cell of the row');
    }
  }

  // The labels point at the trigger the custom select creates.
  const statusLabel = layer.querySelector('label[for="f-status-trigger"]');
  const priorityLabel = layer.querySelector('label[for="f-priority-trigger"]');
  check(!!statusLabel, 'the editor has no label for the status trigger');
  check(!!priorityLabel, 'the editor has no label for the priority trigger');
  if (statusLabel) check(statusLabel.textContent.trim() === '状态', 'the status label is not 状态');
  if (priorityLabel) check(priorityLabel.textContent.trim() === '优先级', 'the priority label is not 优先级');

  // The segmented control is gone from the markup.
  check(layer.innerHTML.indexOf('data-segment') < 0, 'the editor still renders a segmented control');
  check(layer.innerHTML.indexOf('aria-pressed') < 0, 'the editor still renders aria-pressed buttons');

  // ------------------------------------------------------------- every option

  const statusOptions = statusEl.options;
  check(statusOptions.length === STATUS_ORDER.length,
    'the status select offers ' + statusOptions.length + ' options instead of ' + STATUS_ORDER.length);
  STATUS_ORDER.forEach(function (status, index) {
    const option = statusOptions[index];
    check(!!option, 'the status select has no option for ' + status);
    if (!option) return;
    check(option.value === status, 'the status option ' + index + ' is ' + option.value + ' instead of ' + status);
    check(option.textContent.trim() === tv.STATUS_META[status].label,
      'the status option ' + status + ' shows "' + option.textContent.trim() + '" instead of ' + tv.STATUS_META[status].label);
  });
  // The long review label is offered in full.
  const reviewOption = statusOptions.filter(function (option) { return option.value === 'review'; })[0];
  check(!!reviewOption, 'the status select has no review option');
  if (reviewOption) {
    check(reviewOption.textContent.trim() === '待审查验证',
      'the review option shows "' + reviewOption.textContent.trim() + '" instead of 待审查验证');
  }

  const priorityOptions = priorityEl.options;
  check(priorityOptions.length === PRIORITY_ORDER.length,
    'the priority select offers ' + priorityOptions.length + ' options instead of ' + PRIORITY_ORDER.length);
  PRIORITY_ORDER.forEach(function (priority, index) {
    const option = priorityOptions[index];
    check(!!option, 'the priority select has no option for ' + priority);
    if (!option) return;
    check(option.value === priority, 'the priority option ' + index + ' is ' + option.value + ' instead of ' + priority);
    check(option.textContent.trim() === tv.PRIORITY_META[priority].label,
      'the priority option ' + priority + ' shows "' + option.textContent.trim() + '" instead of ' + tv.PRIORITY_META[priority].label);
  });

  // ----------------------------------------------------- the selects are wired

  check(statusEl.dataset.tselectReady === '1', 'the status select was not turned into a custom select');
  check(priorityEl.dataset.tselectReady === '1', 'the priority select was not turned into a custom select');
  check(!!triggerOf(statusEl), 'the status select has no trigger button');
  check(!!triggerOf(priorityEl), 'the priority select has no trigger button');
  if (triggerOf(statusEl)) check(triggerOf(statusEl).id === 'f-status-trigger', 'the status trigger is not #f-status-trigger');
  if (triggerOf(priorityEl)) check(triggerOf(priorityEl).id === 'f-priority-trigger', 'the priority trigger is not #f-priority-trigger');
  check(statusEl.getAttribute('aria-hidden') === 'true', 'the status select is not hidden from the screen readers');
  check(priorityEl.getAttribute('aria-hidden') === 'true', 'the priority select is not hidden from the screen readers');

  // The visible trigger carries the label of the selected option.
  check(triggerLabel(statusEl) === '待办',
    'the status trigger shows "' + triggerLabel(statusEl) + '" instead of 待办');
  check(triggerLabel(priorityEl) === '中',
    'the priority trigger shows "' + triggerLabel(priorityEl) + '" instead of 中');

  closeEditor();

  // ------------------------------------------------- the edit preselection

  STATUS_ORDER.forEach(function (status) {
    const editor = openEditor(makeTask('T-' + status, status, 'medium'));
    const select = statusSelectOf(editor);
    check(!!select, 'the editor of a ' + status + ' task has no status select');
    if (select) {
      check(select.value === status, 'the editor of a ' + status + ' task preselects ' + select.value);
      const selected = select.options.filter(function (option) { return option.hasAttribute('selected'); });
      check(selected.length === 1,
        'the editor of a ' + status + ' task preselects ' + selected.length + ' statuses instead of one');
      if (selected.length === 1) {
        check(selected[0].value === status, 'the editor of a ' + status + ' task marks ' + selected[0].value + ' as selected');
      }
      check(triggerLabel(select) === tv.STATUS_META[status].label,
        'the status trigger of a ' + status + ' task shows "' + triggerLabel(select) + '" instead of ' + tv.STATUS_META[status].label);
    }
    closeEditor();
  });

  PRIORITY_ORDER.forEach(function (priority) {
    const editor = openEditor(makeTask('T-' + priority, 'todo', priority));
    const select = prioritySelectOf(editor);
    check(!!select, 'the editor of a ' + priority + ' task has no priority select');
    if (select) {
      check(select.value === priority, 'the editor of a ' + priority + ' task preselects ' + select.value);
      const selected = select.options.filter(function (option) { return option.hasAttribute('selected'); });
      check(selected.length === 1,
        'the editor of a ' + priority + ' task preselects ' + selected.length + ' priorities instead of one');
      if (selected.length === 1) {
        check(selected[0].value === priority, 'the editor of a ' + priority + ' task marks ' + selected[0].value + ' as selected');
      }
      check(triggerLabel(select) === tv.PRIORITY_META[priority].label,
        'the priority trigger of a ' + priority + ' task shows "' + triggerLabel(select) + '" instead of ' + tv.PRIORITY_META[priority].label);
    }
    closeEditor();
  });

  // A task that is in review keeps both of its values.
  const reviewEditor = openEditor(makeTask('T-REVIEW', 'review', 'urgent'));
  check(statusSelectOf(reviewEditor).value === 'review', 'the editor of a review task preselects another status');
  check(prioritySelectOf(reviewEditor).value === 'urgent', 'the editor of a review task preselects another priority');
  closeEditor();

  // ------------------------------------------- a new task reaches the server

  layer = openEditor(null);
  statusEl = statusSelectOf(layer);
  priorityEl = prioritySelectOf(layer);

  check(chooseWithMouse(statusEl, 'in_progress'), 'the status option in_progress could not be picked');
  check(statusEl.value === 'in_progress', 'the status select did not take the picked in_progress');
  check(triggerLabel(statusEl) === '进行中',
    'the status trigger shows "' + triggerLabel(statusEl) + '" instead of 进行中');
  check(chooseWithMouse(priorityEl, 'urgent'), 'the priority option urgent could not be picked');
  check(priorityEl.value === 'urgent', 'the priority select did not take the picked urgent');
  check(triggerLabel(priorityEl) === '紧急',
    'the priority trigger shows "' + triggerLabel(priorityEl) + '" instead of 紧急');

  setField(layer, 'title', '新建的任务');
  setField(layer, 'project_path', 'C:\\dev\\my-app');

  requests.length = 0;
  check(submitEditor(layer), 'the editor has no form to submit');
  await flush();

  let posts = writes();
  check(posts.length === 1, 'saving a new task sent ' + posts.length + ' write requests instead of one');
  if (posts.length === 1) {
    check(posts[0].method === 'POST', 'a new task was not sent with POST but with ' + posts[0].method);
    check(posts[0].url === '/api/board/tasks', 'a new task was sent to ' + posts[0].url);
    const payload = JSON.parse(posts[0].body);
    check(payload.status === 'in_progress', 'the new task was posted with the status ' + payload.status);
    check(payload.priority === 'urgent', 'the new task was posted with the priority ' + payload.priority);
    check(payload.title === '新建的任务', 'the new task was posted with the title ' + payload.title);
    check(payload.project_path === 'C:\\dev\\my-app', 'the new task was posted with the project path ' + payload.project_path);
  }

  // ------------------------------------------- an edit reaches the server

  layer = openEditor(makeTask('T-EDIT', 'todo', 'medium'));
  statusEl = statusSelectOf(layer);
  priorityEl = prioritySelectOf(layer);

  check(chooseWithMouse(statusEl, 'done'), 'the status option done could not be picked');
  check(chooseWithMouse(priorityEl, 'low'), 'the priority option low could not be picked');

  requests.length = 0;
  check(submitEditor(layer), 'the editor has no form to submit');
  await flush();

  let patches = writes();
  check(patches.length === 1, 'saving an edit sent ' + patches.length + ' write requests instead of one');
  if (patches.length === 1) {
    check(patches[0].method === 'PATCH', 'an edit was not sent with PATCH but with ' + patches[0].method);
    check(patches[0].url === '/api/board/tasks/T-EDIT', 'an edit was sent to ' + patches[0].url);
    const payload = JSON.parse(patches[0].body);
    check(payload.status === 'done', 'the edit was sent with the status ' + payload.status);
    check(payload.priority === 'low', 'the edit was sent with the priority ' + payload.priority);
  }

  // ------------------------------- the keyboard picks a value as well

  layer = openEditor(null);
  statusEl = statusSelectOf(layer);
  priorityEl = prioritySelectOf(layer);

  check(chooseWithKeyboard(statusEl, 'backlog'), 'the status option backlog could not be picked with the keyboard');
  check(statusEl.value === 'backlog', 'the status select did not take the picked backlog');
  check(triggerLabel(statusEl) === '待整理',
    'the status trigger shows "' + triggerLabel(statusEl) + '" instead of 待整理');
  check(chooseWithKeyboard(priorityEl, 'high'), 'the priority option high could not be picked with the keyboard');
  check(priorityEl.value === 'high', 'the priority select did not take the picked high');
  check(triggerLabel(priorityEl) === '高',
    'the priority trigger shows "' + triggerLabel(priorityEl) + '" instead of 高');

  setField(layer, 'title', '键盘的任务');
  setField(layer, 'project_path', '/home/me/my-app');

  requests.length = 0;
  check(submitEditor(layer), 'the editor has no form to submit');
  await flush();

  posts = writes();
  check(posts.length === 1, 'saving with the keyboard sent ' + posts.length + ' write requests instead of one');
  if (posts.length === 1) {
    const payload = JSON.parse(posts[0].body);
    check(payload.status === 'backlog', 'the task was posted with the status ' + payload.status);
    check(payload.priority === 'high', 'the task was posted with the priority ' + payload.priority);
  }

  // ----------------------- an edit without a change keeps the old values

  layer = openEditor(makeTask('T-KEEP', 'review', 'urgent'));
  setField(layer, 'title', '不变的任务');
  setField(layer, 'project_path', 'C:\\dev\\my-app');

  requests.length = 0;
  check(submitEditor(layer), 'the editor has no form to submit');
  await flush();

  patches = writes();
  check(patches.length === 1, 'saving an unchanged edit sent ' + patches.length + ' write requests instead of one');
  if (patches.length === 1) {
    const payload = JSON.parse(patches[0].body);
    check(payload.status === 'review', 'the unchanged edit was sent with the status ' + payload.status);
    check(payload.priority === 'urgent', 'the unchanged edit was sent with the priority ' + payload.priority);
  }

  // --------------------------------------------------- the network hygiene

  check(requests.every(function (request) { return request.url.charAt(0) === '/'; }),
    'the test issued a request outside of the local API');
  check(requests.every(function (request) {
    return request.method === 'GET' || request.method === 'POST' || request.method === 'PATCH';
  }), 'the test issued a request with an unexpected method');

  tv.closeAllModals();

  /* ==================================================================== */
  /* the code context: the path and the two line numbers share one row    */
  /* ==================================================================== */

  // The stylesheet has to carry the layout, not only the marker attribute.
  assertCodeRowCss();

  // ------------------------------------------------ the row of a new task

  let codeLayer = openEditor(null);
  assertCodeRow(codeLayer, 'new task');

  // A new task starts with an empty code context.
  const newFile = fieldElOf(codeLayer, 'file_path');
  const newLineStart = fieldElOf(codeLayer, 'line_start');
  const newLineEnd = fieldElOf(codeLayer, 'line_end');
  check(!!newFile, 'the new task editor has no file path field');
  check(!!newLineStart, 'the new task editor has no start line field');
  check(!!newLineEnd, 'the new task editor has no end line field');
  if (newFile && newLineStart && newLineEnd) {
    check(newFile.value === '', 'a new task starts with the file path "' + newFile.value + '" instead of an empty one');
    check(newLineStart.value === '', 'a new task starts with the start line "' + newLineStart.value + '" instead of an empty one');
    check(newLineEnd.value === '', 'a new task starts with the end line "' + newLineEnd.value + '" instead of an empty one');
  }

  const createSignature = codeRowSignature(codeRowOf(codeLayer));

  // ------------------------------------------------- the row of an edit

  codeLayer = openEditor(makeCodeTask('T-CODE'));
  assertCodeRow(codeLayer, 'edited task');

  // The very same row is used for the new task and for the edit.
  check(codeRowSignature(codeRowOf(codeLayer)) === createSignature,
    'the code row of the new task and the code row of the edit are not the same');

  // An edit shows the code context of the task.
  const editFile = fieldElOf(codeLayer, 'file_path');
  const editLineStart = fieldElOf(codeLayer, 'line_start');
  const editLineEnd = fieldElOf(codeLayer, 'line_end');
  check(!!editFile, 'the edited task editor has no file path field');
  check(!!editLineStart, 'the edited task editor has no start line field');
  check(!!editLineEnd, 'the edited task editor has no end line field');
  if (editFile) {
    check(editFile.value === 'src/server/routes/tasks.js',
      'the edit shows the file path "' + editFile.value + '" instead of the one of the task');
  }
  if (editLineStart) {
    check(String(editLineStart.value) === '42',
      'the edit shows the start line "' + editLineStart.value + '" instead of 42');
  }
  if (editLineEnd) {
    check(String(editLineEnd.value) === '58',
      'the edit shows the end line "' + editLineEnd.value + '" instead of 58');
  }

  // The code context of an edit reaches the server.
  requests.length = 0;
  check(submitEditor(codeLayer), 'the editor has no form to submit');
  await flush();

  let codeWrites = writes();
  check(codeWrites.length === 1,
    'saving an edit with a code context sent ' + codeWrites.length + ' write requests instead of one');
  if (codeWrites.length === 1) {
    check(codeWrites[0].method === 'PATCH', 'the edit with a code context was not sent with PATCH');
    check(codeWrites[0].url === '/api/board/tasks/T-CODE',
      'the edit with a code context was sent to ' + codeWrites[0].url);
    const payload = JSON.parse(codeWrites[0].body);
    check(payload.file_path === 'src/server/routes/tasks.js',
      'the edit was sent with the file path ' + payload.file_path);
    check(payload.line_start === 42, 'the edit was sent with the start line ' + payload.line_start);
    check(payload.line_end === 58, 'the edit was sent with the end line ' + payload.line_end);
  }

  // ------------------------------------- a new task carries a code context

  codeLayer = openEditor(null);
  setField(codeLayer, 'title', '带代码上下文的任务');
  setField(codeLayer, 'project_path', 'C:\\dev\\my-app');
  setField(codeLayer, 'file_path', 'src/app.js');
  setField(codeLayer, 'line_start', '10');
  setField(codeLayer, 'line_end', '20');

  requests.length = 0;
  check(submitEditor(codeLayer), 'the editor has no form to submit');
  await flush();

  codeWrites = writes();
  check(codeWrites.length === 1,
    'saving a new task with a code context sent ' + codeWrites.length + ' write requests instead of one');
  if (codeWrites.length === 1) {
    check(codeWrites[0].method === 'POST', 'the new task with a code context was not sent with POST');
    check(codeWrites[0].url === '/api/board/tasks', 'the new task with a code context was sent to ' + codeWrites[0].url);
    const payload = JSON.parse(codeWrites[0].body);
    check(payload.file_path === 'src/app.js', 'the new task was sent with the file path ' + payload.file_path);
    check(payload.line_start === 10, 'the new task was sent with the start line ' + payload.line_start);
    check(payload.line_end === 20, 'the new task was sent with the end line ' + payload.line_end);
  }

  // ------------------------------------- an empty code context is cleared

  codeLayer = openEditor(makeCodeTask('T-CLEAR'));
  setField(codeLayer, 'file_path', '');
  setField(codeLayer, 'line_start', '');
  setField(codeLayer, 'line_end', '');

  requests.length = 0;
  check(submitEditor(codeLayer), 'the editor has no form to submit');
  await flush();

  codeWrites = writes();
  check(codeWrites.length === 1,
    'clearing the code context sent ' + codeWrites.length + ' write requests instead of one');
  if (codeWrites.length === 1) {
    const payload = JSON.parse(codeWrites[0].body);
    check(payload.file_path === null, 'the cleared file path was sent as ' + JSON.stringify(payload.file_path));
    check(payload.line_start === null, 'the cleared start line was sent as ' + JSON.stringify(payload.line_start));
    check(payload.line_end === null, 'the cleared end line was sent as ' + JSON.stringify(payload.line_end));
  }

  // ------------------------------------------------ the line range is sane

  // A negative end line is refused. The start line stays empty, so the range
  // cannot mask the negative number: only the sign can stop it.
  codeLayer = openEditor(makeCodeTask('T-NEGATIVE-END'));
  setField(codeLayer, 'line_start', '');
  setField(codeLayer, 'line_end', '-3');

  requests.length = 0;
  check(submitEditor(codeLayer), 'the editor has no form to submit');
  await flush();

  codeWrites = writes();
  check(codeWrites.length === 0,
    'a negative end line was written anyway (' + codeWrites.length + ' write requests)');
  checkLinesError(codeLayer, 'a negative end line', '结束行号');

  // A negative start line is refused as well. The end line stays empty, so
  // the range cannot mask the negative number either.
  codeLayer = openEditor(makeCodeTask('T-NEGATIVE'));
  setField(codeLayer, 'line_start', '-5');
  setField(codeLayer, 'line_end', '');

  requests.length = 0;
  check(submitEditor(codeLayer), 'the editor has no form to submit');
  await flush();

  codeWrites = writes();
  check(codeWrites.length === 0,
    'a negative start line was written anyway (' + codeWrites.length + ' write requests)');
  checkLinesError(codeLayer, 'a negative start line', '起始行号');

  // An end line before the start line is refused too.
  codeLayer = openEditor(makeCodeTask('T-REVERSED'));
  setField(codeLayer, 'line_start', '80');
  setField(codeLayer, 'line_end', '20');

  requests.length = 0;
  check(submitEditor(codeLayer), 'the editor has no form to submit');
  await flush();

  codeWrites = writes();
  check(codeWrites.length === 0,
    'a reversed line range was written anyway (' + codeWrites.length + ' write requests)');
  checkLinesError(codeLayer, 'a reversed line range');

  // A range that is the other way round is written.
  codeLayer = openEditor(makeCodeTask('T-ORDERED'));
  setField(codeLayer, 'line_start', '20');
  setField(codeLayer, 'line_end', '80');

  requests.length = 0;
  check(submitEditor(codeLayer), 'the editor has no form to submit');
  await flush();

  codeWrites = writes();
  check(codeWrites.length === 1,
    'a valid line range sent ' + codeWrites.length + ' write requests instead of one');
  if (codeWrites.length === 1) {
    const payload = JSON.parse(codeWrites[0].body);
    check(payload.line_start === 20, 'the valid range was sent with the start line ' + payload.line_start);
    check(payload.line_end === 80, 'the valid range was sent with the end line ' + payload.line_end);
  }

  // ---------------------------------------------- the network hygiene again

  check(requests.every(function (request) { return request.url.charAt(0) === '/'; }),
    'the code context test issued a request outside of the local API');
  check(requests.every(function (request) {
    return request.method === 'GET' || request.method === 'POST' || request.method === 'PATCH';
  }), 'the code context test issued a request with an unexpected method');

  tv.closeAllModals();

  /* ==================================================================== */
  /* the board listing omits the body: the editor fills it in first        */
  /* ==================================================================== */

  // A task as the (trimmed) board listing hands it over: no code_snippet and
  // no agent_notes, flagged so the app knows it has to fetch them.
  const slim = makeTask('T-SLIM', 'todo', 'medium');
  slim.bodyPending = true;

  requests.length = 0;
  await tv.openTaskModal(slim);

  const slimFetches = requests.filter(function (request) {
    return request.method === 'GET' && request.url === '/api/board/tasks/T-SLIM';
  });
  check(slimFetches.length === 1,
    'opening a slim task made ' + slimFetches.length + ' single-task requests instead of one');
  check(requests.every(function (request) { return request.url.charAt(0) === '/'; }),
    'the slim task fetch left the local API');

  const slimLayer = modalRoot.children[modalRoot.children.length - 1];
  check(!!slimLayer, 'the editor of a slim task did not open');
  if (slimLayer) {
    const snippetField = fieldElOf(slimLayer, 'code_snippet');
    const notesField = fieldElOf(slimLayer, 'agent_notes');
    check(!!snippetField, 'the editor of a slim task has no code snippet field');
    check(!!notesField, 'the editor of a slim task has no agent notes field');
    if (snippetField) {
      check(snippetField.value === 'const fetched = true;',
        'the editor of a slim task shows the snippet "' + snippetField.value + '" instead of the fetched one');
    }
    if (notesField) {
      check(notesField.value === '从单条接口补全的备注。',
        'the editor of a slim task shows the notes "' + notesField.value + '" instead of the fetched ones');
    }
  }

  // ensureTaskBody replaces the row in memory with the completed task, so the
  // body is fetched once and stays cached.
  const cached = tv.state.tasks.filter(function (task) { return task.id === 'T-SLIM'; })[0];
  check(!!cached, 'the completed slim task is missing from the in-memory list');
  if (cached) {
    check(cached.bodyPending === false,
      'the fetched body was not cached on the slim task (bodyPending=' + cached.bodyPending + ')');
    check(cached.code_snippet === 'const fetched = true;',
      'the cached task lost the fetched code snippet');
  }

  // Saving it again keeps the fetched body: the PATCH carries both fields.
  requests.length = 0;
  if (slimLayer) {
    check(submitEditor(slimLayer), 'the editor of a slim task has no form to submit');
    await flush();
    const slimWrites = writes();
    check(slimWrites.length === 1,
      'saving the slim task sent ' + slimWrites.length + ' write requests instead of one');
    if (slimWrites.length === 1) {
      const payload = JSON.parse(slimWrites[0].body);
      check(payload.code_snippet === 'const fetched = true;',
        'saving the slim task dropped the fetched code snippet (' + JSON.stringify(payload.code_snippet) + ')');
      check(payload.agent_notes === '从单条接口补全的备注。',
        'saving the slim task dropped the fetched agent notes (' + JSON.stringify(payload.agent_notes) + ')');
    }
  }
  tv.closeAllModals();

  // A task that already carries its body opens without any extra request.
  requests.length = 0;
  tv.openTaskModal(makeCodeTask('T-FULL'));
  check(requests.length === 0,
    'opening a full task made ' + requests.length + ' unnecessary request(s)');
  tv.closeAllModals();

  // Re-opening the cached row does not fetch the body a second time.
  tv.state.tasks = [cached];
  requests.length = 0;
  await tv.openTaskModal(cached);
  const refetches = requests.filter(function (request) {
    return request.method === 'GET' && request.url === '/api/board/tasks/T-SLIM';
  });
  check(refetches.length === 0,
    're-opening a cached slim task fetched it ' + refetches.length + ' more time(s)');
  tv.closeAllModals();

  /* ==================================================================== */
  /* delete + undo keeps the body the trimmed listing does not carry       */
  /* ==================================================================== */

  // The task as the board holds it: no body, flagged for completion.
  const toDelete = makeTask('T-DELETE', 'todo', 'medium');
  toDelete.bodyPending = true;
  tv.state.tasks = [toDelete];
  // Start from an empty toast root so only the delete toast can be found.
  while (toastRoot.firstChild) toastRoot.removeChild(toastRoot.firstChild);

  requests.length = 0;
  await tv.performDelete(toDelete);

  // The delete had to fetch the full task first, then send the DELETE.
  const readBeforeDelete = requests.filter(function (request) {
    return request.method === 'GET' && request.url === '/api/board/tasks/T-DELETE';
  });
  const deletes = requests.filter(function (request) { return request.method === 'DELETE'; });
  check(readBeforeDelete.length === 1,
    'deleting a slim task made ' + readBeforeDelete.length + ' single-task reads instead of one');
  check(deletes.length === 1,
    'deleting a slim task sent ' + deletes.length + ' DELETE requests instead of one');

  // The undo toast carries the full body the listing did not ship.
  const undoButtons = toastRoot.querySelectorAll('[data-toast-action]');
  check(undoButtons.length === 1,
    'the delete toast has ' + undoButtons.length + ' undo buttons instead of one');
  if (undoButtons.length === 1) {
    requests.length = 0;
    undoButtons[0].dispatchEvent(makeEvent('click', { bubbles: true }));
    await flush(8);

    const restores = requests.filter(function (request) { return request.method === 'POST'; });
    check(restores.length === 1,
      'the undo sent ' + restores.length + ' restore requests instead of one');
    if (restores.length === 1) {
      const payload = JSON.parse(restores[0].body);
      check(payload.code_snippet === 'const fetched = true;',
        'the undo lost the code snippet (' + JSON.stringify(payload.code_snippet) + ')');
      check(payload.agent_notes === '从单条接口补全的备注。',
        'the undo lost the agent notes (' + JSON.stringify(payload.agent_notes) + ')');
      check(payload.title === toDelete.title,
        'the undo lost the title (' + JSON.stringify(payload.title) + ')');
      check(payload.project_path === toDelete.project_path,
        'the undo lost the project path (' + JSON.stringify(payload.project_path) + ')');
    }
  }
}

// --------------------------------------------------------------- the result

run().then(function () {
  if (failures.length) {
    console.error('FAIL: ' + failures.length + ' assertion(s) failed');
    failures.forEach(function (failure) { console.error('  - ' + failure); });
    process.exit(1);
  }
  console.log('OK: the task editor offers the status and the priority as two selects in one row, posts the picked values and keeps the code context (path, start line, end line) in one shared row');
}).catch(function (err) {
  console.error('FAIL: the task form test threw: ' + (err && err.stack ? err.stack : err));
  process.exit(1);
});
