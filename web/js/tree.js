// The branch map: the tree as the store sees it — every fork, every abandoned
// stretch, and the one line the session stands on — with the door that moves the
// path to any entry on it.
//
// It exists because a fork is invisible from the transcript: the page draws the
// path, and its siblings stay in the store with no way back. `/tree` answers the
// geometry for every entry (no content, tens of kilobytes), `/snippets` answers
// the opening words for exactly the rows on screen, and this tab draws a map of
// branches rather than a reprint of the transcript: nodes where something
// happens (a fork, a dead end, the tip) stand on their own rows, and every
// linear stretch between them folds to a `⋯ N` line.
//
// The map redraws from the server on every tip move — which rows are lit is its
// whole subject, and re-reading (events.js calls syncTreePanel, and only while
// this tab is the open one) is cheaper than a client-side rule for which flags a
// move touches.
import { el, fmtAgo } from './util.js';
import { state } from './state.js';
import { call, get } from './api.js';
import { panelMode, registerRenderer, showPanelTab } from './panel.js';

let view = null; // the last /tree answer, remembered per session; a tip move drops it
let observer = null; // the lazy snippet loader, replaced by each draw
let queued = []; // ids whose row slid into view since the last batch
let flushT = null;

export function wireTree() {
  registerRenderer('tree', renderTree);
}

// syncTreePanel is what a tip event costs the map when it is open: the flags come
// from the server, not from a guess about the move, so the redraw re-reads.
export function syncTreePanel() {
  if (panelMode() !== 'tree') return;
  view = null;
  showPanelTab('tree');
}

async function renderTree(body) {
  if (!state.id) {
    body.appendChild(el('div', 'dim', 'open a session first'));
    return;
  }
  if (!view || view.at !== state.id) {
    const data = await get('/tree').catch(() => null); // api.js has reported it
    if (!data) return;
    data.at = state.id;
    view = data;
  }
  draw(body);
}

// kinds are read as one letter in a dense row; compaction earns a glyph of its
// own because on the path it is the loudest landmark there is.
const KIND = { user: 'u', assistant: 'a', tool: 't', compaction: '≡' };

