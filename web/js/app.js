// bareloop web UI — entry point.
//
// The UI is plain ES modules with no build step, so `web/` can be edited while the server runs and
// served straight from disk (or embedded into the binary later, unchanged). Each module owns one
// thing and imports downward:
//
//   util.js      selectors, elements, number and id formatting
//   state.js     the one mutable state object
//   bus.js       leaf modules asking higher modules for work (a refresh) without importing them
//   scroll.js    following the transcript, and refusing to when the user scrolled away
//   nav.js       the session drawer on phones
//   ui.js        notes, toasts, and modal confirm/prompt — never window.alert, see ui.js
//   api.js       REST calls to bareloopd
//   transcript.js drawing entries, live streams, tool cards and input requests
//   session.js   reading a session, the sidebar, the status badges
//   events.js    agent events → DOM
//   composer.js  the textarea, attachments, send / interrupt / stop
//   settings.js  the settings dialog and the config PATCHes
//   tools.js     the tool catalogue: what each tool is for, what it costs, its parameters, its switch
//   panel.js     the side panel the inspect view and the catalogue are drawn into
//   transport.js websocket, SSE and polling fallbacks
//   md.js        the markdown subset
//
// This file wires the pieces, handles the global keys, and starts the transport.
import { $ } from './util.js';
import { isParked, state } from './state.js';
import { EV_REFRESH, EV_SESSIONS, on } from './bus.js';
import { fail, note, promptBox } from './ui.js';
import { isPinned, scrollDown, wireScroll } from './scroll.js';
import { bootNav, closeNav, isCollapsed, isMobile, navIsOpen, setNav } from './nav.js';
import { api } from './api.js';
import { wireComposer } from './composer.js';
import { wireSettings } from './settings.js';
import { hidePanel, wirePanel } from './panel.js';
import { wireTools } from './tools.js';
import { wireStats } from './stats.js';
import { flushStreams } from './transcript.js';
import { loadSessions, loadSoon, returnToTip, selectSession, refresh, idFromHash, writeHash, wirePaging, wireSessions } from './session.js';
import { onCompaction } from './events.js';
import { connect } from './transport.js';
import { bootIcons } from './icons.js';

// compactNow asks for an extra instruction first: "summarise this" and "summarise this, keep every
// file name" produce very different summaries, and a compaction is only cheap to redo before it is
// committed.
async function compactNow() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const answer = await promptBox('Extra instruction for the summary (optional).', {
    title: 'compact now',
    placeholder: 'e.g. keep every file name and every command verbatim',
    ok: 'compact',
  });
  if (answer === null) return;
  note('compacting…');
  const res = await api(`/api/session/${state.id}/compact`, { body: { instruction: answer } }).catch(fail);
  if (res) onCompaction(res);
}

// Session ids are directory names, and nobody has one ready when a dialog asks for it, so one is
// suggested here rather than by the server: an id the server invents is a name nobody asked for, and
// the UI can show, change and keep a suggestion. Two readable words plus a short suffix read well in
// `ls data/` and cannot contain anything a filesystem would object to.
const ID_ADJECTIVES = ['amber', 'brisk', 'cosmic', 'crisp', 'gentle', 'humble', 'jolly', 'lunar', 'mellow', 'nimble', 'polite', 'quiet', 'quaint', 'solar', 'spry', 'tidy', 'vivid', 'wry', 'zesty', 'zippy'];
const ID_NOUNS = ['otter', 'cactus', 'beacon', 'kettle', 'maple', 'nebula', 'pixel', 'quill', 'raft', 'saddle', 'thimble', 'violin', 'warbler', 'zephyr', 'anchor', 'cinder', 'dollop', 'ember', 'fathom', 'grotto', 'harbor', 'island', 'lantern', 'orbit'];

function randomBelow(n) {
  const c = window.crypto;
  if (c && c.getRandomValues) {
    const buf = new Uint32Array(1);
    c.getRandomValues(buf);
    return buf[0] % n;
  }
  return Math.floor(Math.random() * n);
}

function newSessionId() {
  const pick = (list) => list[randomBelow(list.length)];
  const suffix = randomBelow(46656).toString(36).padStart(3, '0');
  return `${pick(ID_ADJECTIVES)}-${pick(ID_NOUNS)}-${suffix}`;
}

function wireNewSession() {
  const dlg = $('#dlg-new');
  const form = $('#new-form');
  const idField = form.elements.namedItem('id');
  // What the app made up, so a collision can be resolved without overwriting a hand-typed id.
  let suggested = '';
  const suggest = () => {
    suggested = newSessionId();
    idField.value = suggested;
  };
  // Ids are lowercase (see index.html): folding the text while typing beats rejecting it after the
  // fact, and lowercasing never changes the length, so the caret does not move.
  idField.addEventListener('input', () => {
    const lower = idField.value.toLowerCase();
    if (idField.value !== lower) idField.value = lower;
  });
  $('#new-id-again').onclick = suggest;
  $('#new-session').onclick = () => {
    if (!idField.value.trim()) suggest();
    dlg.showModal();
  };
  // data-close buttons are the only way out besides create. A button that both submitted and closed
  // the dialog used to create a session when the user meant to back out of one.
  dlg.querySelectorAll('[data-close]').forEach((b) => { b.onclick = () => dlg.close(b.dataset.close || 'cancel'); });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    // Enter in the id field also submits the form; only the create button means it.
    if (e.submitter && e.submitter.value !== 'create') return;
    const name = form.elements.namedItem('name').value.trim();
    let id = idField.value.trim().toLowerCase();
    // An empty id is an explicit "I don't care": make one up and say so, rather than sending nothing
    // or letting the server guess behind the scenes.
    if (!id) {
      id = newSessionId();
      suggested = id;
      idField.value = id;
      note(`suggested id: ${id}`);
    } else {
      suggested = '';
    }
    const send = (theId) => api('/api/sessions', { body: { id: theId, name } });
    let data = await send(id).catch((err) => ({ err }));
    if (data && data.err) {
      // Two browsers can be handed the same suggestion; only the server knows what it has already.
      // A hand-typed id is not ours to change, so it keeps its error and stays open for a fix.
      if (id === suggested) {
        const again = newSessionId();
        const retry = await send(again).catch(() => null);
        if (retry) {
          id = again;
          data = retry;
          note(`that id was taken - used ${again}`);
        }
      }
      if (!data.id) { fail(data.err || new Error('create returned no id')); return; }
    }
    form.reset();
    suggested = '';
    dlg.close('create');
    if (data.id) await selectSession(data.id);
  });
}

