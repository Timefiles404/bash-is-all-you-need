// Loading chapter data. One fetch per file, no index beyond chapter.json's
// own level list, so adding a level is one line of JSON plus one file.

const cache = new Map();

async function getJSON(url) {
  if (cache.has(url)) return cache.get(url);
  const res = await fetch(url, { cache: 'no-cache' });
  if (!res.ok) throw new Error(`${res.status} ${url}`);
  const data = await res.json();
  cache.set(url, data);
  return data;
}

export async function loadChapter(id) {
  const chapter = await getJSON(`./content/${id}/chapter.json`);
  const levels = await Promise.all(
    chapter.levels.map((name) => getJSON(`./content/${id}/${name}.json`)),
  );
  // The rail needs the level list in order and by id; keeping both here means
  // no view has to know the file naming scheme.
  chapter.levelData = levels;
  chapter.byId = new Map(levels.map((l) => [l.id, l]));
  return chapter;
}
