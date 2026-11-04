// Where the daemon's own paths begin. The UI is a directory of files that some reverse proxy is free to hang
// under any prefix it likes — `/app/bareloopd/` on a shared host, `/` on a laptop — while the daemon itself
// answers at `/` in every one of those cases, because a proxy that mounts an app strips its prefix before
// forwarding. So the path the browser has to ask for is the prefix plus the daemon's path, and the prefix is
// read from where the *code* was loaded from: the module graph knows that exactly, and the address bar only
// sometimes says it (`#/s/x` is a hash, and a directory typed without its trailing slash moves the answer up
// one level). Nothing is configured, and there is no build step to configure it in.
//
// See `u` for the one place that answers "which of these paths belong to the daemon at all".
export const BASE = new URL('../', import.meta.url).pathname; // this file lives at <base>/js/base.js

// u turns a daemon path (`/api/session/x/send`, `/ws`) into the path this browser has to ask for.
//
// A URL carrying its own scheme or authority comes back untouched. That is not tidiness: the images in a
// transcript are `data:` URLs, and handing one of them to this function's prefix would trade a working picture
// for a broken one.
export function u(path) {
  const p = String(path);
  if (/^[a-z][a-z0-9+.-]*:/i.test(p) || p.startsWith('//')) return p;
  return BASE + p.replace(/^\/+/, '');
}
