// The login gate's client half.
//
// The daemon answers everything behind one credential, the gate says so with a 401
// and its JSON, and this module is the only place the UI learns about it: api.js
// asks here on every 401 and retries the request if the answer comes back, so the
// dialog appears exactly when a request needs it — not on page load, not on a
// guessed expiry — and a daemon without auth never gets here at all. 401 is the
// probe, and that means a daemon with no gate never has to tell this page anything:
// there is no version handshake for something that never answers 401.
//
// There is no logout-on-expiry dance and no timer: the session is a cookie the
// server expires out from under the page, and the next 401 opens this dialog again.
import { $, el } from './util.js';
import { u } from './base.js';

// requireLogin opens the dialog — once. Concurrent 401s (a boot makes several) all
// wait on the same one; a second dialog behind the first is one more form to
// mistake for it.
let ask = null;

export function requireLogin() {
  if (ask) return ask;
  let settle;
  ask = new Promise((resolve) => {
    settle = resolve;
  });

  const dlg = el('dialog', 'modal');
  const form = el('form');
  form.appendChild(el('h3', '', 'log in'));

  const user = el('input');
  user.name = 'user';
  user.autocomplete = 'username';
  user.spellcheck = false;
  user.autocapitalize = 'off';
  const userLabel = el('label', '', 'user');
  userLabel.appendChild(user);

  const pass = el('input');
  pass.type = 'password';
  pass.name = 'pass';
  // The real credential-manager words, so the browser that already knows this
  // daemon can fill both fields without a guess.
  pass.autocomplete = 'current-password';
  const passLabel = el('label', '', 'password');
  passLabel.appendChild(pass);

  const err = el('p', 'hint');
  err.hidden = true;

  const row = el('menu');
  const cancel = el('button', 'ghost', 'cancel');
  cancel.type = 'button';
  const ok = el('button', 'primary', 'log in');
  ok.type = 'submit';
  row.append(cancel, ok);

  form.append(userLabel, passLabel, err, row);

  // Not method="dialog": this form closes only when the daemon agrees. Closing on
  // submit would take the dialog down with every wrong password, and the point of
  // this one is to be asked again.
  let busy = false;
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (busy) return; // every wrong pair charges the rate-limit window; the button must not double-charge a double-click
    busy = true;
    ok.disabled = true;
    err.hidden = true;
    let message = '';
    try {
      const res = await fetch(u('/api/login'), {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ user: user.value, pass: pass.value }),
      });
      if (res.ok) {
        dlg.close('ok');
        refreshWhoami(); // this page booted anonymous; the logout button's rules only exist once there is an identity to log out of
        return;
      }
      const data = await res.json().catch(() => ({}));
      // The gate's own 401 text is deliberately uniform ("authentication
      // required") because it answers strangers too; here there is a person who
      // typed something, and what they need is the plainer sentence. A 429 does
      // carry the useful text — the rate limiter's own words are the schedule.
      message = res.status === 401 ? 'wrong user or password' : data.error || `${res.status} ${res.statusText}`;
      if (res.status === 401) {
        pass.value = '';
        pass.focus();
      }
    } catch {
      message = 'the daemon did not answer';
    }
    err.textContent = message;
    err.style.color = 'var(--err)';
    err.hidden = false;
    ok.disabled = false;
    busy = false;
  });

  cancel.onclick = () => dlg.close(); // Escape closes the same way; both leave returnValue empty
  dlg.addEventListener('close', () => {
    settle(dlg.returnValue === 'ok');
    ask = null;
    dlg.remove(); // ui.js's modal does the same: one dialog per question, no corpses with listeners
  });
  dlg.appendChild(form);
  document.body.appendChild(dlg);
  dlg.showModal();
  user.focus();
  return ask;
}

// refreshWhoami shows or hides the top-bar logout button from what the daemon
// says about this origin. With no gate configured /api/whoami answers like any
// missing file, so the button never promises to log anybody out of an open door.
async function refreshWhoami() {
  const btn = $('#btn-logout');
  if (!btn) return;
  const res = await fetch(u('/api/whoami'), { headers: { accept: 'application/json' } }).catch(() => null);
  if (!res || !res.ok) return;
  const data = await res.json().catch(() => null);
  if (!data || !data.user) return;
  btn.classList.remove('hidden');
  btn.title = `signed in as ${data.user}`;
  btn.setAttribute('aria-label', `log out (signed in as ${data.user})`);
  btn.onclick = async () => {
    await fetch(u('/api/logout'), { method: 'POST' }).catch(() => {});
    // The page's data was read as the identity that just left. Reloading is the
    // honest way to stop showing it: everything comes back through the gate, and
    // the gate asks for a credential again.
    location.reload();
  };
}

// wireAuth asks the daemon who this is, once, at boot: whether a gate exists is
// a fact about its config, not about the session, and a page load is the moment
// that fact is worth knowing. A page that booted anonymous learns the answer a
// second time the moment login succeeds (see refreshWhoami above).
export function wireAuth() {
  return refreshWhoami();
}
