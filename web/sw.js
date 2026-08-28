/* The offline cache.
 *
 * The site is static files, so this could have been left to HTTP caching. It is
 * not, for one reason: a chapter's WebAssembly is about 3.3 MB and there are
 * thirteen of them. Downloading that again because somebody reloaded the page is
 * the difference between a site you can work in on a train and a site you can
 * work in at a desk with good wifi.
 *
 * Three strategies, and which one a request gets is decided entirely by whether
 * its content can change under a fixed URL.
 *
 *   immutable   .wasm, and anything under assets/levels/ — cache first, never
 *               revalidated. These are build outputs. When they change, they
 *               change together with a new BUILD below, and the whole old cache
 *               is deleted rather than individually invalidated.
 *
 *   document    the HTML — network first, cache as the fallback. This is the
 *               one file that must not get stuck: a page served from a stale
 *               cache can reference scripts that no longer exist, and the site
 *               is then broken until somebody knows to clear site data. Being
 *               offline is a worse failure than being one version behind, so
 *               the cache still answers when the network does not.
 *
 *   revalidate  everything else — the cache answers immediately and the network
 *               refreshes it in the background. A reader gets the last version
 *               instantly and the next version next time.
 *
 * Registration is deliberately conditional and failure is deliberately silent;
 * see register() in assets/js/sw-register.js. A service worker is an
 * optimisation, and an optimisation that can break the site is not one.
 */

// BUILD is the cache generation. Bumping it throws away everything cached under
// the old name on the next activation, which is the only invalidation this file
// has and the only one it needs. build.py rewrites this line.
const BUILD = 'dev';

const CACHE = `biayn-${BUILD}`;

// The shell of the site: enough to open, show the introduction and render a
// level's reading material with no network at all. Deliberately not the wasm —
// precaching 3 MB somebody may never need is how a site earns a reputation for
// being slow to open.
const PRECACHE = [
  './',
  './index.html',
  './assets/css/tokens.css',
  './assets/css/layout.css',
  './assets/css/components.css',
  './assets/css/workbench.css',
  './assets/css/overlays.css',
  './assets/favicon.svg',
];

self.addEventListener('install', (e) => {
  // waitUntil, then skipWaiting: the new worker takes over on the next
  // navigation rather than sitting behind the old one until every tab closes.
  // Safe here because the caches are generation-named, so the old worker's
  // entries are never read by the new one.
  e.waitUntil(
    caches
      .open(CACHE)
      // addAll is atomic — one 404 and nothing is cached. That is the wrong
      // trade for a list this optional, so each is added on its own and a
      // failure costs one entry rather than the whole install.
      .then((c) => Promise.all(PRECACHE.map((u) => c.add(u).catch(() => {}))))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((names) => Promise.all(names.filter((n) => n !== CACHE).map((n) => caches.delete(n))))
      .then(() => self.clients.claim()),
  );
});

/** Build outputs, whose bytes never change without BUILD changing too. */
function isImmutable(url) {
  return url.pathname.endsWith('.wasm') || url.pathname.includes('/assets/levels/');
}

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  // Same origin only. A cross-origin request is a CDN's business, it has its own
  // caching headers, and quietly re-serving somebody else's bytes from our cache
  // is a surprise nobody asked for.
  if (url.origin !== self.location.origin) return;

  if (req.mode === 'navigate' || req.destination === 'document') {
    e.respondWith(networkFirst(req));
    return;
  }
  if (isImmutable(url)) {
    e.respondWith(cacheFirst(req));
    return;
  }
  e.respondWith(revalidate(req));
});

async function cacheFirst(req) {
  const hit = await caches.match(req);
  if (hit) return hit;
  const res = await fetch(req);
  // Only a real 200 is worth keeping. An opaque or partial response cached here
  // would be served forever as though it were the file.
  if (res.ok && res.status === 200) {
    const c = await caches.open(CACHE);
    c.put(req, res.clone());
  }
  return res;
}

async function networkFirst(req) {
  try {
    const res = await fetch(req);
    if (res.ok) {
      const c = await caches.open(CACHE);
      c.put(req, res.clone());
    }
    return res;
  } catch (err) {
    const hit = (await caches.match(req)) || (await caches.match('./index.html'));
    if (hit) return hit;
    throw err;
  }
}

async function revalidate(req) {
  const c = await caches.open(CACHE);
  const hit = await c.match(req);
  const net = fetch(req)
    .then((res) => {
      if (res.ok && res.status === 200) c.put(req, res.clone());
      return res;
    })
    // Offline with nothing cached is a real failure and has to propagate; the
    // caller sees it as a failed fetch, which is what it is.
    .catch((err) => {
      if (hit) return hit;
      throw err;
    });
  return hit || net;
}

// One message, so the page can offer to clear everything without the reader
// having to find site data in browser settings.
self.addEventListener('message', (e) => {
  if (e.data === 'biayn:drop-caches') {
    e.waitUntil(caches.keys().then((ns) => Promise.all(ns.map((n) => caches.delete(n)))));
  }
});
