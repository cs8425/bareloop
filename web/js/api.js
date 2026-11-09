// Every call to bareloopd. Actions are plain REST rather than websocket frames so a click is as
// close to instant as the network allows, and so curl can do everything the page can.
import { state } from './state.js';
import { fail } from './ui.js';
import { u } from './base.js';
import { requireLogin } from './auth.js';

// api makes one request and always resolves to an object: a server that answers with something that
// is not JSON (an HTML error page from a proxy, say) comes back as {raw: "..."} instead of throwing
// inside JSON.parse, so the caller can still show what happened. Callers write the daemon's own path
// (`/api/session/…`); base.js puts whatever prefix this copy of the UI is hanging under in front of it.
export async function api(path, opts = {}) {
  const res = await fetch(u(path), {
    headers: { 'content-type': 'application/json' },
    method: opts.method || (opts.body ? 'POST' : 'GET'),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { raw: text };
    }
  }
  if (!res.ok) {
    // The login gate's 401 is treated as recoverable: the dialog opens (one dialog,
    // however many requests answer 401 at once), and when it comes back granted the
    // same request runs again as if nothing interrupted it — callers never need to
    // know a session lapsed in the middle of a fetch. A refused or cancelled dialog
    // falls through to the ordinary error, which says exactly what the daemon said.
    if (res.status === 401 && (await requireLogin())) return api(path, opts);
    throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
  }
  return data;
}

// sessionPath prefixes a session-scoped path with the session currently on screen.
const sessionPath = (path) => `/api/session/${state.id}${path}`;

// call posts an action against the current session. Failures are reported (toast + transcript) and
// rethrown, so a caller that cares can stop and one that does not still leaves the user informed.
export async function call(path, body) {
  try {
    return await api(sessionPath(path), { body: body || {} });
  } catch (err) {
    throw new Error(fail(err));
  }
}

// get reads one session-scoped resource.
export async function get(path) {
  try {
    return await api(sessionPath(path));
  } catch (err) {
    throw new Error(fail(err));
  }
}