// Escape works from the top down: a modal dialog closes itself (the browser does that), then the
// phone drawer, then "stop reading history, take me to the newest message".
function wireKeys() {
  window.addEventListener('keydown', (e) => {
    if (e.key === '/') {
      focusFilter(e);
      return;
    }
    if (e.key !== 'Escape') return;
    if (document.querySelector('dialog[open]')) return;
    if (navIsOpen()) {
      closeNav();
      return;
    }
    // The side panel is a drawer, and drawers answer to Escape: closing it comes before "stop reading
    // history, take me to the newest message".
    if (!$('#panel').classList.contains('hidden')) {
      hidePanel();
      return;
    }
    // Parked, that promise is a page jump rather than a scroll: the newest message is not in this page at
    // all, and scrolling would only walk to the bottom of the page being read and call it "latest".
    if (isParked()) returnToTip();
    else if (!isPinned()) scrollDown(true);
  });
}

// `/` is the list asking to be searched: it puts the caret in the filter without making anybody hunt for a
// small input, and it works from anywhere on the page. An open dialog or a field that types text keeps its
// `/` — a path, a URL and a CJK candidate list are all more literal than a shortcut.
function focusFilter(e) {
  if (e.isComposing || e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.target.closest?.('dialog,input,textarea,select,[contenteditable]')) return;
  const box = $('#session-filter');
  if (!box) return;
  e.preventDefault();
  // "On screen" is a different question in the two shapes: a column that is not collapsed, or a drawer that
  // is open. Focus goes to a visible input only — focusing the hidden one reads as a lost keystroke.
  // A collapsed desktop gets the panel instead, and opening the panel focuses the filter itself (nav.js).
  if (isMobile() ? navIsOpen() : !isCollapsed()) {
    box.focus();
    box.select();
  } else setNav(true);
}

function wire() {
  // Icons first: the markup carries `data-icon` placeholders, and every button is measured and styled
  // around the glyph that replaces them.
  bootIcons();
  wireScroll();
  // The paging chips are wired after the scroll wiring because they hang a second listener on the same
  // scroller: one owns following the tip, these own reading backwards and forwards.
  wirePaging();
  // bootNav before anything that paints the sidebar: it is the one class that says whether 210px of this
  // window belongs to the session list, and applying it after the first paint is a layout jump on every
  // reload of a collapsed window.
  bootNav();
  wireSessions();
  wireComposer();
  wireSettings();
  wirePanel();
  wireTools();
  wireStats();
  wireNewSession();
  wireKeys();
  // The render gate draws nothing while this tab is hidden — a background tab has no reader — so coming
  // back needs one explicit repaint: the fragments that arrived meanwhile are in the stream buffers but
  // not yet on screen. There is no timer in the gate to do it for us, deliberately.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) flushStreams();
  });
  $('#btn-compact').onclick = compactNow;
  on(EV_REFRESH, () => refresh().catch(() => {}));
  on(EV_SESSIONS, () => loadSoon());
  // A hash change is somebody asking to look at another session: the back button, the forward button, or an
  // edited address bar. An id the list does not know is answered by reading the list once more - the session
  // may be brand new or just deleted - and then by putting the session actually on screen back into the URL.
  window.addEventListener('hashchange', async () => {
    const id = idFromHash();
    if (!id) {
      writeHash(state.id, true); // the hash was wiped by hand: make the URL describe the view again
      return;
    }
    if (id === state.id) return; // selectSession wrote this hash itself
    if (!state.sessions.some((s) => s.id === id)) {
      await loadSessions().catch(() => {});
      if (!state.sessions.some((s) => s.id === id)) {
        note(`no session ${id}`);
        writeHash(state.id, true);
        return;
      }
    }
    selectSession(id).catch(() => {});
  });
  window.addEventListener('beforeunload', () => state.es?.close());
}

async function main() {
  wire();
  await loadSessions();
  // The URL wins over "the first session": a reload, a bookmark or a link someone pasted has to land on the
  // conversation it names, closed or not - a deep link is deliberate, while the default only has to guess.
  // A hash that names nothing is corrected by the selection below, so the address bar always ends up
  // describing what is on the screen.
  const linked = idFromHash();
  // "The first session that is worth opening" is a live one: not shut, and not archived either — putting a
  // conversation the reader took out of the list back on screen on every reload is the fold disagreeing with
  // the ask. Only when nothing else is available does an archived session get opened, because the alternative
  // is telling a reader who has eleven sessions that there is nothing yet.
  const initial =
    state.sessions.find((s) => s.id === linked)?.id ||
    state.sessions.find((s) => !s.closed && !s.archived)?.id ||
    state.sessions.find((s) => !s.closed)?.id;
  if (initial) await selectSession(initial);
  else {
    note('no session yet — create one');
    writeHash('');
  }
  connect();
}

main();
