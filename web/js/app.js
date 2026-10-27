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
//   panel.js     the inspect panel and the payload dump
//   transport.js websocket, SSE and polling fallbacks
//   md.js        the markdown subset
//
// This file wires the pieces, handles the global keys, and starts the transport.
import { $ } from './util.js';
import { state } from './state.js';
import { EV_REFRESH, EV_SESSIONS, on } from './bus.js';
import { fail, note, promptBox } from './ui.js';
import { isPinned, scrollDown, wireScroll } from './scroll.js';
import { closeNav, navIsOpen, wireNav } from './nav.js';
import { api } from './api.js';
import { wireComposer } from './composer.js';
import { wireSettings } from './settings.js';
import { hidePanel, wirePanel } from './panel.js';
import { loadSessions, selectSession, refresh } from './session.js';
import { onCompaction } from './events.js';
import { connect } from './transport.js';

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

function wireNewSession() {
  $('#new-session').onclick = () => $('#dlg-new').showModal();
  $('#new-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = e.target;
    const body = { id: f.elements.namedItem('id').value.trim(), name: f.elements.namedItem('name').value.trim() };
    const data = await api('/api/sessions', { body }).catch((err) => {
      fail(err);
      return null;
    });
    if (!data) return; // a rejected id leaves the dialog open so it can be retyped
    f.reset();
    $('#dlg-new').close();
    if (data.id) await selectSession(data.id);
  });
}

// Escape works from the top down: a modal dialog closes itself (the browser does that), then the
// phone drawer, then "stop reading history, take me to the newest message".
function wireKeys() {
  window.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (document.querySelector('dialog[open]')) return;
    if (navIsOpen()) {
      closeNav();
      return;
    }
    if (!isPinned()) scrollDown(true);
  });
}

function wire() {
  wireScroll();
  wireNav();
  wireComposer();
  wireSettings();
  wirePanel();
  wireNewSession();
  wireKeys();
  $('#btn-compact').onclick = compactNow;
  on(EV_REFRESH, () => refresh().catch(() => {}));
  on(EV_SESSIONS, () => loadSessions().catch(() => {}));
  window.addEventListener('beforeunload', () => state.es?.close());
}

async function main() {
  wire();
  await loadSessions();
  const initial = state.sessions.find((s) => !s.closed)?.id;
  if (initial) await selectSession(initial);
  else note('no session yet — create one');
  connect();
}

main();
