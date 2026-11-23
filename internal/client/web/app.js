'use strict';

// CodeRoom student client UI. Talks only to the local client process over
// loopback; the client process is what talks to the teacher's server.

const S = {
  st: null,            // last /local/state payload
  serverTime: 0,       // server clock at last poll
  localAtPoll: 0,      // local clock at that same moment
  current: null,       // selected problem id
  editor: null,
  saveTimer: null,
  dirty: false,
  pollTimer: null,
};

const $ = (s, r = document) => r.querySelector(s);
const el = (tag, attrs = {}, ...kids) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') n.className = v;
    else if (k === 'html') n.innerHTML = v;
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else if (v === true) n.setAttribute(k, '');
    else n.setAttribute(k, v);
  }
  for (const kid of kids.flat()) {
    if (kid === null || kid === undefined || kid === false) continue;
    n.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return n;
};

// replaceChildren() renders a null argument as the text "null". Call sites
// that build children conditionally must go through this instead.
const setKids = (node, ...kids) =>
  node.replaceChildren(...kids.flat().filter((k) => k !== null && k !== undefined && k !== false));

function toast(msg, bad = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.toggle('bad', bad);
  t.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { t.hidden = true; }, bad ? 5000 : 2200);
}

async function call(path, body) {
  const res = await fetch('/local' + path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body || {}),
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { detail: text }; }
  if (!res.ok) throw new Error((data && (data.detail || data.error)) || `HTTP ${res.status}`);
  return data;
}

const VERDICT_LABEL = {
  AC: '通过', WA: '答案错误', TLE: '运行超时', MLE: '超出内存', RE: '运行时错误',
  CE: '编译错误', OLE: '输出超限', SE: '系统错误', PD: '判题中',
};
const verdictBadge = (v) => el('span', { class: `v v-${v || 'PD'}` },
  `${v || 'PD'} ${VERDICT_LABEL[v] || ''}`);

const LANG_LABEL = { cpp: 'C++', c: 'C', python: 'Python' };

// ------------------------------------------------------------ sheet / modal

function sheet(title, body, foot = []) {
  $('#sheet-title').textContent = title;
  $('#sheet-body').replaceChildren(...[body].flat().filter(Boolean));
  $('#sheet-foot').replaceChildren(...[foot].flat().filter(Boolean));
  $('#sheet').hidden = false;
}
const closeSheet = () => { $('#sheet').hidden = true; };
$('#sheet-close').addEventListener('click', closeSheet);
$('#sheet').addEventListener('click', (e) => { if (e.target.id === 'sheet') closeSheet(); });

// -------------------------------------------------------- markdown (subset)

