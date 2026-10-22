'use strict';
// Regression test for offline bundling and zero external CDN dependencies.
//
// The TaskView web client must run completely offline without any external
// network access:
//   * No external script tags (e.g. cdn.tailwindcss.com)
//   * No external stylesheet tags or font stylesheets
//   * Local precompiled Tailwind CSS (tailwind.min.css) exists and is linked
//   * Precompiled CSS contains necessary framework utilities and custom animations
//
// Usage: node scripts/offline_bundle_test.js [path/to/index.html]
//
// No external network dependencies, no files are written.

const fs = require('fs');
const path = require('path');

const indexPath = path.resolve(
  process.argv[2] || path.join(__dirname, '..', 'src', 'public', 'index.html')
);
const publicDir = path.dirname(indexPath);
const cssPath = path.join(publicDir, 'tailwind.min.css');

const failures = [];
function check(condition, message) {
  if (!condition) failures.push(message);
}

// ----------------------------------------------------------- HTML assertions

check(fs.existsSync(indexPath), 'index.html does not exist at ' + indexPath);
const html = fs.readFileSync(indexPath, 'utf8');

// 1. No external scripts
const scriptTags = html.match(/<script\b[^>]*>/gi) || [];
scriptTags.forEach(function (tag) {
  const srcMatch = tag.match(/\bsrc=["']([^"']+)["']/i);
  if (srcMatch) {
    const src = srcMatch[1];
    check(
      !/^https?:\/\/|^\/\//i.test(src),
      'index.html still loads external script from ' + src
    );
  }
});
check(
  !/cdn\.tailwindcss\.com/.test(html),
  'index.html still references cdn.tailwindcss.com'
);

// 2. No external stylesheets
const linkTags = html.match(/<link\b[^>]*>/gi) || [];
linkTags.forEach(function (tag) {
  const isStylesheet = /rel=["']stylesheet["']/i.test(tag);
  const hrefMatch = tag.match(/\bhref=["']([^"']+)["']/i);
  if (isStylesheet && hrefMatch) {
    const href = hrefMatch[1];
    check(
      !/^https?:\/\/|^\/\//i.test(href),
      'index.html still loads external stylesheet from ' + href
    );
  }
});

// 3. Local tailwind.min.css must be linked
const localCssLinked = /<link\b[^>]*rel=["']stylesheet["'][^>]*href=["']tailwind\.min\.css["']/i.test(html) ||
                       /<link\b[^>]*href=["']tailwind\.min\.css["'][^>]*rel=["']stylesheet["']/i.test(html);
check(localCssLinked, 'index.html does not link to local tailwind.min.css');

// ------------------------------------------------------------ CSS assertions

check(fs.existsSync(cssPath), 'tailwind.min.css does not exist at ' + cssPath);
const css = fs.readFileSync(cssPath, 'utf8');

check(css.length > 5000, 'tailwind.min.css is suspiciously small (' + css.length + ' bytes)');

// Verify that key Tailwind utilities and custom extensions are compiled in
const requiredTokens = [
  'bg-app',
  'text-ink',
  'text-urgent',
  'border-line',
  'shadow-brand',
  'animate-fadeUp',
  'animate-spin',
  'animate-pulse',
];

requiredTokens.forEach(function (token) {
  check(css.indexOf(token) >= 0, 'tailwind.min.css is missing utility: ' + token);
});

// ------------------------------------------------------------------ Summary

if (failures.length) {
  console.error('FAIL: ' + failures.length + ' assertion(s) failed');
  failures.forEach(function (failure) { console.error('  - ' + failure); });
  process.exit(1);
}

console.log('OK: index.html is 100% offline with precompiled tailwind.min.css and zero CDN dependencies');
