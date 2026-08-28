// Keeping a learner's files across a reload.
//
// IndexedDB rather than localStorage, for two reasons that both matter here.
// localStorage stores strings, so a file's bytes would have to become base64 —
// a third larger, and lossy for anything that is not text unless it is encoded
// carefully. And localStorage is synchronous: writing a tree of files blocks
// the thread it runs on, which in this design is the worker that is also
// running the shell.
//
// The whole tree goes in as one record. Per-file records would be tidier and
// would let a single file be written without rewriting the rest — but a level's
// tree is tens of small files, one structured-clone of the lot is well under a
// millisecond, and a partial write is the failure mode where a learner's
// filesystem comes back internally inconsistent. One record is atomic.
//
// What this does not do is sync anywhere. The data is in this browser, on this
// device, for this origin. Clearing site data clears it. That is stated in the
// UI rather than hidden, because a learner who spends an hour on a chapter
// deserves to know where the hour is stored.

const DB_NAME = 'bash-is-all-you-need';
const DB_VERSION = 1;
const STORE = 'filesystems';

export class SnapshotStore {
  constructor() {
    this._db = null;
  }

  async _open() {
    if (this._db) return this._db;
    this._db = await new Promise((resolve, reject) => {
      let req;
      try {
        req = indexedDB.open(DB_NAME, DB_VERSION);
      } catch (err) {
        // Some browsers throw here rather than firing onerror — a private
        // window with storage blocked, most often.
        reject(err);
        return;
      }
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE);
      };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error || new Error('indexedDB.open failed'));
      req.onblocked = () => reject(new Error('another tab is holding an older version of the database'));
    });
    return this._db;
  }

  _key(level) {
    // One tree per level. A learner moving between chapters should not find
    // chapter 7's files in chapter 2's terminal.
    return `fs:${level || 'default'}`;
  }

  /**
   * @param {?string} level
   * @param {object} snapshot from MemFS.snapshot()
   */
  async save(level, snapshot) {
    const db = await this._open();
    await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE, 'readwrite');
      tx.objectStore(STORE).put({ savedAt: Date.now(), snapshot }, this._key(level));
      tx.oncomplete = resolve;
      tx.onerror = () => reject(tx.error || new Error('write failed'));
      tx.onabort = () => reject(tx.error || new Error('write aborted — the storage quota is probably full'));
    });
  }

  /** @returns {Promise<?object>} the snapshot, or null when there is none */
  async load(level) {
    const db = await this._open();
    return new Promise((resolve, reject) => {
      const tx = db.transaction(STORE, 'readonly');
      const req = tx.objectStore(STORE).get(this._key(level));
      req.onsuccess = () => resolve(req.result?.snapshot || null);
      req.onerror = () => reject(req.error || new Error('read failed'));
    });
  }

  async clear(level) {
    const db = await this._open();
    await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE, 'readwrite');
      if (level === undefined) tx.objectStore(STORE).clear();
      else tx.objectStore(STORE).delete(this._key(level));
      tx.oncomplete = resolve;
      tx.onerror = () => reject(tx.error);
    });
  }

  /** Rough bytes used, where the browser will say. Shown in the settings panel. */
  async usage() {
    if (!navigator.storage?.estimate) return null;
    const est = await navigator.storage.estimate();
    return { usage: est.usage, quota: est.quota };
  }
}