// Deliberately small: headings, emphasis, inline/fenced code and lists. All
// input is escaped first - a problem statement is still untrusted text as far
// as this page is concerned.
function md(src) {
  const escHtml = (s) => s.replace(/[&<>]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c]));
  const lines = escHtml(src || '').split('\n');
  let out = '';
  let inCode = false, inList = false, para = [];

  const flushPara = () => {
    if (!para.length) return;
    out += '<p>' + inline(para.join('<br>')) + '</p>';
    para = [];
  };
  const closeList = () => { if (inList) { out += '</ul>'; inList = false; } };
  const inline = (s) => s
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|\W)\*([^*\n]+)\*/g, '$1<em>$2</em>');

  for (const raw of lines) {
    const line = raw.replace(/\s+$/, '');
    if (/^```/.test(line)) {
      flushPara(); closeList();
      out += inCode ? '</pre>' : '<pre>';
      inCode = !inCode;
      continue;
    }
    if (inCode) { out += line + '\n'; continue; }
    const h = line.match(/^(#{1,4})\s+(.*)$/);
    if (h) { flushPara(); closeList(); out += `<h${h[1].length + 2}>${inline(h[2])}</h${h[1].length + 2}>`; continue; }
    const li = line.match(/^\s*[-*+]\s+(.*)$/);
    if (li) {
      flushPara();
      if (!inList) { out += '<ul>'; inList = true; }
      out += '<li>' + inline(li[1]) + '</li>';
      continue;
    }
    if (line === '') { flushPara(); closeList(); continue; }
    para.push(line);
  }
  flushPara(); closeList();
  if (inCode) out += '</pre>';
  return out;
}

// ------------------------------------------------------------------- screens

function showScreen(name) {
  for (const id of ['connect', 'login', 'main']) {
    $('#screen-' + id).hidden = (id !== name);
  }
}

// --- connect ---------------------------------------------------------------

async function scanServers() {
  const list = $('#server-list');
  list.replaceChildren(el('div', { class: 'muted small' }, '搜索中…'));
  let found = [];
  try { found = await call('/discover', {}); } catch (e) { /* broadcast may be blocked */ }
  if (!found.length) {
    list.replaceChildren(el('div', { class: 'muted small' },
      '未自动发现服务端。请向教师确认服务器地址后手动输入。'));
    return;
  }
  list.replaceChildren(...found.map((s) => el('div', {
    class: 'item', onclick: () => connect(s.addr),
  }, el('span', {}, '🖥'), el('div', {},
    el('div', {}, s.name || 'CodeRoom 服务端'),
    el('div', { class: 'muted small' }, s.addr)))));
}

async function connect(addr) {
  $('#connect-err').textContent = '';
  try {
    const st = await call('/connect', { addr });
    applyState(st);
    enterLogin();
  } catch (e) {
    $('#connect-err').textContent = e.message;
  }
}

$('#btn-connect').addEventListener('click', () => connect($('#manual-addr').value));
$('#manual-addr').addEventListener('keydown', (e) => { if (e.key === 'Enter') connect($('#manual-addr').value); });
$('#btn-rescan').addEventListener('click', scanServers);
$('#btn-back').addEventListener('click', () => { showScreen('connect'); scanServers(); });

// --- login -----------------------------------------------------------------

function enterLogin() {
  const st = S.st;
  $('#login-server').textContent = '服务端 ' + (st.server_addr || '');
  const sel = $('#exam-select');
  sel.replaceChildren(...st.exams.map((e) => el('option', { value: e.id },
    `${e.name}  [${e.type === 'exam' ? '考试' : '练习'}]`)));
  if (!st.exams.length) {
    sel.replaceChildren(el('option', { value: '' }, '当前没有开放的考试或练习'));
  }
  const syncPasscode = () => {
    const e = st.exams.find((x) => String(x.id) === sel.value);
    $('#passcode-wrap').hidden = !(e && e.passcode_required);
  };
  sel.addEventListener('change', syncPasscode);
  syncPasscode();
  if (st.student_no) $('#in-student-no').value = st.student_no;
  if (st.name) $('#in-name').value = st.name;
  showScreen('login');
}

$('#btn-login').addEventListener('click', async () => {
  const examId = Number($('#exam-select').value);
  const studentNo = $('#in-student-no').value.trim();
  const name = $('#in-name').value.trim();
  $('#login-err').textContent = '';
  if (!examId) return ($('#login-err').textContent = '请选择考试或练习');
  if (!studentNo || !name) return ($('#login-err').textContent = '请填写学号和姓名');
  try {
    const st = await call('/login', {
      exam_id: examId, student_no: studentNo, name,
      passcode: $('#in-passcode').value,
    });
    applyState(st);
    enterMain();
  } catch (e) {
    $('#login-err').textContent = e.message;
  }
});

// --- main ------------------------------------------------------------------

function enterMain() {
  showScreen('main');
  buildEditor();
  renderMain();
  startPolling();
}

function buildEditor() {
  if (S.editor) return;
  S.editor = new CodeEditor($('#editor-host'), {
    language: 'cpp',
    onChange: () => {
      S.dirty = true;
      $('#ed-saved').textContent = '未保存';
      clearTimeout(S.saveTimer);
      S.saveTimer = setTimeout(saveDraft, 1500); // autosave while typing
    },
    onPaste: ({ chars, lines }) => {
      // Reported for every paste; the server decides what counts as unusual.
      // This is the evidence trail that replaces "block the clipboard".
      call('/event', {
        type: chars >= 200 || lines >= 10 ? 'paste_large' : 'paste',
        severity: chars >= 200 || lines >= 10 ? 'warn' : 'info',
        payload: { chars, lines, problem_id: S.current },
      }).catch(() => {});
    },
  });

  const sel = $('#ed-lang');
  sel.addEventListener('change', () => {
    S.editor.setLanguage(sel.value);
    saveDraft();
  });
  $('#btn-save').addEventListener('click', () => saveDraft(true));
  $('#btn-submit').addEventListener('click', submitCurrent);
}

async function saveDraft(explicit = false) {
  if (!S.current) return;
  try {
    await call('/draft', {
      problem_id: S.current,
      language: $('#ed-lang').value,
      code: S.editor.getValue(),
    });
    S.dirty = false;
    $('#ed-saved').textContent = '已保存到本机';
    if (explicit) toast('已保存到本机，稍后自动上传服务端');
  } catch (e) {
    $('#ed-saved').textContent = '保存失败';
    toast('本地保存失败: ' + e.message, true);
  }
}

async function submitCurrent() {
  if (!S.current) return;
  const code = S.editor.getValue();
  if (!code.trim()) return toast('代码为空，无法提交', true);
  const item = (S.st.paper?.items || []).find((i) => i.problem_id === S.current);
  const title = item?.problem?.title || ('#' + S.current);
  if (!confirm(`确认提交「${title}」？\n\n语言: ${LANG_LABEL[$('#ed-lang').value]}\n代码 ${code.length} 字符`)) return;

  $('#btn-submit').disabled = true;
  try {
    const r = await call('/submit', {
      problem_id: S.current, language: $('#ed-lang').value, code,
    });
    S.dirty = false;
    $('#ed-saved').textContent = '已提交';
    toast(r.message || '提交成功');
    await refresh();
  } catch (e) {
    toast('提交失败: ' + e.message, true);
  } finally {
    $('#btn-submit').disabled = false;
  }
}

function selectProblem(id) {
  if (S.current === id) return;
  if (S.current && S.dirty) saveDraft();
  S.current = id;
  const draft = S.st.drafts[String(id)];
  const item = (S.st.paper?.items || []).find((i) => i.problem_id === id);
  const allowed = item?.problem?.languages || ['cpp'];

  const sel = $('#ed-lang');
  sel.replaceChildren(...allowed.map((l) => el('option', { value: l }, LANG_LABEL[l] || l)));
  sel.value = draft && allowed.includes(draft.language) ? draft.language : allowed[0];

  S.editor.setLanguage(sel.value);
  S.editor.setValue(draft ? draft.code : '');
  S.dirty = false;
  $('#ed-saved').textContent = draft ? '已保存到本机' : '';
  renderMain();
}

function renderMain() {
  const st = S.st;
  if (!st || !st.exam) return;

  $('#hdr-name').textContent = st.name;
  $('#hdr-no').textContent = st.student_no + (st.class ? ' · ' + st.class : '');
  $('#hdr-exam').textContent = st.exam.name +
    (st.exam.type === 'exam' ? ' [考试]' : ' [练习]');

  const items = st.paper?.items || [];
  const locked = !st.paper || st.paper.locked;

  $('#locked-banner').hidden = !locked;
  if (locked) {
    $('#locked-banner').textContent =
      (st.paper?.reason || '题目尚未下发') + '。题目会在教师开考后自动出现，请保持本程序开启。';
  }
  $('.panes').classList.toggle('locked', locked);
  $('#pane-problem').hidden = locked;
  document.querySelector('.pane-editor').hidden = locked;

  // problem list
  const bestByProblem = {};
  for (const r of st.results || []) {
    const cur = bestByProblem[r.problem_id];
    if (!cur || r.score > cur.score) bestByProblem[r.problem_id] = r;
  }
  $('#pane-list').replaceChildren(...items.map((it) => {
    const best = bestByProblem[it.problem_id];
    const draft = st.drafts[String(it.problem_id)];
    return el('div', {
      class: 'plist-item' + (it.problem_id === S.current ? ' active' : ''),
      onclick: () => selectProblem(it.problem_id),
    },
      el('span', { class: 'idx' }, it.index),
      el('span', { class: 'ttl' }, it.problem ? it.problem.title : '题目'),
      best && st.results_visible ? el('span', { class: `v v-${best.verdict}` }, best.verdict)
        : (draft && draft.code ? el('span', { class: 'muted small' }, '●') : null));
  }));

  if (!locked && !S.current && items.length) {
    selectProblem(items[0].problem_id);
    return;
  }

  // statement
  const item = items.find((i) => i.problem_id === S.current);
  if (item && item.problem) {
    const p = item.problem;
    $('#ed-title').textContent = `第${item.index}题 · ${p.title} (${item.score}分)`;
    $('#pane-problem').replaceChildren(el('div', { class: 'statement' },
      el('h2', {}, `第${item.index}题 ${p.title}`),
      el('div', { class: 'meta' },
        el('span', { class: 'tag' }, `${item.score} 分`),
        el('span', { class: 'tag' }, `时限 ${p.time_limit_ms} ms`),
        el('span', { class: 'tag' }, `内存 ${p.memory_limit_mb} MB`),
        el('span', { class: 'tag' }, (p.languages || []).map((l) => LANG_LABEL[l] || l).join(' / '))),
      el('div', { html: md(p.description_md) }),
      p.input_desc ? el('div', {}, el('h3', {}, '输入格式'), el('div', { html: md(p.input_desc) })) : null,
      p.output_desc ? el('div', {}, el('h3', {}, '输出格式'), el('div', { html: md(p.output_desc) })) : null,
      ...(p.samples || []).map((s, i) => el('div', {},
        el('h3', {}, `样例 ${i + 1}`),
        el('div', { class: 'sample' },
          el('div', {}, el('div', { class: 'lbl' }, '输入'), el('pre', {}, s.input)),
          el('div', {}, el('div', { class: 'lbl' }, '输出'), el('pre', {}, s.output))),
        s.note ? el('p', { class: 'muted small' }, s.note) : null)),
    ));
  }

  renderResults();
  renderStatus();
}

function renderResults() {
  const st = S.st;
  const box = $('#result-box');
  if (!st.results_visible) {
    box.replaceChildren(el('div', { class: 'muted small' },
      st.results_reason || '本场考试成绩由教师公布后可见。'));
    return;
  }
  const mine = (st.results || []).filter((r) => r.problem_id === S.current);
  if (!mine.length) {
    box.replaceChildren(el('div', { class: 'muted small' }, '还没有提交记录。'));
    return;
  }
  const latest = mine[0];
  setKids(box,
    el('div', { class: 'row' },
      el('strong', {}, `第 ${latest.seq} 次提交`),
      verdictBadge(latest.verdict),
      el('span', { class: 'muted small' },
        `${latest.score}/${latest.max_score} 分 · ${latest.time_ms}ms`),
      el('div', { class: 'spacer' }),
      mine.length > 1 ? el('button', {
        onclick: () => sheet('历史提交', mine.map((r) => el('div', { class: 'row' },
          el('span', { class: 'muted small' }, `第${r.seq}次`),
          verdictBadge(r.verdict),
          el('span', { class: 'muted small' }, `${r.score}/${r.max_score}分`))),
          [el('button', { onclick: closeSheet }, '关闭')]),
      }, `历史 (${mine.length})`) : null),
    latest.compile_log ? el('pre', { class: 'log' }, latest.compile_log) : null,
    latest.cases && latest.cases.length ? el('table', { class: 'cases' },
      el('thead', {}, el('tr', {},
        el('th', {}, '测试点'), el('th', {}, '结果'), el('th', {}, '得分'),
        el('th', {}, '耗时'), el('th', {}, '说明'))),
      el('tbody', {}, latest.cases.map((c) => el('tr', {},
        el('td', {}, c.index), el('td', {}, verdictBadge(c.verdict)),
        el('td', {}, `${c.score}/${c.max_score}`), el('td', {}, c.time_ms + 'ms'),
        el('td', {}, c.message || ''))))) : null,
  );
}

function serverNow() {
  return S.serverTime + (Date.now() - S.localAtPoll);
}

function renderStatus() {
  const st = S.st;
  const sync = $('#hdr-sync');
  const bits = [el('span', { class: 'dot ' + (st.connected ? 'on' : 'off') })];
  bits.push(el('span', { class: st.connected ? 'muted' : 'err' },
    st.connected ? '已连接' : '连接中断'));
  if (st.pending_uploads) bits.push(el('span', { class: 'warn' }, `${st.pending_uploads} 份待上传`));
  // Students are told the room is monitored. Two words in the header, the
  // detail in the 更多 sheet: a check nobody knows about deters nobody, and
  // being up front about it is what keeps an argument afterwards short.
  if ((st.monitors || []).length) {
    bits.push(el('span', {
      class: 'muted',
      title: '本场启用的考场监控：' + st.monitors.join('、') + '。详见「更多」。',
    }, '监考中'));
  }
  sync.replaceChildren(...bits);
  sync.title = st.last_error || '与服务端同步正常';

  const cd = $('#hdr-countdown');
  if (st.deadline) {
    const left = st.deadline - serverNow();
    if (left <= 0) {
      cd.textContent = '已到交卷时间';
      cd.classList.add('urgent');
    } else {
      const s = Math.floor(left / 1000);
      const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
      const p = (n) => String(n).padStart(2, '0');
      cd.textContent = (h > 0 ? h + ':' : '') + p(m) + ':' + p(sec);
      cd.classList.toggle('urgent', left < 5 * 60 * 1000);
    }
  } else {
    cd.textContent = '';
  }
  $('#btn-handin').textContent = st.handed_in ? '已交卷' : '交卷';
  $('#btn-handin').disabled = !!st.handed_in;
}

// --- hand-in / menu --------------------------------------------------------

$('#btn-handin').addEventListener('click', async () => {
  if (S.dirty) await saveDraft();
  const pending = S.st.pending_uploads;
  const msg = pending
    ? `还有 ${pending} 份代码尚未上传到服务端，交卷会先尝试上传。确认交卷？`
    : '确认交卷？交卷后仍可继续提交，但教师会看到你的交卷时间。';
  if (!confirm(msg)) return;
  try {
    const st = await call('/handin', {});
    applyState(st);
    renderMain();
    toast('已交卷');
  } catch (e) {
    toast('交卷失败: ' + e.message, true);
  }
});

$('#btn-menu').addEventListener('click', () => {
  const st = S.st;
  sheet('更多', [
    el('div', {}, el('strong', {}, '连接信息')),
    el('div', { class: 'muted small' },
      `服务端 ${st.server_addr} · 客户端 v${st.version}`),
    el('div', { class: 'muted small' },
      `最后同步 ${st.last_sync_at ? new Date(st.last_sync_at).toLocaleTimeString() : '从未'} · ` +
      `待上传 ${st.pending_uploads} 份 · 待上报事件 ${st.pending_events} 条`),
    el('hr'),
    el('div', {}, el('strong', {}, '考场监控')),
    (st.monitors || []).length
      ? el('div', { class: 'muted small' },
          `本场启用：${st.monitors.join('、')}。` +
          '监控只记录并上报，不会阻止你做任何操作，也不读取你的文件内容。' +
          '代码靠粘贴录入，所以剪贴板不会被禁用。')
      : el('div', { class: 'muted small' }, '本场未启用考场监控。'),
    st.last_error ? el('div', { class: 'err small' }, st.last_error) : null,
    el('hr'),
    el('div', { class: 'row' },
      el('button', {
        onclick: async () => {
          try { applyState(await call('/sync', {})); renderMain(); toast('已与服务端同步'); }
          catch (e) { toast(e.message, true); }
        },
      }, '立即同步'),
      el('button', {
        onclick: async () => {
          try {
            const r = await call('/export', {});
            sheet('已导出答卷', [
              el('p', {}, '如果网络无法恢复，请将下面这个文件夹交给教师：'),
              el('pre', { class: 'log' }, r.path),
            ], [el('button', { onclick: closeSheet }, '知道了')]);
          } catch (e) { toast(e.message, true); }
        },
      }, '导出本机答卷'),
    ),
    el('p', { class: 'muted small' },
      '「导出本机答卷」用于网络故障时的兜底补交：代码始终先保存在本机，再上传服务端。'),
  ], [
    el('button', {
      class: 'danger', onclick: async () => {
        if (!confirm('退出登录？本机已保存的代码不会丢失。')) return;
        applyState(await call('/logout', {}));
        closeSheet();
        stopPolling();
        enterLogin();
      },
    }, '退出登录'),
    el('button', { onclick: closeSheet }, '关闭'),
  ]);
});

// --- polling ---------------------------------------------------------------

function applyState(st) {
  S.st = st;
  if (st.server_time) {
    S.serverTime = st.server_time;
    S.localAtPoll = Date.now();
  }
}

async function refresh() {
  try {
    applyState(await call('/state'));
    if (S.st.logged_in) renderMain(); else { stopPolling(); enterLogin(); }
  } catch (e) { /* the local process is gone; nothing useful to show */ }
}

function startPolling() {
  stopPolling();
  S.pollTimer = setInterval(refresh, 2000);
  S.clockTimer = setInterval(renderStatus, 1000);
}
function stopPolling() {
  clearInterval(S.pollTimer);
  clearInterval(S.clockTimer);
  S.pollTimer = S.clockTimer = null;
}

// Window focus is reported so the teacher can see a student switching away.
// Whether it is recorded at all is the server's policy decision.
window.addEventListener('focus', () => call('/focus', { focused: true }).catch(() => {}));
window.addEventListener('blur', () => call('/focus', { focused: false }).catch(() => {}));
window.addEventListener('beforeunload', () => { if (S.dirty) saveDraft(); });

// --- boot ------------------------------------------------------------------

(async function boot() {
  try {
    applyState(await call('/state'));
  } catch (e) {
    document.body.replaceChildren(el('div', { class: 'screen' },
      el('div', { class: 'card' }, el('h1', {}, '客户端未就绪'), el('p', { class: 'err' }, e.message))));
    return;
  }
  if (S.st.logged_in) { enterMain(); return; }

  // A lab with broadcast disabled can deploy the client with -server=<addr>,
  // which arrives here as a query parameter.
  const preset = new URLSearchParams(location.search).get('server');
  if (preset) {
    try {
      applyState(await call('/connect', { addr: preset }));
      enterLogin();
      return;
    } catch (e) {
      showScreen('connect');
      $('#manual-addr').value = preset;
      $('#connect-err').textContent = '指定的服务端无法连接: ' + e.message;
      scanServers();
      return;
    }
  }

  if (S.st.server_addr) {
    // Reconnect to the address this machine used last, so a restarted client
    // lands straight on the login screen.
    try {
      applyState(await call('/connect', { addr: S.st.server_addr }));
      enterLogin();
      return;
    } catch (e) { /* fall through to discovery */ }
  }
  showScreen('connect');
  scanServers();
})();