function draw(body) {
  if (observer) observer.disconnect();
  queued = [];
  const rows = view.rows || [];
  const byId = new Map(rows.map((r) => [r.id, r]));
  // Children keep seq order (the answer arrives in it), and a parent outside this
  // read — off the store, or behind a truncating limit's window — makes the row a
  // root of its own stretch. The tree may lie about edges; the map draws what it is told.
  const kids = new Map();
  for (const r of rows) {
    if (!byId.has(r.parent)) continue;
    const list = kids.get(r.parent);
    if (list) list.push(r.id);
    else kids.set(r.parent, [r.id]);
  }
  // Interesting rows carry their own line: a fork, a leaf (a dead end you can
  // still walk back to), the tip, a root. Everything between two of them is a
  // stretch of chain and folds.
  const interesting = (r) => !r || r.nkids !== 1 || r.isTip || !r.parent;

  const head = el('div', 'tree-head');
  const forks = rows.filter((r) => r.nkids > 1).length;
  const off = rows.filter((r) => !r.onPath).length;
  head.appendChild(
    el('div', '', `${rows.length} entries · ${forks} fork point${forks === 1 ? '' : 's'}${off ? ` · ${off} off the path` : ''}`),
  );
  head.appendChild(el('div', 'dim', 'click any row to move the path there — the transcript follows, nothing is deleted'));
  if (view.truncated) {
    head.appendChild(el('div', 'tree-trunc', `the tree is deeper than one read carries (${view.limit} rows): the oldest stretch is missing from this map`));
  }
  body.appendChild(head);

  const rootish = rows.filter((r) => !byId.has(r.parent));
  for (const r of rootish) walk(r, 0, 0);

  if (!rows.length) body.appendChild(el('div', 'dim', 'this session has no entries yet'));
  // The map opens where the session stands, not at the top of a thousand rows.
  body.raw?.querySelector?.('.tree-row.is-tip')?.scrollIntoView({ block: 'center' });

  function walk(r, depth, gap) {
    if (gap) {
      const line = el('div', 'tree-gap', `⋯ ${gap} more ${gap === 1 ? 'entry' : 'entries'}`);
      line.style.paddingLeft = `${depth * 14}px`;
      line.title = 'a straight stretch of this path, folded — nothing branches inside it';
      body.appendChild(line);
    }
    body.appendChild(nodeRow(r, depth));
    const ks = kids.get(r.id) || [];
    if (ks.length === 1) {
      // The one child continues the line at the same depth; fold everything up to
      // the next interesting row.
      let cur = byId.get(ks[0]);
      let count = 1;
      while (!interesting(cur)) {
        const nk = kids.get(cur.id);
        if (!nk || nk.length !== 1) break;
        cur = byId.get(nk[0]);
        count++;
      }
      walk(cur, depth, count);
    } else {
      for (const k of ks) walk(byId.get(k), depth + 1, 0);
    }
  }

  function nodeRow(r, depth) {
    const row = el('div', `tree-row ${r.isTip ? 'is-tip' : r.onPath ? 'on-path' : 'off-path'}`);
    row.style.paddingLeft = `${depth * 14}px`;
    const mark = r.isTip ? '●' : r.nkids > 1 ? '⑂' : r.onPath ? '·' : '┈';
    const m = el('span', 'tree-mark', mark);
    if (r.nkids > 1) m.title = `${r.nkids} children — the conversation forked here`;
    if (r.isTip) m.title = 'the tip — where the next message continues from';
    row.appendChild(m);
    row.appendChild(el('span', 'tree-kind', KIND[r.kind] || '?'));
    row.appendChild(el('span', 'tree-id', r.id));
    const snip = el('span', 'tree-snip', '…');
    snip.dataset.id = r.id;
    row.appendChild(snip);
    const ago = el('span', 'tree-ago', fmtAgo(new Date(r.ts * 1000).toISOString()));
    row.appendChild(ago);
    if (!r.isTip) {
      const go = el('button', 'tree-go', 'set as origin');
      go.title = `move the path here (${r.id}): the next message continues from this entry — every entry stays either way`;
      go.onclick = (e) => {
        e.stopPropagation();
        setTip(r);
      };
      row.appendChild(go);
    }
    row.onclick = () => setTip(r); // the whole row, because a phone has no hover to reveal a button under
    observe(snip);
    return row;
  }

  // Snippets ride on view, not on need-per-draw: every row the map renders asks
  // for its opening line once its row actually slides on screen, batched, so a
  // thousand-row map costs one screenful of point queries.
  function observe(snipEl) {
    if (!observer) {
      observer = new IntersectionObserver(
        (entries) => {
          for (const e of entries) {
            if (!e.isIntersecting) continue;
            observer.unobserve(e.target);
            ask(e.target.dataset.id);
          }
        },
        { root: document.getElementById('panel-body') },
      );
    }
    observer.observe(snipEl);
  }

  function ask(id) {
    queued.push(id);
    // The catch is the design, not hygiene: a batch landing after the reader switched
    // tabs finds its body proxy expired ("panel moved on"), and the correct repair for
    // words nobody is reading is to drop them.
    if (!flushT) flushT = setTimeout(() => flushSnips().catch(() => {}), 120);
  }

  async function flushSnips() {
    flushT = null;
    const ids = queued.splice(0, queued.length);
    if (!ids.length) return;
    const data = await get(`/snippets?ids=${ids.join(',')}`).catch(() => null);
    if (!data) return;
    const root = body.raw || body;
    for (const s of data.snippets) {
      for (const node of root.querySelectorAll(`.tree-snip[data-id="${s.id}"]`)) {
        node.textContent = s.snippet || '⟨no text⟩';
        node.title = s.snippet || '';
      }
    }
  }
}

// setTip moves the path. Nothing is redrawn from here on success: the tip event
// comes back through events.js, which refreshes the transcript and calls
// syncTreePanel — one path for a moved tip, whoever moved it.
async function setTip(r) {
  if (r.isTip) return;
  await call('/tip', { id: r.id }).catch(() => {}); // call() reports a failure itself
}
