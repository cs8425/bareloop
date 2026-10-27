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

function inline(text) {
  let out = escape(text);
  // Code spans first, so their contents are not touched by the other rules.
  const spans = [];
  out = out.replace(/`([^`]+)`/g, (_, code) => {
    spans.push(code);
    return `\u0000${spans.length - 1}\u0000`;
  });
  out = out.replace(/\*\*\*([^*]+)\*\*\*/g, '<strong><em>$1</em></strong>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|[^\w*])\*([^*\n]+)\*(?![\w*])/g, '$1<em>$2</em>')
    .replace(/(^|[^\w_])__([^_\n]+)__(?![\w_])/g, '$1<strong>$2</strong>')
    .replace(/(^|[^\w_])_([^_\n]+)_(?![\w_])/g, '$1<em>$2</em>')
    .replace(/~~([^~]+)~~/g, '<del>$1</del>');
  // Links: [text](url), restricted to http(s), mailto and relative targets.
  out = out.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, label, url) =>
    /^(https?:|mailto:|\/|#|\.)/i.test(url)
      ? `<a href="${url}" target="_blank" rel="noopener noreferrer">${label}</a>`
      : m,
  );
  out = out.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${escape(spans[Number(i)])}</code>`);
  for (const ext of extensions) {
    if (ext.inline) {
      const next = ext.inline(out);
      if (typeof next === 'string') out = next;
    }
  }
  return out;
}

const isTableSep = (line) => /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(line) && line.includes('-');
const splitRow = (line) => line.replace(/^\s*\|/, '').replace(/\|\s*$/, '').split('|').map((c) => c.trim());

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

// render turns markdown source into an HTML string.
export function render(src) {
  const lines = String(src ?? '').replace(/\r\n?/g, '\n').split('\n');
  const out = [];
  const helpers = { inline, escape };
  let i = 0;
  let para = [];
  const flushPara = () => {
    if (para.length) {
      out.push(`<p>${inline(para.join('\n')).replace(/\n/g, '<br>')}</p>`);
      para = [];
    }
  };

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code (``` or ~~~).
    const fence = line.match(/^\s*(```|~~~)\s*([\w+-]*)\s*$/);
    if (fence) {
      flushPara();
      const marker = fence[1];
      const lang = fence[2];
      const buf = [];
      i++;
      while (i < lines.length && !new RegExp('^\\s*' + marker).test(lines[i])) {
        buf.push(lines[i]);
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

    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      flushPara();
      out.push(`<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`);
      i++;
      continue;
    }

    if (/^\s*([-*_])\s*(\1\s*){2,}$/.test(line)) {
      flushPara();
      out.push('<hr>');
      i++;
      continue;
    }

    if (/^\s*>/.test(line)) {
      flushPara();
      const buf = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) {
        buf.push(lines[i].replace(/^\s*>\s?/, ''));
        i++;
      }
      out.push(`<blockquote>${inline(buf.join(' '))}</blockquote>`);
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
      out.push(html + '</tbody></table>');
      continue;
    }

    const li = line.match(/^(\s*)([-*+]|\d+[.)])\s+(.*)$/);
    if (li) {
      flushPara();
      const ordered = /\d/.test(li[2]);
      const items = [];
      while (i < lines.length) {
        const m = lines[i].match(/^(\s*)([-*+]|\d+[.)])\s+(.*)$/);
        if (!m) {
          if (/^\s+\S/.test(lines[i]) && items.length) {
            items[items.length - 1].text += ' ' + lines[i].trim();
            i++;
            continue;
          }
          break;
        }
        if (/\d/.test(m[2]) !== ordered) break;
        items.push({ indent: m[1].length, text: m[3] });
        i++;
      }
      const tag = ordered ? 'ol' : 'ul';
      let html = `<${tag}>`;
      let nested = false;
      for (const item of items) {
        if (item.indent >= 2 && !nested) { html += `<${tag}>`; nested = true; }
        else if (item.indent < 2 && nested) { html += `</${tag}>`; nested = false; }
        html += `<li>${inline(item.text)}</li>`;
      }
      if (nested) html += `</${tag}>`;
      out.push(html + `</${tag}>`);
      continue;
    }

    para.push(line);
    i++;
  }
  flushPara();
  return out.join('\n');
}

// renderTo replaces the contents of an element with rendered markdown.
export function renderTo(el, src) {
  el.innerHTML = render(src);
}

export const md = { register, render, renderTo, escape, extensions };

if (typeof window !== 'undefined') {
  window.bareloopMarkdown = md;
}

export default md;
