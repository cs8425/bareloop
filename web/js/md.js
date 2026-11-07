// Tiny markdown renderer with an extension point. No dependencies, no build step.
//
// Everything is HTML-escaped first and only the constructs implemented here are turned into
// markup, so pasted model output can never inject script tags. Raw HTML in the source stays
// visible as text on purpose.
//
// Extension point (load a script before app.js, or from the devtools console):
//
//   const md = window.bareloopMarkdown;
//   md.register({
//     name: 'mermaid',
//     code(code, lang) { return lang === 'mermaid' ? `<pre class="mermaid">${code}</pre>` : null; },
//   });
//   md.register({
//     name: 'math',
//     inline(text) { return text.replace(/\$\$(.+?)\$\$/g, '<i>[$1]</i>'); },
//   });
//   md.register({
//     name: 'callout',
//     // Return the index of the next line to process when the block was consumed, else -1.
//     block(lines, i, out, helpers) {
//       if (!lines[i].startsWith('!!!')) return -1;
//       out.push(`<p class="callout">${helpers.escape(lines[i].slice(3))}</p>`);
//       return i + 1;
//     },
//   });
//
// Hooks run in registration order. `code` hooks are tried before the default fence renderer and
// the first one returning a string wins. `inline` hooks run after the built-in inline pass.

const extensions = [];

export function register(ext) {
  if (ext && (ext.block || ext.inline || ext.code)) {
    extensions.push({ name: 'anonymous', ...ext });
  }
}

