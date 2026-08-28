/* Registering the offline cache, and the three cases where we do not.
 *
 * The service worker exists so a chapter's WebAssembly is downloaded once
 * rather than once per reload. It is an optimisation, so every failure here is
 * silent and the site carries on without it — a cache that can break the page
 * is worse than no cache.
 */

const OFF = new URLSearchParams(location.search).has('nosw');

/**
 * Whether a service worker can be registered at all.
 *
 * Browsers only allow one on a secure origin, and treat localhost as secure so
 * that development works. `file://` is neither, which is the case a reader
 * double-clicking index.html lands in — and the page already tells them to
 * serve the directory instead.
 */
function possible() {
  return 'serviceWorker' in navigator && window.isSecureContext && location.protocol !== 'file:';
}

export function register() {
  if (OFF || !possible()) return;
  // After load rather than during it. Registration competes for the same
  // connections as the page's own scripts, and the first visit is the one where
  // the cache is empty and cannot help anyway.
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('./sw.js', { scope: './' }).catch(() => {});
  });
}

/**
 * Throw away everything cached, for a reader who suspects they are looking at a
 * stale build. Returns false when there is nothing to talk to.
 *
 * Worth having as a command rather than an instruction to clear site data:
 * "clear your browser cache" is advice that costs the reader their progress,
 * which lives in localStorage and is nothing to do with this.
 */
export async function dropCaches() {
  if (!('serviceWorker' in navigator)) return false;
  const reg = await navigator.serviceWorker.getRegistration();
  if (!reg || !reg.active) return false;
  reg.active.postMessage('biayn:drop-caches');
  return true;
}
