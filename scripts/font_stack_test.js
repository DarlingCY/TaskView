'use strict';
// Regression test for the font configuration.
//
// The board must not depend on an external web font: the Google Fonts
// preconnects and stylesheet are gone, and the Tailwind font stack uses the
// operating system fonts only.
//
// Usage: node scripts/font_stack_test.js [path/to/index.html]
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

// No external font service is referenced any more.
check(
  !/fonts\.googleapis\.com/.test(html),
  'the page still loads stylesheets from fonts.googleapis.com'
);
check(
  !/fonts\.gstatic\.com/.test(html),
  'the page still preconnects to fonts.gstatic.com'
);
check(
  !/Geist|Space Grotesk|JetBrains Mono/.test(html),
  'the page still names a downloaded web font'
);

// The font stack is the system one.
check(
  /display:\s*\['system-ui'/.test(html),
  'the display font is not the system stack'
);
check(
  /sans:\s*\['system-ui'/.test(html),
  'the sans font is not the system stack'
);
check(
  /mono:\s*\['ui-monospace'/.test(html),
  'the mono font is not the system stack'
);

// The Chinese UI font is part of the stack, so the text stays readable.
check(
  /"Microsoft YaHei"/.test(html),
  'the system font stack has no Chinese font'
);

if (failures.length) {
  console.error('FAIL: ' + failures.length + ' assertion(s) failed');
  failures.forEach(function (failure) { console.error('  - ' + failure); });
  process.exit(1);
}

console.log('OK: the board uses the system fonts and loads no external font');
