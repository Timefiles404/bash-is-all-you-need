// Every user-facing string in the chrome goes through t(). Two tables, one
// file, so a missing translation is a diff away from being found.
//
// Chapter content is not here — it carries its own {zh, en} pairs, because a
// level's prose belongs with the level.

const zh = {
  'app.subtitle': '动手写一个编码 agent',
  'app.progress': '本章进度',

  'level.locked': '未解锁',
  'level.available': '可以开始',
  'level.cleared': '已通过',
  'level.skipped': '已揭示',
  'rail.quiz': '章末测验',
  'rail.closing': '读完之后',
  'rail.lockedHint': '先通过上一关',

  'btn.rail': '折叠章节栏',
  'btn.railExpand': '展开章节栏',
  'btn.format': '格式化',
  'btn.save': '保存',
  'btn.reveal': '揭示答案',
  'btn.help': '快捷键',
  'btn.reading': '打开阅读材料',
  'btn.readingHide': '收起阅读材料',
  'btn.run': '运行',
  'btn.stop': '停止',
  'btn.clear': '清屏',
  'btn.close': '关闭',
  'btn.diff': '看它是怎么搭起来的',
  'tab.run': '运行',
  'tab.shell': 'shell',
  'files.title': '文件',
  'files.readonly': '只读',

  'st.checkIdle': '未检查',
  'st.checking': '检查中',
  'st.checkOk': '没有问题',
  'st.checkErr': '{n} 处问题',
  'st.saved': '已保存',
  'st.dirty': '未保存',
  'st.holes': '还有 {n} 处待选',
  'st.holesDone': '空位已填满',
  'st.holesWrong': '{n} 处选错了',
  'st.objective': '目标',

  'rt.compiler': '编译',
  'rt.shell': 'shell',
  'rt.llm': '模型',
  'rt.idle': '待命',
  'rt.loading': '加载中',
  'rt.ready': '就绪',
  'rt.unavailable': '不可用',
  'rt.booting': '运行时启动中',

  'read.title': '阅读',
  'read.none': '这一关还没有配阅读材料。',
  'read.jump.hole': '跳到代码里的这处空位',
  'read.jump.line': '跳到代码里的这一行',
  'read.jump.file': '打开这个文件',

  'chooser.title': '这里该写什么',
  'chooser.right': '这个是对的',
  'chooser.wrong': '这个不对',
  'chooser.foot': '↑ ↓ 选择 · Enter 确认 · Esc 关闭',
  'chooser.filled': '已选。点一下可以换。',

  'clear.title': '这一关过了',
  'clear.sub': '下一关已解锁',
  'clear.subLast': '本章的关卡都过完了，接着是章末测验',
  'clear.next': '下一关',
  'clear.quiz': '去做测验',
  'clear.stay': '留在这里',

  'reveal.title': '已揭示答案',
  'reveal.sub': '这一关记为「已揭示」，不算通过。可以看看这段代码是怎么一步步搭起来的。',

  'quiz.title': '章末测验',
  'quiz.intro': '这些题目答对的前提是你真的把前面几关跑过一遍。答完会显示每题的正确选项和理由，答错也一样。',
  'quiz.score': '答对 {n} / {m}',
  'quiz.answered': '已作答 {n} / {m}',
  'quiz.correctIs': '正确答案是 {mk}',
  'quiz.finish': '看结果',
  'quiz.retry': '重做',
  'quiz.next': '继续',

  'closing.title': '读完这一章之后',
  'closing.reading': '延伸阅读',
  'closing.try': '可以试试',
  'closing.back': '回到关卡',

  'help.title': '键盘',
  'help.note': '所有按钮的提示里也写着对应的快捷键。',
  'help.lang': '界面语言',
  'key.save': '保存当前文件',
  'key.undo': '撤销',
  'key.redo': '重做',
  'key.run': '运行',
  'key.format': '格式化',
  'key.rail': '折叠 / 展开章节栏',
  'key.reading': '收起 / 展开阅读材料',
  'key.palette': '命令面板',
  'key.stop': '停止运行',
  'key.help': '这个面板',

  'palette.title': '命令',
  'palette.placeholder': '输入命令名称',
  'palette.empty': '没有匹配的命令',
  'cmd.run': '运行',
  'cmd.stop': '停止运行',
  'cmd.save': '保存当前文件',
  'cmd.format': '格式化',
  'cmd.check': '检查语法',
  'cmd.rail': '折叠 / 展开章节栏',
  'cmd.reading': '收起 / 展开阅读材料',
  'cmd.help': '快捷键',
  'cmd.reveal': '揭示本关答案',
  'cmd.diff': '看它是怎么搭起来的',
  'cmd.quiz': '打开章末测验',
  'cmd.closing': '打开「读完之后」',
  'cmd.lang': '切换界面语言',
  'cmd.reset': '清空本地进度',

  'diff.title': '这段代码是怎么搭起来的',
  'diff.step': '第 {i} / {n} 步',
  'diff.blank': '起点：所有空位都还空着',
  'diff.filled': '填上第 {i} 处',
  'diff.prev': '上一步',
  'diff.next': '下一步',

  'toast.saved': '已保存 {path}',
  'toast.formatted': '已格式化',
  'toast.formatFail': '格式化没有生效：{msg}',
  'toast.checkFail': '检查失败：{msg}',
  'toast.buildFail': '构建没通过，看下面的诊断',
  'toast.runNeedsHoles': '还有空位没填，先把它们选完',
  'toast.revealed': '已填入全部正确选项',
  'toast.wrong': '这个选项不对，理由在选择框里',
  'toast.right': '这处对了',
  'toast.cleared': '关卡通过',
  'toast.stopped': '已停止',
  'toast.reset': '本地进度已清空',
  'toast.locked': '这一关还没解锁',
  'toast.noHoles': '这个文件里没有空位',
  'toast.runtimeFail': '运行时没能启动：{msg}',

  'run.expectFail': '程序跑起来了，但输出和这一关要求的不一样。',
  'run.expectWant': '期望里应当出现：{s}',
  'run.wrongPicks': '先把选错的地方改对，再运行。',

  'cdn.degraded':
    '没能从 CDN 加载 {what}，已切换到简化版（编辑器为纯文本框，终端为日志）。功能仍可用，只是没有高亮和光标。',
  'content.fail': '章节内容加载失败：{msg}',
  'shell.hint': '输入命令后回车。试试 ls、cat main.go、pwd。',

  'mock.nocompile': '[mock] 这个运行时不带编译器，没法真的执行任意一种组合。',
  'mock.blanks': '[mock] 和答案不一致的空位：{ids}',
};

