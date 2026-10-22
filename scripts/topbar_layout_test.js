'use strict';
// Regression test for the top bar action cluster.
//
// The action cluster (search, filter pill, sort, view switch, theme, refresh,
// new task) has to sit flush against the right edge of the top bar, with the
// breadcrumb alone on the left. The search field therefore must not stretch
// over the free space between them.
//
// The front end is a single HTML file, so the test asserts on the markup of
// the top bar instead of rendering it.
//
// Usage: node scripts/topbar_layout_test.js [path/to/index.html]
//
// No dependencies, no network, no files are written.

const fs = require('fs');
const path = require('path');

const indexPath = path.resolve(
  process.argv[2] || path.join(__dirname, '..', 'src', 'public', 'index.html')
);

const html = fs.readFileSync(indexPath, 'utf8');

const failures = [];
function check(condition, message) {
  if (!condition) failures.push(message);
}

// The top bar runs from the header element to its closing tag.
const headerStart = html.indexOf('<header');
const headerEnd = html.indexOf('</header>', headerStart);
if (headerStart < 0 || headerEnd < 0) {
  console.error('FAIL: no top bar header found in ' + indexPath);
  process.exit(1);
}
const header = html.slice(headerStart, headerEnd);

// The search field is the only element that used to stretch over the free
// space: "flex-1" together with a max width.
const searchBox = header.match(/<div class="[^"]*relative ml-auto[^"]*"[^>]*>/);
if (!searchBox) {
  console.error('FAIL: no search container found in the top bar');
  process.exit(1);
}
const searchClasses = searchBox[0];

check(
  !/\bmd:max-w-\[340px\]/.test(searchClasses),
  'the search field still stretches over the free space (md:max-w-[340px])'
);
check(
  /\bmd:flex-none\b/.test(searchClasses),
  'the search field does not keep a fixed width on wide screens (md:flex-none)'
);
check(
  /\bmd:w-\[240px\]/.test(searchClasses),
  'the search field has no fixed width on wide screens (md:w-[240px])'
);
check(
  /\bflex-1\b/.test(searchClasses),
  'the search field still grows over the free space (flex-1)'
);

// The whole cluster is pushed to the right by the search container.
check(
  /\bml-auto\b/.test(searchClasses),
  'the action cluster is no longer pushed to the right (ml-auto)'
);

// Every control of the cluster is still present and in the documented order.
const order = [
  'id="search"',
  'id="filter-pill"',
  'id="sort"',
  'id="view-board"',
  'id="view-list"',
  'id="theme-toggle"',
  'id="refresh"',
  'id="new-task"',
];
let previous = -1;
for (const marker of order) {
  const at = header.indexOf(marker);
  if (at < 0) {
    failures.push('the top bar lost ' + marker);
    continue;
  }
  if (at < previous) {
    failures.push('the top bar reordered ' + marker);
  }
  previous = at;
}

// The new task button stays the rightmost control.
const newTask = header.indexOf('id="new-task"');
const afterNewTask = header.slice(newTask);
check(
  !/id="(search|filter-pill|sort|view-board|view-list|theme-toggle|refresh)"/.test(afterNewTask),
  'the new task button is no longer the rightmost control'
);

if (failures.length) {
  console.error('FAIL: ' + failures.length + ' assertion(s) failed');
  failures.forEach(function (failure) { console.error('  - ' + failure); });
  process.exit(1);
}

console.log('OK: the top bar action cluster sits flush right with a fixed-width search field');