export function escape(s) {
  return String(s ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// Flanking, spelled out because it is the reason emphasis cannot be a regular expression: whether a run of
// `*` or `_` may open or close depends on the characters on both sides of the whole run, and the answer for
// `_` is deliberately not the answer for `*`. Absorbed here as "absent counts as whitespace", punctuation is
// Unicode's P and S categories (so `(` and `。` count, a Cyrillic letter does not), and the extra clause on
// `_` is what keeps `snake_case_name` plain while `_foo_bar_baz_` still emphasises.
const isFlow = (ch) => ch === undefined || /\s/.test(ch);
const isPunct = (ch) => ch !== undefined && /[\p{P}\p{S}]/u.test(ch);

// emphasis implements CommonMark's delimiter-run matching as far as a renderer with no AST can: find every
// run of `*`/`_`, decide what each of its ends is allowed to be, then let each closer walk back to the
// nearest still-free opener of its own kind. Two rules people notice when they are missing: a closer looks
// back for a second time while a run still has characters left in it (`***foo***` is em wrapped in strong,
// not one strong and two stray asterisks), and the rule of three (`*foo***bar***baz` keeps its middle
// emphasis) — both are load-bearing in real documents and both are invisible in a toy test.
function emphasis(text) {
  const runs = [];
  const scan = /([*_])\1*/gu;
  for (let m = scan.exec(text); m; m = scan.exec(text)) {
    const ch = m[1];
    const before = text[m.index - 1];
    const after = text[m.index + m[0].length];
    const leftFlanking = !isFlow(after) && (!isPunct(after) || isFlow(before) || isPunct(before));
    const rightFlanking = !isFlow(before) && (!isPunct(before) || isFlow(after) || isPunct(after));
    runs.push({
      at: m.index,
      len: m[0].length,
      ch,
      left: m[0].length,
      opens: [],
      closes: [],
      canOpen: ch === '*' ? leftFlanking : leftFlanking && (!rightFlanking || isPunct(before)),
      canClose: ch === '*' ? rightFlanking : rightFlanking && (!leftFlanking || isPunct(after)),
    });
  }
  for (let c = 1; c < runs.length; c++) {
    const closer = runs[c];
    if (!closer.canClose) continue;
    for (let o = c - 1; o >= 0 && closer.left; ) {
      const opener = runs[o];
      if (!opener.left || opener.ch !== closer.ch || !opener.canOpen) {
        o--;
        continue;
      }
      // The rule of three, in the spec's own words: of the two delimiters, the one that can also act as the
      // other way round must not sum to a multiple of three with its partner unless one of them is itself
      // divisible by three. Skipping it is what makes `*foo***bar***baz` come out wrong.
      const bothWays = opener.canClose || closer.canOpen;
      if (bothWays && (opener.left + closer.left) % 3 === 0 && opener.left % 3 !== 0 && closer.left % 3 !== 0) {
        o--;
        continue;
      }
      const use = opener.left >= 2 && closer.left >= 2 ? 2 : 1;
      opener.left -= use;
      closer.left -= use;
      opener.opens.push(use);
      closer.closes.push(use);
      // Everything still unused strictly between the pair is now spent: the text it sat in has just become
      // the content of an element, and a delimiter that failed to match before the span closed cannot start
      // one after it. Without this rule `*foo _bar* baz_` italicises the wrong half of the sentence, because
      // the stray `_` would find an opener that the closing `*` has already swallowed.
      for (let k = o + 1; k < c; k++) {
        runs[k].canOpen = false;
        runs[k].canClose = false;
      }
      if (!opener.left) o--; // spent: the next look-back starts further left
    }
  }
  const tag = (n, close) => (n === 2 ? (close ? '</strong>' : '<strong>') : close ? '</em>' : '<em>');
  let out = '';
  let at = 0;
  for (const run of runs) {
    out += text.slice(at, run.at);
    at = run.at + run.len;
    // Delimiters a run did not spend stay outside whatever element it helped create, which is what `**foo*`
    // is meant to look like. Opening tags are emitted innermost-first — the last pair found is the outer
    // element — and closing tags in the order they were found, so the two ends agree.
    out += run.closes.map((n) => tag(n, true)).join('');
    out += run.ch.repeat(run.left);
    out += run.opens.map((n) => tag(n, false)).reverse().join('');
  }
  return out + text.slice(at);
}

// Brackets outrank asterisks, so links are cut out of the text before emphasis ever sees it, and the finished
// element is parked out of reach until the last step: `*[bar*](/url)` has its asterisks outside the link, and
// they have to come back on the page where they were written.
//
// A destination is a small grammar, not a token — it may be wrapped in angle brackets, may hold balanced
// parentheses, may escape either with a backslash, may carry a title, and may be empty — and writing it as one
// regular expression is how the previous version ended up showing a reader `[link](/uri "title")` and
// `[link](foo(and(bar)))` as their own source code.
//
// Images are the one construct here that is never rendered, on purpose: an `img` asks the network for a picture
// the instant it is inserted, and nothing in this renderer may reach out on its own. The source stays on the
// page, which records what was written without fetching it, and is better than what the old pattern did — a
// stray `!` in front of a live link, which neither recorded the image nor avoided the request.
const labelHtml = (label) => emphasis(label).replace(/~~([^~]+)~~/g, '<del>$1</del>');

const anchor = (url, title, inner) =>
  `<a href="${url}"${title ? ` title="${title}"` : ''} target="_blank" rel="noopener noreferrer">${inner}</a>`;

// A finished element that must survive the rest of the pipeline untouched: `\u0001n\u0001` is opaque to
// emphasis (it reads as one unbroken character, which is exactly what a link is), and `inline` puts the real
// markup back after every rule has had its turn.
function slot(slots, html) {
  slots.push(html);
  return `\u0001${slots.length - 1}\u0001`;
}

// safeHref allow-lists instead of deny-listing. An `href` is the one place a reply could still make the browser
// execute something (`javascript:`, `data:`) now that raw HTML never survives escaping, so the answer is a short
// list of schemes that cannot, not a hunt for the ones that can. Control characters and spaces are stripped
// before the test because that is what a browser does while resolving a URL — `java\nscript:` has to fail on
// the same terms the eventual click would be judged on.
function safeHref(url) {
  const bare = url.replace(/[\u0000-\u0020]/g, '');
  const scheme = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(bare);
  if (!scheme) return true;
  const name = scheme[1].toLowerCase();
  return name === 'http' || name === 'https' || name === 'mailto';
}

// parseTarget reads the `( destination optional-title )` that follows a label, starting at the `(`. It answers
// with the index just past the closing `)`, or null when there is no link there — null is the common case and
// the caller handles it by printing the `[` as text. The `&lt;` and `&quot;` in here are the escaped spellings,
// because the text was escaped before it got here: that ordering is also what keeps `[a](x"onclick="y)` an
// ordinary URL rather than an attribute injected into the element we are building.
function parseTarget(text, start) {
  let at = start + 1;
  let space = () => text[at] === ' ' || text[at] === '\t';
  // A line ending may separate the destination from the title, or the title from the closing `)` — an author
  // wrapping a long link across lines is common enough that refusing it costs a real link.
  let gap = () => /^[ \t\n]/.test(text[at] || '');
  while (space()) at++;
  let url = '';
  let angle = false;
  let depth = 0;
  if (text.startsWith('&lt;', at)) {
    angle = true;
    at += 4;
  }
  while (at < text.length) {
    const c = text[at];
    if (angle) {
      if (text.startsWith('&gt;', at)) {
        at += 4;
        angle = false;
        break;
      }
      if (c === '\n') return null;
      url += c;
      at++;
      continue;
    }
    if (c === '\\' && text[at + 1] && !/\s/.test(text[at + 1])) {
      // `\` in front of an entity swallows the backslash and leaves the entity alone: `&gt;` is one character
      // to a reader, so reading `\&` as "an escaped ampersand" here would eat half of it and leave a
      // destination that opened `&lt;` and never finds its close.
      const entity = text[at + 1] === '&' ? /^&[A-Za-z]+;|^&#\d+;|^&#[xX][0-9a-fA-F]+;/.exec(text.slice(at + 1)) : null;
      if (entity) {
        url += entity[0];
        at += entity[0].length + 1;
        continue;
      }
      url += text[at + 1];
      at += 2;
      continue;
    }
    if (c === '\n' || c === ' ' || c === '\t') break;
    if (c === '(') depth++;
    else if (c === ')') {
      if (!depth) break;
      depth--;
    }
    url += c;
    at++;
  }
  if (angle) return null; // a destination that opened `&lt;` and never closed it is not a destination
  if (url.includes(' ')) url = url.replace(/ /g, '%20'); // only the angle form can hold a space at all

  let title = '';
  const afterDest = at; // a title must be separated from the destination by whitespace, or it is not one
  while (gap()) at++;
  const quote = at > afterDest ? (text.startsWith('&quot;', at) ? '&quot;' : text[at] === "'" ? "'" : '') : '';
  if (quote) {
    const end = text.indexOf(quote, at + quote.length);
    const body = text.slice(at + quote.length, end);
    if (end < 0 || body.includes('\n')) return null;
    title = body;
    at = end + quote.length;
    while (gap()) at++;
  } else if (text[at] === '(') {
    const end = text.indexOf(')', at + 1);
    const body = text.slice(at + 1, end);
    if (end < 0 || body.includes('\n')) return null;
    title = body;
    at = end + 1;
    while (gap()) at++;
  }
  if (text[at] !== ')') return null;
  return { at: at + 1, url, title };
}

function links(text, slots) {
  let out = '';
  let i = 0;
  while (i < text.length) {
    const open = text.indexOf('[', i);
    if (open < 0) {
      out += text.slice(i);
      break;
    }
    out += text.slice(i, open);
    let close = -1;
    if (text[open - 1] !== '!') {
      let depth = 0;
      for (let j = open; j < text.length; j++) {
        if (text[j] === '\\') j++;
        else if (text[j] === '[') depth++;
        else if (text[j] === ']' && --depth === 0) {
          close = j;
          break;
        }
      }
    }
    // A link label may not contain another link, so when it does, these brackets are not a link and the inner
    // one is — which the next round of this loop finds, because `continue` below only moves past the `[`.
    // `[foo [bar](/uri)](/uri)` is therefore `foo` and a link and some loose brackets, exactly as CommonMark
    // renders it, instead of one link whose text repeats the syntax it just swallowed.
    const label = close > 0 ? text.slice(open + 1, close) : '';
    const nested = close > 0 && label.includes('](');
    // Two grammars share this start: `[a](dest)` is an inline link, while `[a][ref]`, `[a][]` and `[a]` ask a
    // definition collected earlier in the document.
    let target = null;
    if (close > 0 && !nested) {
      target = text[close + 1] === '(' ? parseTarget(text, close + 1) : refTarget(text, close, label);
    }
    if (!target) {
      out += '[';
      i = open + 1;
      continue;
    }
    out += safeHref(target.url)
      ? slot(slots, anchor(target.url, target.title, labelHtml(label)))
      : text.slice(open, target.at); // an unsafe scheme is shown as written, never followed
    i = target.at;
  }
  return out;
}

// Angle autolinks and email autolinks, same allow-list as every other link: a URL spelled out in full between
// angle brackets is a link, and leaving it escaped shows the reader a thing that looks like a tag instead.
// CommonMark's other form — a bare `www.foo.com` with no brackets at all — is not done, because deciding where
// a host name ends is a heuristics exercise that turns version numbers into links.
function autolinks(text, slots) {
  return text
    .replace(/&lt;((?:https?|mailto):[^\s<>]*?)&gt;/g, (m, url) => slot(slots, anchor(url, '', url)))
    .replace(/&lt;([\w.+-]+@[\w-]+(?:\.[\w-]+)+)&gt;/g, (m, mail) => slot(slots, anchor(`mailto:${mail}`, '', mail)));
}

// A link reference definition — `[foo]: /url "title"` — says "this label means this place", and the line is
// consumed rather than rendered: `[foo]: /url` on its own renders nothing at all. Labels match
// case-insensitively with runs of whitespace collapsed, the first definition of a label wins, and a definition
// may live anywhere in the document — below the link that uses it, or inside a blockquote. That forward reach
// is the one construct whose answer is not inside its own block, which is acceptable here for one reason: the
// last frame of a stream is a single clean render of the whole reply (see `finish` in transcript.js), so what
// a reader ends up looking at is the resolved document, not whatever the frozen prefix could resolve.
//
// Definitions belong to the document rather than to the text being rendered, so `render` collects them before
// anything is rendered and `links` reads them from here; saving and restoring them around `render` is what
// lets an extension render markdown of its own without meeting its host document's labels.
let docRefs = new Map();
let docDefLines = new Set();

const normLabel = (label) => label.trim().replace(/\s+/g, ' ').toLowerCase();
const isAsciiPunct = (c) => /[!-/:-@\[-`{-~]/.test(c);

// A definition is `[label]: destination title?`, and any part of it may be followed by a line ending, because
// authors wrap long destinations and put the title on the next line. It may not contain a blank line, and it
// is only a definition if nothing is left over on the last line it touches: `[foo]: <bar>(baz)` stays text,
// since a title in parentheses has to be separated from its destination by whitespace.
//
// The lines a definition could span are joined so one cursor can walk the grammar, and the line endings it
// steps over are what the caller swallows — which is how a definition followed by `"title" ok` drops itself and
// keeps the paragraph.
function parseDef(lines, at) {
  let s = '';
  for (let j = at; j < lines.length && !/^\s*$/.test(lines[j]); j++) s += (j > at ? '\n' : '') + lines[j];
  if (!/^ {0,3}\[/.test(s)) return null; // four spaces of indentation is a code block, not a definition
  const linesUnder = (upto) => {
    let n = 0;
    for (let j = 0; j < upto; j++) if (s[j] === '\n') n++;
    return n;
  };

  let i = s.indexOf('[') + 1;
  let label = '';
  let closed = false;
  while (i < s.length) {
    const c = s[i];
    if (c === '\\') {
      label += c + (s[i + 1] === undefined ? '' : s[i + 1]); // the key is the label as written — the escape is
      i += 2; //                                                undone later, when the link itself is rendered
      continue;
    }
    if (c === '[') return null; // a label may not nest
    if (c === ']') {
      closed = true;
      i++;
      break;
    }
    label += c;
    i++;
  }
  if (!closed || !label.trim() || s[i] !== ':') return null; // `[foo] bar` is a paragraph
  i++;
  while (i < s.length && /\s/.test(s[i])) i++; // and a line ending may follow the colon

  let url = '';
  if (s[i] === '<') {
    i++;
    while (i < s.length && s[i] !== '>' && s[i] !== '<') {
      if (s[i] === '\\' && isAsciiPunct(s[i + 1] || '')) {
        url += s[i + 1];
        i += 2;
        continue;
      }
      url += s[i] === '\n' ? ' ' : s[i];
      i++;
    }
    if (s[i] !== '>') return null;
    i++;
    url = url.trim().replace(/ /g, '%20'); // only the angle form may hold a space, and a space is %20
  } else {
    let depth = 0;
    while (i < s.length && !/\s/.test(s[i]) && s[i] !== '<') {
      const c = s[i];
      if (c === '\\' && isAsciiPunct(s[i + 1] || '')) {
        url += s[i + 1];
        i += 2;
        continue;
      }
      if (c === '(') depth++;
      if (c === ')') {
        if (depth === 0) break;
        depth--;
      }
      url += c;
      i++;
    }
    if (depth !== 0) return null; // `foo(and(bar` is not a destination, so neither is the line
  }

  // The title is an attempt rather than a requirement: if it cannot be closed, or closes and leaves something
  // behind before the line ends, then the definition was only ever the destination.
  let title = null;
  let j = i;
  while (j < s.length && /\s/.test(s[j])) j++;
  // A title has to be separated from its destination by whitespace, which is what makes `[foo]: <bar>(baz)`
  // text rather than a definition carrying the title `baz`.
  const quote = j > i ? (s[j] === '"' || s[j] === "'" ? s[j] : s[j] === '(' ? ')' : '') : '';
  if (quote) {
    let k = j + 1;
    let body = '';
    while (k < s.length && s[k] !== quote) {
      if (s[k] === '\\' && isAsciiPunct(s[k + 1] || '')) {
        body += s[k + 1];
        k += 2;
        continue;
      }
      body += s[k++];
    }
    if (s[k] === quote) {
      let m = k + 1;
      while (m < s.length && /[ \t]/.test(s[m])) m++;
      if (m === s.length || s[m] === '\n') {
        title = body;
        i = m;
      }
    }
  }
  if (title === null) {
    let m = i;
    while (m < s.length && /[ \t]/.test(s[m])) m++;
    if (m !== s.length && s[m] !== '\n') return null; // leftovers: this was never a definition
    i = m;
  }

  return { end: at + linesUnder(i), label, url, title: title === null ? '' : title };
}

// collectDefs walks the document once, in block terms: a definition is a definition only where a block may
// start, not inside a fenced block, and not interrupting a paragraph. Consumed line numbers come back with
// the table so `render` can drop exactly these lines and keep everything else.
function collectDefs(lines) {
  const refs = new Map();
  const consumed = new Set();
  let fence = null;
  let paraOpen = false; // a definition cannot interrupt a paragraph, so this is the state that matters
  let quoteOpen = false; // and inside a blockquote, only where a paragraph inside it would start
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (fence) {
      if (new RegExp('^\\s*' + fence).test(line)) fence = null;
      continue;
    }
    const open = /^ {0,3}(```|~~~)/.exec(line);
    if (open) {
      fence = open[1];
      paraOpen = false;
      quoteOpen = false;
      continue;
    }
    if (/^\s*$/.test(line)) {
      paraOpen = false;
      quoteOpen = false;
      continue;
    }
    if (/^ {0,3}>/.test(line)) {
      const inner = line.replace(/^ {0,3}>\s?/, '');
      const def = paraOpen || quoteOpen ? null : parseDef([inner], 0);
      if (def) consumed.add(i);
      if (def) {
        const key = normLabel(def.label);
        if (key && !refs.has(key)) refs.set(key, { url: def.url, title: def.title });
      }
      quoteOpen = !/^\s*$/.test(inner);
      paraOpen = false;
      continue;
    }
    quoteOpen = false;
    // Everything else that may hold the first line of a block starts with a character this grammar cannot
    // begin with, so a heading, a rule, a list item or a table row simply means "a block boundary here".
    if (/^ {0,3}(#{1,6}\s|\||([-*_])\s*(\2\s*){2,}$)|^ {0,3}([-*+]|\d+[.)])\s/.test(line)) {
      paraOpen = false;
      continue;
    }
    const def = paraOpen ? null : parseDef(lines, i);
    if (!def) {
      paraOpen = true;
      continue;
    }
    const key = normLabel(def.label);
    if (key && !refs.has(key)) refs.set(key, { url: def.url, title: def.title }); // the first definition wins
    for (let j = i; j <= def.end; j++) consumed.add(j);
    i = def.end;
    paraOpen = false;
  }
  return { refs, consumed };
}

// The reference forms of a link: `[text][label]`, `[text][]`, and the shortcut `[label]`. An undefined label is
// not a link at all and the brackets stay, which is why ordinary prose saying `[sic]` is untouched unless this
// document happens to define `sic`.
function refTarget(text, close, label) {
  let key = normLabel(label);
  let at = close + 1;
  if (text[at] === '[') {
    const end = text.indexOf(']', at + 1);
    if (end < 0) return null;
    const inner = normLabel(text.slice(at + 1, end));
    if (inner) key = inner; // `[text][]` means the label the author already wrote
    at = end + 1;
  }
  const ref = docRefs.get(key);
  return ref ? { at, url: ref.url, title: ref.title } : null;
}

// Code spans and backslash escapes are the two inline rules that take what is between their marks exactly as
// written, so a single left-to-right pass handles both before anything else reads a character. One pass, not
// two, because they can each swallow the other's opening mark: whoever comes first wins, which is what makes
// `` \`a\` `` a pair of literal backticks around `a` while `` `a\*b` `` keeps its backslash. A backslash that
// escapes nothing (`\x`, `\2`) stays exactly as written, because in CommonMark only punctuation can be escaped.
//
// The text an escape stands for is kept as an entity (`\<` arrives here as `\&lt;` after `escape`), and
// the code span's contents were escaped once at the top of `inline` — neither may be escaped a second time on
// the way out, which is why both go into placeholders rather than straight into the HTML.
function rawPass(text, spans, esc) {
  let out = '';
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    if (c === '\\') {
      const entity = /^&(?:[A-Za-z]+|#\d+|#[xX][0-9a-fA-F]+);/.exec(text.slice(i + 1));
      if (entity) {
        esc.push(entity[0]);
        out += `\u0002${esc.length - 1}\u0002`;
        i += entity[0].length + 1;
        continue;
      }
      const next = text[i + 1];
      if (next === '\n') {
        out += '\n'; // a backslash at the end of a line is that line's break; here every line ending is one
        i += 2;
        continue;
      }
      if (next !== undefined && isAsciiPunct(next)) {
        esc.push(next);
        out += `\u0002${esc.length - 1}\u0002`;
        i += 2;
        continue;
      }
      out += c;
      i++;
      continue;
    }
    if (c === '`') {
      let run = 1;
      while (text[i + run] === '`') run++;
      // A code span opens with a run of N backticks and closes at the next run of exactly N; a longer run is
      // content, which is how `` ``a`b`` `` holds a backtick.
      let j = i + run;
      let close = -1;
      while (close < 0) {
        const k = text.indexOf('`', j);
        if (k < 0) break;
        let n = 1;
        while (text[k + n] === '`') n++;
        if (n === run) close = k;
        else j = k + n;
      }
      if (close < 0) {
        out += '`'.repeat(run);
        i += run;
        continue;
      }
      let code = text.slice(i + run, close).replace(/\n/g, ' ');
      if (code.length > 2 && code.startsWith(' ') && code.endsWith(' ') && code.trim() !== '') {
        code = code.slice(1, -1); // one space at each end is the author keeping `a` apart from the ticks
      }
      spans.push(code);
      out += `\u0000${spans.length - 1}\u0000`;
      i = close + run;
      continue;
    }
    out += c;
    i++;
  }
  return out;
}

// ---- character references (§2.5) ----------------------------------------------
//
// References are looked for in the ALREADY-ESCAPED stream — `&amp;#39;` is the source's `&#39;` — and the
// character they stand for is put back already escaped again. Both halves matter: the second is why `&lt;script&gt;`
// stays a string about a tag instead of becoming one, and the first is why a reference that this renderer wrote
// itself (an `&amp;` from escaping a bare ampersand) is not mistaken for one the author typed.
//
// It runs after emphasis and links, so a decoded `*` cannot start emphasis (§2.5: `&#42;foo&#42;` is not
// `<em>foo</em>`) and before the placeholders are restored, so a code span keeps its backslashes and a link's
// `href` is left for the browser, which decodes entities inside attribute values on its own.
//
// Only names in `NAMED` are references; any other `&word;` is text and stays exactly as written. That is a
// deliberate narrowing of HTML's ~2200 named names to the ones a reply is likely to hold — the mathematical
// names the spec's own examples use (`&Identical;`, `&ngE;`) are not among them, and HTML's five legacy names
// that work without a semicolon (`&copy`, `&nbsp`) are not either.
const NAMED = (() => {
  const map = {
    amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: '\u00a0',
    copy: '\u00a9', reg: '\u00ae', trade: '\u2122', hellip: '\u2026',
    ndash: '\u2013', mdash: '\u2014', lsquo: '\u2018', rsquo: '\u2019',
    ldquo: '\u201c', rdquo: '\u201d', laquo: '\u00ab', raquo: '\u00bb',
    bull: '\u2022', middot: '\u00b7', deg: '\u00b0', plusmn: '\u00b1',
    sect: '\u00a7', para: '\u00b6', dagger: '\u2020', permil: '\u2030',
    cent: '\u00a2', pound: '\u00a3', yen: '\u00a5', euro: '\u20ac',
    curren: '\u00a4', brvbar: '\u00a6', uml: '\u00a8', ordf: '\u00aa',
    not: '\u00ac', shy: '\u00ad', macr: '\u00af', acute: '\u00b4',
    micro: '\u00b5', cedil: '\u00b8', sup1: '\u00b9', sup2: '\u00b2',
    sup3: '\u00b3', iquest: '\u00bf', iexcl: '\u00a1', frac12: '\u00bd',
    frac14: '\u00bc', frac34: '\u00be', Prime: '\u2033',
  };
  // The Latin-1 letters are two unbroken runs of code points (0xC0–0xDF, 0xE0–0xFF — `multiply` and `divide` sit
  // in what would otherwise be the holes), so they are generated from their names rather than typed as glyphs:
  // a table of 64 accents is one mistyped character away from a wrong letter no reviewer will notice. Greek is
  // NOT generated this way, because its runs have gaps (0x3A2 is reserved, and σ/ς are two code points for one
  // name) and a generated table with a gap silently shifts every letter after it.
  const latin = (base) => (name, k) => { map[name] = String.fromCodePoint(base + k); };
  'Agrave Aacute Acirc Atilde Auml Aring AElig Ccedil Egrave Eacute Ecirc Euml Igrave Iacute Icirc Iuml ETH Ntilde Ograve Oacute Ocirc Otilde Ouml multiply Oslash Ugrave Uacute Ucirc Uuml Yacute THORN szlig'
    .split(' ').forEach(latin(0xc0));
  'agrave aacute acirc atilde auml aring aelig ccedil egrave eacute ecirc euml igrave iacute icirc iuml eth ntilde ograve oacute ocirc otilde ouml divide oslash ugrave uacute ucirc uuml yacute thorn yuml'
    .split(' ').forEach(latin(0xe0));
  return map;
})();

function entities(s) {
  return s.replace(/&amp;(?:#([0-9]+)|#[xX]([0-9a-fA-F]+)|([a-zA-Z][a-zA-Z0-9]{1,31}));/g, (all, dec, hex, name) => {
    if (name !== undefined) {
      const known = NAMED[name];
      return known === undefined ? all : escape(known); // `&notanentity;` is a word, not a reference
    }
    const cp = parseInt(dec !== undefined ? dec : hex, dec !== undefined ? 10 : 16);
    // §2.5: a reference to nothing (0), to something outside Unicode, or to a lone surrogate is the replacement
    // character — the author asked for a character and there is none to give them.
    if (cp === 0 || cp > 0x10ffff || (cp >= 0xd800 && cp <= 0xdfff)) return '\ufffd';
    return escape(String.fromCodePoint(cp));
  });
}

function inline(text) {
  let out = escape(text);
  // Code spans and escapes are taken out first, in one pass (see `rawPass`), so no other rule can read a
  // character that the author wrote as literal.
  const spans = [];
  const slots = [];
  const esc = [];
  out = rawPass(out, spans, esc);
  // Emphasis is one pass over the delimiter runs (see above), not a chain of patterns: a chain of patterns
  // cannot tell `a * foo bar*` from `foo*bar*` and this renderer was wrong in both directions — emphasising
  // a run that opens onto a space, and refusing the intraword emphasis `*` is allowed.
  out = links(out, slots);
  out = autolinks(out, slots);
  out = emphasis(out).replace(/~~([^~]+)~~/g, '<del>$1</del>');
  // Character references are decoded last among the rules that read text, for the two reasons above: nothing a
  // reference stands for may take part in the syntax that has already been decided, and nothing it produces may
  // reach the document without going through `escape`.
  out = entities(out);
  // `spans` holds text that was escaped once, at the top of this function, so it goes into the element as it
  // is. Escaping it a second time here is what made `info:"connected"` print `info:&quot;connected&quot;` and
  // `a & b` print `a &amp; b` — every quote, ampersand and angle bracket in a code span, shown as its own
  // entity. (Code spans are cut out before the other rules and put back last, which is the only reason the
  // text passes through `escape` twice on its way here and never got caught.)
  // Slots first, code spans second: a label that held a code span carries its placeholder inside the finished
  // `<a>`, so the element has to be back in the text before the placeholders inside it are resolved. Restoring
  // them the other way round left `\u0000` characters in the page for every `` [`code`](/url) ``.
  out = out
    .replace(/\u0001(\d+)\u0001/g, (_, i) => slots[Number(i)])
    .replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${spans[Number(i)]}</code>`)
    .replace(/\u0002(\d+)\u0002/g, (_, i) => esc[Number(i)]); // last: it may sit inside an element put back above
  for (const ext of extensions) {
    if (ext.inline) {
      const next = ext.inline(out);
      if (typeof next === 'string') out = next;
    }
  }
  return out;
}

const isTableSep = (line) => /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(line) && line.includes('-');

// splitRow cuts one table row into cells. A `|` divides a cell only when it is not escaped: a row saying
// `run{status:ok\|error}` — which is what a pipe inside a `code` span costs you in a table, and the spelling
// GFM tells an author to use — is one cell holding `run{status:ok|error}`, not two cells each wearing a stray
// backslash and losing the code span that was wrapping them. This row does its own unescaping only because it
// runs on the raw line, before `inline` has met a single character of it; once a cell gets there, `\|` is
// already an escaped pipe like any other escaped punctuation.
function splitRow(line) {
  const cells = [];
  let cell = '';
  let k = 0;
  while (k < line.length) {
    if (line[k] === '\\' && line[k + 1] === '|') {
      cell += '|';
      k += 2;
      continue;
    }
    if (line[k] === '|') {
      cells.push(cell);
      cell = '';
      k++;
      continue;
    }
    cell += line[k];
    k++;
  }
  cells.push(cell);
  // The pipes at the two ends of a row frame it rather than divide it. Read off the line, the way the
  // `^\s*\|` / `\|\s*$` pair this replaces did, so a cell that legitimately holds only spaces stays a cell.
  const edge = line.trim();
  if (edge.startsWith('|')) cells.shift();
  if (edge.endsWith('|') && !edge.endsWith('\\|')) cells.pop();
  return cells.map((c) => c.trim());
}

// ---- lists -----------------------------------------------------------------
//
// A list is the one construct here that does not end at a blank line, and that single fact is why the block
// layer needs a scanner rather than a regex per line: `- a`, blank, `- b` is ONE loose list (§5.2), not two
// tight ones. `walkList` is now the only place that decides where a list ends, and `lastSafeBoundary` asks it
// the same question the renderer asks — “is this block over?” answered in two different places is exactly how a
// frozen prefix comes to end with `</ul>` a moment before the list gains its next item.

// A list item's marker: indentation, a bullet or `1.`/`1)`, then the item's first characters. Five or more
// spaces after a marker is indented code inside the item, so it does not continue a list.
const listItemLine = (line) => /^([ \t]*)([-*+]|\d{1,9}[.)])([ \t]+|$)(.*)$/.exec(line);

// walkList consumes the maximal run of lines that form the ONE list beginning at `at`. Three kinds of line keep
// the current item going: another marker at the list's own indentation; a line indented past that marker, which
// is the item's content and is dedented by its content column; and a lazy paragraph continuation — unindented,
// not a block start, allowed by §5.2 and constant in model output. A blank line ends the list only once the next
// text line turns out not to be another item of this list.
//
// `items` may be null: the streaming freeze needs the end of the run and nothing else, and building item
// content on every frame would make the freeze costlier than the rendering it exists to avoid.
function walkList(lines, at, items) {
  const first = listItemLine(lines[at]);
  const indent = first[1].length;
  const ordered = /\d/.test(first[2]);
  const start = ordered ? Number(first[2].slice(0, -1)) : 1;
  let i = at;
  let blanks = 0;
  let count = 0; // items consumed, so the cheap walk still knows it is inside one when it is handed no array
  let contentCol = 0;
  while (i < lines.length) {
    const cur = lines[i];
    if (/^[ \t]*$/.test(cur)) {
      blanks++;
      i++;
      continue;
    }
    const m = listItemLine(cur);
    // A marker at the run's own indentation is the next item. One indented past that but *shallower than the
    // content column* is the next item too, since it cannot be a nested list's item — that is what keeps `- foo`
    // / ` - bar` one list rather than one item containing a second list.
    const joinsRun = m !== null && (m[1].length === indent || (m[1].length > indent && m[1].length < contentCol));
    if (m && joinsRun && /\d/.test(m[2]) === ordered && (i === at || m[3].length < 5) && !isThematic(cur)) {
      // An item whose marker has nothing after it still reserves one space of content column, or `-
      //   foo` would dedent to “ foo” and keep a space the reader never wrote.
      contentCol = indent + m[2].length + Math.max(m[3].length, 1);
      // GFM's task marker (§6.3) arrives as `- [ ] todo`. The spec renders it as a disabled `<input>`, which this
      // renderer does not emit: a checkbox nobody can uncheck is decoration, and one anybody can uncheck is a
      // control inside a message with nothing behind it. A box glyph says the same thing as text, so the marker
      // becomes one and the rest of the line is an ordinary item.
      const task = /^\[( |x|X)\][ \t]/.exec(m[4]);
      const body = task ? (task[1] === ' ' ? '☐ ' : '☑ ') + m[4].slice(task[0].length) : m[4];
      if (items) items.push({ lines: [body], beforeBlank: blanks > 0, innerBlank: false });
      count++;
      blanks = 0;
      i++;
      continue;
    }
    // `count`, not `items.length`: the cheap walk passes no array, and if the two modes disagreed about where
    // the run ends then the freeze would disagree with the renderer — the exact failure this scanner exists to
    // prevent, so it must not be reintroduced for the sake of skipping a few string pushes.
    if (count === 0) break;
    const last = items ? items[count - 1] : null;
    const lead = cur.match(/^[ \t]*/)[0].length;
    // Only a line indented as far as the item's content column is the item's own, and it is dedented by that
    // column — which is what makes `- a` / `  - b` hand the item the line `- b`, so a nested list becomes a child
    // of the `<li>` instead of a `<ul>` floating inside a `<ul>`. Anything shallower may only lazily continue the
    // paragraph beneath it, and a blank line has ended that paragraph: after a blank line the run is over. That
    // last half was missing, and `- one` / blank / ` two` therefore glued a loose paragraph into the bullet — the
    // reader saw a second paragraph inside an item that never held it.
    if (lead >= contentCol) {
      if (docDefLines.has(i)) {
        blanks = 0; // a definition inside an item is read by the document and leaves the item, as in a quote
        i++;
        continue;
      }
      if (last && blanks) { last.innerBlank = true; last.lines.push(''); } // a blank inside an item separates its blocks
      if (last) last.lines.push(cur.slice(contentCol));
      blanks = 0;
      i++;
      continue;
    }
    if (blanks === 0 && !startsNewBlock(cur)) {
      if (last) last.lines.push(cur.replace(/^[ \t]+/, '')); // lazy continuation
      i++;
      continue;
    }
    break;
  }
  // The blank lines at the end were consumed while asking the question. They belong to the document, and a
  // boundary after one of them is safe, so the run stops before them.
  return { next: i - blanks, start, ordered };
}

// renderList renders the list starting at `at` as one element, with each item's content walked by the same
// block layer that is walking this one (§5.3: an item's content is a block-level content of its own).
function renderList(lines, at) {
  const items = [];
  const { next, start, ordered } = walkList(lines, at, items);
  // Loose when an item is separated from its neighbour by a blank line or holds two blocks with one between
  // them; loose means every item's paragraphs become `<p>`, tight means the item's text is the item.
  const loose = items.some((it) => it.beforeBlank || it.innerBlank);
  const tag = ordered ? 'ol' : 'ul';
  let html = ordered && start !== 1 ? `<${tag} start="${start}">` : `<${tag}>`;
  for (const it of items) html += `<li>${renderBlocks(it.lines, { tight: !loose, nested: true })}</li>`;
  return { html: html + `</${tag}>`, next };
}

// A thematic break — `***`, `- - -`, `___`. It wins over the list item it resembles (§4.1), which is what makes
// `***` written under a bullet a break rather than another bullet. Up to three spaces of indent, like every
// block here: four is indented code, which this renderer deliberately does not treat as anything.
const isThematic = (line) => /^ {0,3}([-*_])([ \t]*\1){2,}[ \t]*$/.test(line);

// Whether a line could open a block of its own. This is what decides whether a line may lazily continue the
// paragraph inside an open container (§5.1): a line that starts a block ends the container instead.
const startsNewBlock = (line) =>
  listItemLine(line) !== null ||
  /^ {0,3}>/.test(line) ||
  /^ {0,3}#{1,6}([ \t]|$)/.test(line) ||
  /^ {0,3}(```|~~~)/.test(line) ||
  isThematic(line);

// walkQuote consumes the block quote that starts at `at`, returning its own lines with ONE `>` level stripped —
// so `>>` arrives as `>` and the block walk below turns it into a nested `<blockquote>` — and where the quote
// ends. Pass no array to ask for the extent alone.
//
// A blank line belongs to the quote while some later line still opens with `>`, which is what makes `> a`,
// blank, `> b` one quote holding two paragraphs; it is also why `lastSafeBoundary` may not cut there. A line
// without `>` lazily continues the quote's last paragraph only if it could not start a block of its own —
// `> foo` followed by `- bar` is a quote and then a list.
function walkQuote(lines, at, body) {
  const push = (text) => { if (body) body.push(text); };
  let i = at;
  let blanks = 0;
  while (i < lines.length) {
    const cur = lines[i];
    if (/^ {0,3}>/.test(cur)) {
      if (body && blanks && body.length) push(''); // one blank line is a paragraph break inside the quote
      if (body && !docDefLines.has(i)) push(cur.replace(/^ {0,3}>[ \t]?/, '')); // a definition inside a quote
      blanks = 0; //                                 counts, and disappears from the quote as well
      i++;
      continue;
    }
    if (/^[ \t]*$/.test(cur)) {
      blanks++;
      i++;
      continue;
    }
    if (blanks === 0 && !startsNewBlock(cur)) {
      push(cur.replace(/^[ \t]+/, '')); // lazy continuation of the quoted paragraph
      i++;
      continue;
    }
    break;
  }
  return { next: i - blanks };
}

// renderQuote renders the quote as one element whose content is walked by the same block layer, which is the
// whole point of this function: `> ## Title`, `> - item` and a fence inside a quote were previously joined into
// one paragraph of text, because the quote was the only container that did not recurse.
function renderQuote(lines, at) {
  const body = [];
  const { next } = walkQuote(lines, at, body);
  return { html: `<blockquote>${renderBlocks(body, { nested: true })}</blockquote>`, next };
}

// containerRunMask marks the lines belonging to an open container run — a list or a block quote — the blank
// lines between their items or paragraphs included, because those are the blank lines the streaming freeze may
// not cut after. Block starts inside a fenced block read as ordinary text in here, which can only ever mark too
// many lines as “still inside a container”: a boundary that goes unclaimed costs one more repaint, where one
// taken too early costs a document the reader already watched change under their cursor.
function containerRunMask(lines) {
  const mask = new Uint8Array(lines.length);
  let i = 0;
  while (i < lines.length) {
    let next = 0;
    if (listItemLine(lines[i])) next = walkList(lines, i, null).next;
    else if (/^ {0,3}>/.test(lines[i])) next = walkQuote(lines, i, null).next;
    if (next > i) {
      for (let n = i; n < next; n++) mask[n] = 1;
      i = next;
      continue;
    }
    i++;
  }
  return mask;
}

// ---- fenced code ------------------------------------------------------------
//
// An opening fence is up to three spaces, three or more of one character, then the info string. That string may
// say anything except contain a backtick inside a backtick fence (§4.5), which is what lets
// ```` ```py {line-numbers=3} ```` and `~~~~ ruby startline=3 $%@#$` open a code block instead of degenerating
// into a paragraph of prose — in developer chat the fenced block usually IS the message, so an info string that
// refuses to open one is the most visible kind of bug this file can have.
// `lastSafeBoundary` calls these same helpers, for the standing reason: a boundary that recognises a different
// set of fences than the renderer would cut inside a code block and freeze half of it in place.
const fenceOpen = (line) => {
  const m = /^( {0,3})(`{3,}|~{3,})(.*)$/.exec(line);
  if (!m) return null;
  if (m[2][0] === '`' && m[3].includes('`')) return null;
  return { marker: m[2], info: m[3], indent: m[1].length };
};

// A closing fence is the same character, at least as long as the one that opened, with nothing after it but
// spaces — so a line like ```` ``` aaa ```` is code, not the end of the block.
const fenceClose = (line, marker) => new RegExp('^ {0,3}' + marker[0] + '{' + marker.length + ',}[ \\t]*$').test(line);

function codeBlock(code, lang) {
  for (const ext of extensions) {
    if (ext.code) {
      const html = ext.code(code, lang);
      if (typeof html === 'string') return html;
    }
  }
  const cls = lang ? ` class="lang-${escape(lang)}"` : '';
  return `<pre><code${cls}>${escape(code)}</code></pre>`;
}

// renderBlocks walks an array of lines and returns the document's HTML. It takes lines rather than text
// because a list item's content is itself a document (CommonMark §5.3), and the only way to keep the two
// readings of “what is a block” from drifting apart is for there to be one loop.
//
// `opts.tight` is the one piece of context a container passes down: in a tight list item a paragraph is the
// item's own text, in a loose one it is a `<p>` (§5.4.1). Everything else — nesting, blank lines, a table or a
// fence inside an item — needs no flag, because the same walk simply runs again on the item's lines.
// Definitions are the exception: they are collected for the whole document before the walk and are NOT
// re-collected inside an item, so a `[label]: url` written inside a list item stays visible text. It is a
// deliberate limit — an item that quietly redefined a reference elsewhere in the reply would be a worse
// surprise than a line a reader can see.
function renderBlocks(lines, opts = {}) {
  const savedRefs = [docRefs, docDefLines];
  if (opts.nested !== true) ({ refs: docRefs, consumed: docDefLines } = collectDefs(lines));
  else docDefLines = new Set(); // the outer set indexes the outer array; its numbers mean nothing down here
  const out = [];
  const helpers = { inline, escape };
  let i = 0;
  // A block type that consumes no line would spin forever here, and a renderer that hangs is worse than one
  // that renders the wrong thing: it takes the tab, and with it the reply the reader was waiting for. So the
  // loop watches its own index and says so loudly, with the line that did it, instead of going quiet.
  let stalledAt = -1;
  let para = [];
  const flushPara = () => {
    if (para.length) {
      // Trailing spaces are consumed by a line break (they ARE the hard break in §4.6), so they must not reach
      // the document: invisible on screen, but they survive a reader selecting the text and pasting it.
      const body = inline(para.map((l) => l.replace(/[ \t]+$/, '')).join('\n')).replace(/\n/g, '<br>');
      out.push(opts.tight ? body : `<p>${body}</p>`);
      para = [];
    }
  };

  while (i < lines.length) {
    if (i === stalledAt) throw new Error(`md: block layer made no progress at line ${i + 1}: ${JSON.stringify(String(lines[i]).slice(0, 60))}`);
    stalledAt = i;
    const line = lines[i];
    if (docDefLines.has(i)) {
      i++;
      continue; // a definition was already read by the pass above, and is not part of the document's text
    }

    // Fenced code (§4.5) — see `fenceOpen` for what opens one. A fence never closed runs to the end of the
    // document, which is what a reply that never emitted the closing fence deserves, and what the streaming
    // freeze already assumes.
    const fence = fenceOpen(line);
    if (fence) {
      flushPara();
      const lang = fence.info.trim().split(/[ \t]/)[0]; // the first word is the language; the rest is for readers
      const unindent = fence.indent > 0 ? new RegExp('^ {1,' + fence.indent + '}') : null;
      const buf = [];
      i++;
      while (i < lines.length && !fenceClose(lines[i], fence.marker)) {
        buf.push(unindent ? lines[i].replace(unindent, '') : lines[i]);
        i++;
      }
      i++;
      out.push(codeBlock(buf.join('\n'), lang));
      continue;
    }

    if (/^\s*$/.test(line)) {
      flushPara();
      i++;
      continue;
    }

    let handled = false;
    for (const ext of extensions) {
      if (!ext.block) continue;
      flushPara();
      const before = out.length;
      const next = ext.block(lines, i, out, helpers);
      if (typeof next === 'number' && next > i) {
        i = next;
        handled = true;
        break;
      }
      out.length = before; // the hook produced nothing usable
    }
    if (handled) continue;

    // An ATX heading: up to three spaces of indent, 1–6 `#`, then the text, and an optional closing run of `#`
    // that authors type as decoration and that is not part of the heading (§4.3). `#foo` is not a heading, and
    // four spaces of indent stays out of reach on purpose — that is indented code, a deliberate non-feature.
    const h = /^ {0,3}(#{1,6})[ \t]+(.*)$/.exec(line);
    if (h) {
      flushPara();
      out.push(`<h${h[1].length}>${inline(h[2].replace(/[ \t]+#+[ \t]*$/, '').trim())}</h${h[1].length}>`);
      i++;
      continue;
    }

    if (isThematic(line)) {
      flushPara();
      out.push('<hr>');
      i++;
      continue;
    }

    if (/^ {0,3}>/.test(line)) {
      flushPara();
      const quote = renderQuote(lines, i);
      out.push(quote.html);
      i = quote.next > i ? quote.next : i + 1; // same rule as the list branch: never trust an un-advanced scanner
      continue;
    }

    if (line.includes('|') && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      flushPara();
      const header = splitRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes('|') && !/^\s*$/.test(lines[i])) {
        rows.push(splitRow(lines[i]));
        i++;
      }
      let html = '<table><thead><tr>' + header.map((c) => `<th>${inline(c)}</th>`).join('') + '</tr></thead><tbody>';
      for (const r of rows) html += '<tr>' + r.map((c) => `<td>${inline(c)}</td>`).join('') + '</tr>';
      // The div is a scrollbar waiting for a table too wide for the message column (see `.md-scroll` in
      // app.css): without a scroll container of its own, a wide table gets its columns cut to characters
      // instead of running sideways. It is emitted per table, so a table still arriving in a stream's tail
      // never leaves an unclosed div behind in the frozen prefix.
      out.push(`<div class="md-scroll">${html}</tbody></table></div>`);
      continue;
    }

    if (listItemLine(line)) {
      flushPara();
      const list = renderList(lines, i);
      out.push(list.html);
      // Never trust a scanner to have advanced: a block type that consumes nothing would hang the tab, and a
      // renderer that hangs is worse than one that renders the wrong thing.
      i = list.next > i ? list.next : i + 1;
      continue;
    }

    para.push(line.replace(/^[ \t]+/, '')); // §4.6: a paragraph line's leading whitespace is not its text
    i++;
  }
  flushPara();
  [docRefs, docDefLines] = savedRefs;
  return out.join('\n');
}

// render turns markdown source into an HTML string.
export function render(src) {
  return renderBlocks(String(src ?? '').replace(/\r\n?/g, '\n').split('\n'));
}

// lastSafeBoundary answers one question about an unfinished reply: how much of it can never change
// again? The return value is an index into `src` — the part before it is finished, the part from it
// onwards may still grow. A stream renderer freezes the prefix and repaints only the tail, which is what
// turns "redraw the whole reply on every fragment" (O(n²) in total) back into O(n).
//
// The rule is one line wide and it is the whole safety argument: **cut after a blank line, unless that blank
// line is inside a fenced block or inside a list or block quote that has not ended.** Every other construct here
// ends at a blank line — paragraphs flush, tables stop — so a tail that begins after one cannot be continuing
// anything before it, and `render(prefix) + render(tail)` is the same document as `render(src)`.
// Two constructs swallow blank lines, which is why the rule has two halves: a fence, tracked below because the
// blank lines inside it are code, and an open container, which asks the very walkers the renderer uses where it
// ends. `- a`, blank, `- b` is one loose list (§5.2) and `> a`, blank, `> b` is one quote holding two paragraphs
// (§5.1); cutting after the first item froze a `</ul>` — and after the first paragraph a `</blockquote>` — that
// the finished document does not have.
//
// Everything the renderer joins across lines — a paragraph, an unclosed `**bold`, an open code span, a
// table's next row — is therefore left in the tail, repainted until it closes and never frozen half
// written. It returns 0 when nothing may be frozen (no blank line yet, or a fence that has not closed, or a
// list that has not ended): the caller then repaints the whole reply, which is the honest fallback, not a
// wrong answer.
//
// Indices refer to `src` exactly as given, so unlike `render` this does NOT normalise CRLF — an index into
// a different string would be worse than useless. `\r` needs no special case either: it is whitespace, and
// every test below already allows trailing whitespace.
//
// The one non-obvious rule: a blank line only separates if something follows it. A stream fragment ends
// mid-reply, and `split('\n')` hands back a trailing `''` for text ending in a newline — that is not a
// blank line, it is the absence of a next line. Treating it as one freezes whatever is still being written:
// a pipe table whose next row arrives a moment later, or a paragraph that is about to continue, would be
// frozen short and then contradict the finished document. This is not hypothetical — it is the first thing
// the bench's boundary property caught.
export function lastSafeBoundary(src) {
  const text = String(src ?? '');
  let cut = 0;
  let fence = null; // the marker that opened the fence we are inside ('```' or '~~~'), or null
  const lines = text.split('\n');
  // Blank lines inside an open container — between two items of one list, or two paragraphs of one quote — are
  // not boundaries either, and the answer comes from the same walkers the renderer uses so the two can never
  // disagree about where a container ends.
  const inRun = containerRunMask(lines);
  let at = 0; // index in `text` of the line being read
  for (let n = 0; n < lines.length; n++) {
    const line = lines[n];
    // The same two helpers the renderer uses: a boundary recognising a different set of fences than the code it
    // is trying to split would take a cut inside a code block and freeze half of it there.
    const open = fenceOpen(line);
    if (fence) {
      if (fenceClose(line, fence)) fence = null; // its own kind closes it; anything else is code
    } else if (open) {
      fence = open.marker; // blank lines inside are code, so no boundary may be taken here
    } else if (/^\s*$/.test(line) && !inRun[n] && at + line.length + 1 < text.length) {
      cut = at + line.length + 1; // the blank line belongs to the prefix; the tail starts on the next line
    }
    at += line.length + 1;
  }
  return cut;
}

// renderTo replaces the contents of an element with rendered markdown.
export function renderTo(el, src) {
  el.innerHTML = render(src);
}

export const md = { register, render, renderTo, escape, lastSafeBoundary, extensions };

if (typeof window !== 'undefined') {
  window.bareloopMarkdown = md;
}

export default md;