const en = {
  'app.subtitle': 'Build a coding agent by hand',
  'app.progress': 'chapter progress',

  'level.locked': 'locked',
  'level.available': 'available',
  'level.cleared': 'cleared',
  'level.skipped': 'revealed',
  'rail.quiz': 'chapter quiz',
  'rail.closing': 'after the chapter',
  'rail.lockedHint': 'clear the previous level first',

  'btn.rail': 'collapse the rail',
  'btn.railExpand': 'expand the rail',
  'btn.format': 'format',
  'btn.save': 'save',
  'btn.reveal': 'reveal',
  'btn.help': 'shortcuts',
  'btn.reading': 'open the reading',
  'btn.readingHide': 'collapse the reading',
  'btn.run': 'run',
  'btn.stop': 'stop',
  'btn.clear': 'clear',
  'btn.close': 'close',
  'btn.diff': 'see how it was built',
  'tab.run': 'run',
  'tab.shell': 'shell',
  'files.title': 'files',
  'files.readonly': 'read-only',

  'st.checkIdle': 'not checked',
  'st.checking': 'checking',
  'st.checkOk': 'no problems',
  'st.checkErr': '{n} problems',
  'st.saved': 'saved',
  'st.dirty': 'unsaved',
  'st.holes': '{n} blanks left',
  'st.holesDone': 'all blanks filled',
  'st.holesWrong': '{n} wrong',
  'st.objective': 'objective',

  'rt.compiler': 'compiler',
  'rt.shell': 'shell',
  'rt.llm': 'model',
  'rt.idle': 'idle',
  'rt.loading': 'loading',
  'rt.ready': 'ready',
  'rt.unavailable': 'unavailable',
  'rt.booting': 'runtime starting',

  'read.title': 'reading',
  'read.none': 'This level has no reading material yet.',
  'read.jump.hole': 'jump to that blank in the code',
  'read.jump.line': 'jump to that line in the code',
  'read.jump.file': 'open that file',

  'chooser.title': 'what goes here',
  'chooser.right': 'this one is right',
  'chooser.wrong': 'not this one',
  'chooser.foot': 'Up/Down to move, Enter to pick, Esc to close',
  'chooser.filled': 'picked. Click again to change it.',

  'clear.title': 'level cleared',
  'clear.sub': 'the next level is unlocked',
  'clear.subLast': 'that was the last level; the chapter quiz is next',
  'clear.next': 'next level',
  'clear.quiz': 'take the quiz',
  'clear.stay': 'stay here',

  'reveal.title': 'answers revealed',
  'reveal.sub':
    'This level counts as revealed, not cleared. You can step through how the file was built up.',

  'quiz.title': 'chapter quiz',
  'quiz.intro':
    'These are answerable only if you actually ran the levels. Every question shows the right answer and why, whether or not you got it.',
  'quiz.score': '{n} of {m} right',
  'quiz.answered': '{n} of {m} answered',
  'quiz.correctIs': 'the right answer is {mk}',
  'quiz.finish': 'see the result',
  'quiz.retry': 'try again',
  'quiz.next': 'continue',

  'closing.title': 'after this chapter',
  'closing.reading': 'further reading',
  'closing.try': 'things to try',
  'closing.back': 'back to the levels',

  'help.title': 'keyboard',
  'help.note': 'Every button names its shortcut in its tooltip.',
  'help.lang': 'interface language',
  'key.save': 'save the open file',
  'key.undo': 'undo',
  'key.redo': 'redo',
  'key.run': 'run',
  'key.format': 'format',
  'key.rail': 'collapse / expand the rail',
  'key.reading': 'collapse / expand the reading',
  'key.palette': 'command palette',
  'key.stop': 'stop the run',
  'key.help': 'this panel',

  'palette.title': 'commands',
  'palette.placeholder': 'type a command',
  'palette.empty': 'no matching command',
  'cmd.run': 'run',
  'cmd.stop': 'stop the run',
  'cmd.save': 'save the open file',
  'cmd.format': 'format',
  'cmd.check': 'check syntax',
  'cmd.rail': 'collapse / expand the rail',
  'cmd.reading': 'collapse / expand the reading',
  'cmd.help': 'shortcuts',
  'cmd.reveal': 'reveal this level',
  'cmd.diff': 'see how it was built',
  'cmd.quiz': 'open the chapter quiz',
  'cmd.closing': 'open "after the chapter"',
  'cmd.lang': 'switch interface language',
  'cmd.reset': 'clear local progress',

  'diff.title': 'how this file was built up',
  'diff.step': 'step {i} of {n}',
  'diff.blank': 'start: every blank still empty',
  'diff.filled': 'blank {i} filled in',
  'diff.prev': 'back',
  'diff.next': 'forward',

  'toast.saved': 'saved {path}',
  'toast.formatted': 'formatted',
  'toast.formatFail': 'format did nothing: {msg}',
  'toast.checkFail': 'check failed: {msg}',
  'toast.buildFail': 'build failed, see the diagnostics',
  'toast.runNeedsHoles': 'blanks are still empty; fill them first',
  'toast.revealed': 'every blank filled with the right option',
  'toast.wrong': 'wrong option; the reason is in the chooser',
  'toast.right': 'that one is right',
  'toast.cleared': 'level cleared',
  'toast.stopped': 'stopped',
  'toast.reset': 'local progress cleared',
  'toast.locked': 'that level is not unlocked yet',
  'toast.noHoles': 'this file has no blanks',
  'toast.runtimeFail': 'the runtime did not start: {msg}',

  'run.expectFail': 'It ran, but the output is not what this level asks for.',
  'run.expectWant': 'expected to contain: {s}',
  'run.wrongPicks': 'fix the wrong picks first, then run.',

  'cdn.degraded':
    'Could not load {what} from the CDN, so this page switched to the plain version (a text box for the editor, a log for the terminal). Everything still works, without highlighting or a cursor.',
  'content.fail': 'could not load the chapter: {msg}',
  'shell.hint': 'Type a command and press Enter. Try ls, cat main.go, pwd.',

  'mock.nocompile': '[mock] This runtime has no compiler, so it cannot run an arbitrary assembly.',
  'mock.blanks': '[mock] blanks that differ from the answer key: {ids}',
};

const TABLES = { zh, en };
const KEY = 'biayn.lang';

let lang = localStorage.getItem(KEY) || 'zh';

export function getLang() {
  return lang;
}

export function setLang(next) {
  lang = TABLES[next] ? next : 'zh';
  localStorage.setItem(KEY, lang);
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  window.dispatchEvent(new CustomEvent('langchange'));
}

export function toggleLang() {
  setLang(lang === 'zh' ? 'en' : 'zh');
}

export function t(key, vars) {
  const s = TABLES[lang][key] ?? TABLES.zh[key] ?? key;
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
}

/** Chapter content stores {zh, en}; the en slot is present and empty for the
 *  prototype, so fall back rather than render a blank pane. */
export function L(pair) {
  if (pair == null) return '';
  if (typeof pair === 'string') return pair;
  return pair[lang] || pair.zh || pair.en || '';
}

document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
