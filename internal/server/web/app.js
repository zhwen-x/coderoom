'use strict';

// CodeRoom teacher console. No build step on purpose: the server ships as a
// single executable, so the UI is plain ES modules-free JavaScript served from
// an embedded filesystem.

const S = {
  token: '',
  view: 'exams',
  examId: null,
  exams: [],
  problems: [],
  lastEventId: 0,
  timer: null,
};

// ------------------------------------------------------------------- utils

const $ = (sel, root = document) => root.querySelector(sel);
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

const fmtTime = (ms) => {
  if (!ms) return '-';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
};
const fmtDur = (ms) => {
  if (ms < 0) ms = 0;
  const s = Math.floor(ms / 1000);
  return `${Math.floor(s / 60)}分${String(s % 60).padStart(2, '0')}秒`;
};

// replaceChildren() renders a null argument as the text "null"; filter like el().
const setKids = (node, ...kids) =>
  node.replaceChildren(...kids.flat().filter((k) => k !== null && k !== undefined && k !== false));

function toast(msg, bad = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.toggle('bad', bad);
  t.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { t.hidden = true; }, bad ? 5000 : 2500);
}

async function api(path, opts = {}) {
  const res = await fetch('/api/teacher' + path, {
    ...opts,
    headers: {
      'Content-Type': 'application/json',
      'X-Admin-Token': S.token,
      ...(opts.headers || {}),
    },
  });
  if (res.status === 401) {
    sessionStorage.removeItem('coderoom_token');
    location.reload();
    throw new Error('unauthorized');
  }
  const text = await res.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = { detail: text }; }
  if (!res.ok) throw new Error((body && (body.detail || body.error)) || `HTTP ${res.status}`);
  return body;
}

const VERDICT_LABEL = {
  AC: '通过', WA: '答案错误', TLE: '超时', MLE: '超内存', RE: '运行错误',
  CE: '编译错误', OLE: '输出超限', SE: '系统错误', PD: '待判',
};
const verdictBadge = (v) =>
  el('span', { class: `v v-${v || 'PD'}`, title: VERDICT_LABEL[v] || '' }, v || 'PD');

// ------------------------------------------------------------------- modal

function showModal(title, bodyNodes, footNodes = []) {
  $('#modal-title').textContent = title;
  const body = $('#modal-body');
  body.replaceChildren(...[bodyNodes].flat().filter(Boolean));
  $('#modal-foot').replaceChildren(...[footNodes].flat().filter(Boolean));
  $('#modal').hidden = false;
}
function closeModal() { $('#modal').hidden = true; }
$('#modal-close').addEventListener('click', closeModal);
$('#modal').addEventListener('click', (e) => { if (e.target.id === 'modal') closeModal(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeModal(); });

// -------------------------------------------------------------------- gate

function bootGate() {
  const url = new URL(location.href);
  const fromUrl = url.searchParams.get('t');
  if (fromUrl) {
    sessionStorage.setItem('coderoom_token', fromUrl);
    url.searchParams.delete('t');
    history.replaceState(null, '', url.pathname + url.search);
  }
  const saved = sessionStorage.getItem('coderoom_token');
  if (saved) { S.token = saved; return start(); }

  $('#gate').hidden = false;
  const submit = async () => {
    S.token = $('#gate-token').value.trim();
    if (!S.token) return;
    try {
      await api('/system');
      sessionStorage.setItem('coderoom_token', S.token);
      $('#gate').hidden = true;
      start();
    } catch (e) {
      $('#gate-err').textContent = '口令无效';
    }
  };
  $('#gate-btn').addEventListener('click', submit);
  $('#gate-token').addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
}

async function start() {
  $('#gate').hidden = true;
  $('#app').hidden = false;
  for (const b of document.querySelectorAll('#tabs button')) {
    b.addEventListener('click', () => switchView(b.dataset.view));
  }
  await refreshExams();
  switchView('exams');
}

function switchView(v) {
  S.view = v;
  for (const b of document.querySelectorAll('#tabs button')) {
    b.classList.toggle('active', b.dataset.view === v);
  }
  clearInterval(S.timer);
  S.timer = null;
  render();
}

function render() {
  const views = {
    exams: viewExams, problems: viewProblems, monitor: viewMonitor,
    submissions: viewSubmissions, scores: viewScores, system: viewSystem,
  };
  (views[S.view] || viewExams)();
}

function setMain(...nodes) { $('#view').replaceChildren(...nodes.flat().filter(Boolean)); }

function examContext() {
  const e = S.exams.find((x) => x.id === S.examId);
  $('#ctx').textContent = e ? `当前: ${e.name} (${e.state})` : '未选择考试';
}

// ---------------------------------------------------------------- exam view

async function refreshExams() {
  S.exams = await api('/exams');
  if (!S.examId && S.exams.length) S.examId = S.exams[0].id;
  examContext();
}

const STATE_LABEL = {
  draft: '草稿', published: '已发布', running: '进行中',
  ended: '已结束', judging: '判题中', graded: '已评分',
};

function viewExams() {
  const rows = S.exams.map((e) => el('tr', { class: e.id === S.examId ? 'clickable' : 'clickable',
      onclick: () => { S.examId = e.id; examContext(); render(); } },
    el('td', {}, e.id === S.examId ? '▶ ' : '', e.name),
    el('td', {}, e.type === 'exam' ? '考试' : '练习'),
    el('td', {}, el('span', { class: 'tag' }, STATE_LABEL[e.state] || e.state)),
    el('td', {}, e.roster_required ? '名单校验' : '自由输入'),
    el('td', {}, e.passcode ? '有口令' : '-'),
    el('td', {}, e.show_results ? el('span', { class: 'ok' }, '成绩可见') : '成绩未公布'),
    el('td', {}, fmtTime(e.created_at)),
  ));

  setMain(
    el('div', { class: 'panel' },
      el('div', { class: 'row' },
        el('h2', {}, '考试 / 练习'),
        el('button', { class: 'primary', onclick: openExamEditor }, '新建'),
        el('button', { onclick: async () => { await refreshExams(); render(); } }, '刷新'),
      ),
      S.exams.length
        ? el('div', { class: 'scroll' }, el('table', {},
            el('thead', {}, el('tr', {},
              ...['名称', '类型', '状态', '身份', '口令', '成绩', '创建时间'].map((h) => el('th', {}, h)))),
            el('tbody', {}, rows)))
        : el('div', { class: 'empty' }, '还没有考试或练习，点击「新建」开始。'),
    ),
    S.examId ? examControlPanel() : null,
  );
}

function examControlPanel() {
  const e = S.exams.find((x) => x.id === S.examId);
  if (!e) return null;

  const setState = async (state) => {
    try {
      await api(`/exams/${e.id}/state`, { method: 'POST', body: JSON.stringify({ state }) });
      await refreshExams();
      render();
      toast(`状态已改为「${STATE_LABEL[state] || state}」`);
    } catch (err) { toast(err.message, true); }
  };

  const box = el('div', { class: 'panel' },
    el('h2', {}, `控制: ${e.name}`),
    el('div', { class: 'row' },
      el('button', { onclick: () => setState('published'), disabled: e.state === 'published' }, '发布(允许登录)'),
      el('button', { class: 'primary', onclick: () => setState('running'), disabled: e.state === 'running' }, '开考(下发题面)'),
      el('button', { onclick: () => setState('ended') }, '结束'),
      el('button', {
        onclick: async () => {
          try {
            const r = await api(`/exams/${e.id}/judge`, { method: 'POST' });
            toast(r.queued ? `已将 ${r.queued} 份提交加入判题队列` : '没有可判的终稿提交');
            await refreshExams(); render();
          } catch (err) { toast(err.message, true); }
        },
      }, '全部重判'),
      el('button', {
        onclick: async () => {
          try {
            await api(`/exams/${e.id}/release`, {
              method: 'POST', body: JSON.stringify({ show: !e.show_results }),
            });
            await refreshExams(); render();
            toast(e.show_results ? '已隐藏成绩' : '成绩已对学生公布');
          } catch (err) { toast(err.message, true); }
        },
      }, e.show_results ? '隐藏成绩' : '公布成绩'),
      el('button', {
        class: 'danger',
        onclick: async () => {
          if (!confirm(`删除「${e.name}」？该考试的会话、提交记录和判题结果都会一并删除，且不可恢复。`)) return;
          try {
            await api(`/exams/${e.id}`, { method: 'DELETE' });
            S.examId = null; await refreshExams(); render(); toast('已删除');
          } catch (err) { toast(err.message, true); }
        },
      }, '删除'),
    ),
    el('p', { class: 'muted' },
      '发布后学生可登录但看不到题面；开考后才下发题面并开始倒计时。'),
  );
  api(`/exams/${e.id}`).then((d) => {
    box.append(el('div', { class: 'scroll' }, el('table', {},
      el('thead', {}, el('tr', {},
        el('th', {}, '题号'), el('th', {}, '题目'), el('th', { class: 'num' }, '分值'),
        el('th', { class: 'num' }, '测试点'))),
      el('tbody', {}, d.items.map((it) => el('tr', {},
        el('td', {}, it.idx), el('td', {}, it.title || `#${it.problem_id}`),
        el('td', { class: 'num' }, it.score),
        el('td', { class: 'num' }, it.case_count || el('span', { class: 'warn' }, '0 ⚠')),
      ))))));
  }).catch(() => {});
  return box;
}

async function openExamEditor() {
  if (!S.problems.length) S.problems = await api('/problems');
  if (!S.problems.length) {
    toast('请先在「题库」中创建题目', true);
    return;
  }
  const name = el('input', { value: '' });
  const type = el('select', {},
    el('option', { value: 'practice' }, '练习（即时判题，可多次重提）'),
    el('option', { value: 'exam' }, '考试（成绩需教师公布）'));
  const duration = el('input', { type: 'number', value: '0', min: '0' });
  const passcode = el('input', { value: '' });
  const roster = el('input', { type: 'checkbox' });
  const rejudge = el('input', { type: 'checkbox' });
  rejudge.checked = true;

  const picks = el('div', { class: 'caselist' });
  const addPick = (problemId = S.problems[0].id, score = 10) => {
    const sel = el('select', {}, ...S.problems.map((p) =>
      el('option', { value: p.id, selected: p.id === problemId },
        `#${p.id} ${p.title}${p.case_count ? '' : ' (无测试数据)'}`)));
    const sc = el('input', { type: 'number', value: score, min: '1' });
    const row = el('div', { class: 'row' }, sel, el('span', { class: 'muted' }, '分值'), sc,
      el('button', { class: 'danger', onclick: () => row.remove() }, '移除'));
    row._get = () => ({ problem_id: Number(sel.value), score: Number(sc.value) });
    picks.append(row);
  };
  addPick();

  showModal('新建考试 / 练习', [
    el('div', { class: 'grid2' },
      el('label', {}, '名称', name),
      el('label', {}, '类型', type),
      el('label', {}, '时长(分钟, 0 表示不限)', duration),
      el('label', {}, '考试口令(留空则不需要)', passcode),
    ),
    el('div', { class: 'row' },
      el('label', { class: 'row' }, roster, '要求学号姓名与名单匹配'),
      el('label', { class: 'row' }, rejudge, '允许重复提交'),
    ),
    el('div', {},
      el('div', { class: 'row' }, el('h3', {}, '试卷题目'),
        el('button', { onclick: () => addPick() }, '添加题目')),
      picks),
  ], [
    el('button', { onclick: closeModal }, '取消'),
    el('button', {
      class: 'primary',
      onclick: async () => {
        const items = [...picks.children].map((r) => r._get()).filter((x) => x.problem_id);
        if (!name.value.trim()) return toast('请填写名称', true);
        if (!items.length) return toast('请至少添加一道题目', true);
        try {
          await api('/exams', {
            method: 'POST',
            body: JSON.stringify({
              name: name.value.trim(),
              type: type.value,
              duration_min: Number(duration.value) || 0,
              passcode: passcode.value.trim(),
              roster_required: roster.checked,
              allow_rejudge: rejudge.checked,
              show_results: type.value === 'practice',
              state: 'draft',
              items,
            }),
          });
          closeModal();
          await refreshExams();
          render();
          toast('已创建，点击「发布」后学生即可登录');
        } catch (err) { toast(err.message, true); }
      },
    }, '创建'),
  ]);
}

// ------------------------------------------------------------- problem view

async function viewProblems() {
  S.problems = await api('/problems');
  const rows = S.problems.map((p) => el('tr', {},
    el('td', {}, '#' + p.id),
    el('td', {}, p.title),
    el('td', { class: 'num' }, p.time_limit_ms + 'ms'),
    el('td', { class: 'num' }, p.memory_limit_mb + 'MB'),
    el('td', { class: 'num' },
      p.case_count ? `${p.case_count} (${p.case_score}分)` : el('span', { class: 'warn' }, '0 ⚠')),
    el('td', {}, (p.languages || []).join(' / ')),
    el('td', { class: 'row' },
      el('button', { onclick: () => openProblemEditor(p.id) }, '编辑'),
      el('button', { onclick: () => openTestcaseEditor(p.id, p.title) }, '测试数据'),
      el('button', {
        class: 'danger',
        onclick: async () => {
          if (!confirm(`删除题目「${p.title}」及其全部测试数据？不可恢复。`)) return;
          try { await api(`/problems/${p.id}`, { method: 'DELETE' }); viewProblems(); toast('已删除'); }
          catch (e) { toast(e.message, true); }
        },
      }, '删除')),
  ));

  setMain(el('div', { class: 'panel' },
    el('div', { class: 'row' },
      el('h2', {}, '题库'),
      el('button', { class: 'primary', onclick: () => openProblemEditor(null) }, '新建题目'),
      el('button', { onclick: viewProblems }, '刷新')),
    S.problems.length
      ? el('div', { class: 'scroll' }, el('table', {},
          el('thead', {}, el('tr', {},
            ...['ID', '标题', '时限', '内存', '测试点', '语言', '操作'].map((h) => el('th', {}, h)))),
          el('tbody', {}, rows)))
      : el('div', { class: 'empty' }, '题库为空，点击「新建题目」。')));
}

async function openProblemEditor(id) {
  const p = id ? await api(`/problems/${id}`) : {
    title: '', description_md: '', input_desc: '', output_desc: '', samples: [],
    time_limit_ms: 1000, memory_limit_mb: 256, difficulty: 2, tags: [],
    languages: ['cpp', 'c', 'python'], score_mode: 'sum',
  };

  const f = {
    title: el('input', { value: p.title }),
    desc: el('textarea', { rows: '7' }),
    inDesc: el('textarea', { rows: '3' }),
    outDesc: el('textarea', { rows: '3' }),
    tl: el('input', { type: 'number', value: p.time_limit_ms, min: '100' }),
    ml: el('input', { type: 'number', value: p.memory_limit_mb, min: '16' }),
    diff: el('input', { type: 'number', value: p.difficulty, min: '1', max: '5' }),
    tags: el('input', { value: (p.tags || []).join(',') }),
    mode: el('select', {},
      el('option', { value: 'sum', selected: p.score_mode !== 'allornothing' }, '按测试点累加'),
      el('option', { value: 'allornothing', selected: p.score_mode === 'allornothing' }, '全部通过才得分')),
  };
  f.desc.value = p.description_md || '';
  f.inDesc.value = p.input_desc || '';
  f.outDesc.value = p.output_desc || '';

  const langs = ['cpp', 'c', 'python'].map((l) => {
    const cb = el('input', { type: 'checkbox' });
    cb.checked = (p.languages || []).includes(l);
    cb._lang = l;
    return el('label', { class: 'row' }, cb, l);
  });

  const samples = el('div', { class: 'caselist' });
  const addSample = (input = '', output = '', note = '') => {
    const i = el('textarea', { rows: '3' }); i.value = input;
    const o = el('textarea', { rows: '3' }); o.value = output;
    const n = el('input', { value: note });
    const box = el('div', { class: 'case' },
      el('div', { class: 'grid2' },
        el('label', {}, '样例输入', i), el('label', {}, '样例输出', o)),
      el('label', {}, '说明', n),
      el('div', { class: 'row' }, el('button', { class: 'danger', onclick: () => box.remove() }, '移除样例')));
    box._get = () => ({ input: i.value, output: o.value, note: n.value });
    samples.append(box);
  };
  (p.samples || []).forEach((s) => addSample(s.input, s.output, s.note));

  showModal(id ? `编辑题目 #${id}` : '新建题目', [
    el('label', {}, '标题', f.title),
    el('label', {}, '题目描述 (Markdown)', f.desc),
    el('div', { class: 'grid2' },
      el('label', {}, '输入格式', f.inDesc),
      el('label', {}, '输出格式', f.outDesc)),
    el('div', { class: 'grid2' },
      el('label', {}, '时间限制(ms)', f.tl),
      el('label', {}, '内存限制(MB)', f.ml),
      el('label', {}, '难度 1-5', f.diff),
      el('label', {}, '知识点标签(逗号分隔)', f.tags),
      el('label', {}, '评分方式', f.mode)),
    el('div', {}, el('div', { class: 'muted' }, '允许的语言'), el('div', { class: 'row' }, langs)),
    el('div', {},
      el('div', { class: 'row' }, el('h3', {}, '样例'), el('button', { onclick: () => addSample() }, '添加样例')),
      samples),
  ], [
    el('button', { onclick: closeModal }, '取消'),
    el('button', {
      class: 'primary',
      onclick: async () => {
        const chosen = langs.map((l) => l.querySelector('input')).filter((c) => c.checked).map((c) => c._lang);
        if (!f.title.value.trim()) return toast('请填写标题', true);
        if (!chosen.length) return toast('请至少选择一种语言', true);
        const body = {
          title: f.title.value.trim(),
          description_md: f.desc.value,
          input_desc: f.inDesc.value,
          output_desc: f.outDesc.value,
          samples: [...samples.children].map((s) => s._get()),
          time_limit_ms: Number(f.tl.value), memory_limit_mb: Number(f.ml.value),
          difficulty: Number(f.diff.value),
          tags: f.tags.value.split(',').map((t) => t.trim()).filter(Boolean),
          languages: chosen, score_mode: f.mode.value,
        };
        try {
          if (id) await api(`/problems/${id}`, { method: 'PUT', body: JSON.stringify(body) });
          else await api('/problems', { method: 'POST', body: JSON.stringify(body) });
          closeModal(); viewProblems(); toast('已保存');
        } catch (e) { toast(e.message, true); }
      },
    }, '保存'),
  ]);
}

async function openTestcaseEditor(id, title) {
  const cases = await api(`/problems/${id}/testcases`);
  const list = el('div', { class: 'caselist' });
  const addCase = (input = '', output = '', score = 10, isSample = false) => {
    const i = el('textarea', { rows: '4' }); i.value = input;
    const o = el('textarea', { rows: '4' }); o.value = output;
    const sc = el('input', { type: 'number', value: score, min: '1' });
    const samp = el('input', { type: 'checkbox' }); samp.checked = isSample;
    const box = el('div', { class: 'case' },
      el('div', { class: 'grid2' }, el('label', {}, '输入', i), el('label', {}, '期望输出', o)),
      el('div', { class: 'row' },
        el('label', { class: 'row' }, '分值', sc),
        el('label', { class: 'row' }, samp, '作为样例'),
        el('button', { class: 'danger', onclick: () => box.remove() }, '移除')));
    box._get = () => ({ input: i.value, output: o.value, score: Number(sc.value), is_sample: samp.checked });
    list.append(box);
  };
  cases.forEach((c) => addCase(c.input, c.output, c.score, c.is_sample));
  if (!cases.length) addCase();

  showModal(`测试数据: ${title}`, [
    el('p', { class: 'muted' },
      '比对规则：忽略每行行尾空白与文件末尾空行，CRLF 等同 LF；其余严格匹配。'),
    el('div', { class: 'row' }, el('button', { onclick: () => addCase() }, '添加测试点')),
    list,
  ], [
    el('button', { onclick: closeModal }, '取消'),
    el('button', {
      class: 'primary',
      onclick: async () => {
        const data = [...list.children].map((c) => c._get()).filter((c) => c.input !== '' || c.output !== '');
        if (!data.length) return toast('请至少保留一个测试点', true);
        try {
          const r = await api(`/problems/${id}/testcases`, { method: 'PUT', body: JSON.stringify(data) });
          closeModal(); viewProblems(); toast(`已保存 ${r.count} 个测试点`);
        } catch (e) { toast(e.message, true); }
      },
    }, '保存'),
  ]);
}

// ------------------------------------------------------------- monitor view

function viewMonitor() {
  if (!S.examId) return setMain(el('div', { class: 'empty' }, '请先在「考试/练习」中选择一场。'));
  const sessionsBox = el('div', { class: 'panel' }, el('h2', {}, '在线学生'));
  const eventsBox = el('div', { class: 'panel' }, el('h2', {}, '异常行为事件'));
  setMain(sessionsBox, eventsBox);

  const tick = async () => {
    if (S.view !== 'monitor') return;
    try {
      const [sessions, events] = await Promise.all([
        api(`/exams/${S.examId}/sessions`),
        api(`/exams/${S.examId}/events?limit=100`),
      ]);
      const online = sessions.filter((x) => x.online).length;
      sessionsBox.replaceChildren(
        el('div', { class: 'row' },
          el('h2', {}, '在线学生'),
          el('span', { class: 'muted' }, `${online} / ${sessions.length} 在线`)),
        sessions.length ? el('div', { class: 'scroll' }, el('table', {},
          el('thead', {}, el('tr', {},
            ...['', '学号', '姓名', '班级', '机器', 'IP', '最后心跳', '交卷'].map((h) => el('th', {}, h)))),
          el('tbody', {}, sessions.map((x) => el('tr', {},
            el('td', {}, el('span', { class: `dot ${x.online ? 'on' : 'off'}`,
              title: x.online ? '在线' : `失联 ${fmtDur(x.silent_for_ms)}` })),
            el('td', {}, x.student_no), el('td', {}, x.name), el('td', {}, x.class || '-'),
            el('td', {}, x.machine_name || '-'), el('td', {}, x.ip || '-'),
            el('td', {}, x.online ? '正常' : el('span', { class: 'warn' }, `失联 ${fmtDur(x.silent_for_ms)}`)),
            el('td', {}, x.submitted_at ? fmtTime(x.submitted_at) : '-'),
          )))))
          : el('div', { class: 'empty' }, '还没有学生登录。'));

      eventsBox.replaceChildren(
        el('div', { class: 'row' }, el('h2', {}, '异常行为事件'),
          el('span', { class: 'muted' }, `共 ${events.length} 条（最新在前）`)),
        events.length ? el('div', { class: 'scroll' }, el('table', {},
          el('thead', {}, el('tr', {},
            ...['时间', '学号', '类型', '级别', '详情'].map((h) => el('th', {}, h)))),
          el('tbody', {}, events.map((v) => el('tr', {},
            el('td', {}, fmtTime(v.server_time)), el('td', {}, v.student_no || '-'),
            el('td', {}, v.type),
            el('td', {}, el('span', {
              class: v.severity === 'alert' ? 'err' : v.severity === 'warn' ? 'warn' : 'muted',
            }, v.severity)),
            el('td', {}, JSON.stringify(v.payload || {})),
          )))))
          : el('div', { class: 'empty' }, '暂无事件。行为监控将在 M3 阶段接入更多检测项。'));
    } catch (e) { toast(e.message, true); }
  };
  tick();
  S.timer = setInterval(tick, 5000);
}

// ---------------------------------------------------------- submission view

async function viewSubmissions() {
  if (!S.examId) return setMain(el('div', { class: 'empty' }, '请先在「考试/练习」中选择一场。'));
  const kindSel = el('select', {},
    el('option', { value: 'final' }, '仅终稿提交'),
    el('option', { value: '' }, '全部(含定时快照)'),
    el('option', { value: 'snapshot' }, '仅定时快照'));
  const box = el('div', { class: 'panel' });
  setMain(box);

  const load = async () => {
    const rows = await api(`/exams/${S.examId}/submissions?kind=${kindSel.value}&limit=500`);
    box.replaceChildren(
      el('div', { class: 'row' }, el('h2', {}, '提交记录'), kindSel,
        el('button', { onclick: load }, '刷新')),
      rows.length ? el('div', { class: 'scroll' }, el('table', {},
        el('thead', {}, el('tr', {},
          ...['时间', '学号', '姓名', '题目', '语言', '类型', '#', '结果', '得分', '耗时', ''].map((h) => el('th', {}, h)))),
        el('tbody', {}, rows.map((r) => el('tr', {},
          el('td', {}, fmtTime(r.created_at)), el('td', {}, r.student_no), el('td', {}, r.name),
          el('td', {}, '#' + r.problem_id), el('td', {}, r.language),
          el('td', {}, r.kind === 'final' ? '终稿' : '快照'),
          el('td', { class: 'num' }, r.seq),
          el('td', {}, verdictBadge(r.verdict)),
          el('td', { class: 'num' }, r.max_score ? `${r.score}/${r.max_score}` : '-'),
          el('td', { class: 'num' }, r.time_ms ? r.time_ms + 'ms' : '-'),
          el('td', {}, el('button', { onclick: () => openSubmission(r.id) }, '查看')),
        )))))
        : el('div', { class: 'empty' }, '暂无提交。'));
  };
  kindSel.addEventListener('change', load);
  await load();
}

async function openSubmission(id) {
  const d = await api(`/submissions/${id}`);
  const r = d.result;
  const caseTable = r && r.cases && r.cases.length
    ? el('div', { class: 'scroll' }, el('table', {},
        el('thead', {}, el('tr', {},
          ...['#', '结果', '得分', '耗时', '内存', '说明'].map((h) => el('th', {}, h)))),
        el('tbody', {}, r.cases.map((c) => el('tr', {},
          el('td', { class: 'num' }, c.index), el('td', {}, verdictBadge(c.verdict)),
          el('td', { class: 'num' }, `${c.score}/${c.max_score}`),
          el('td', { class: 'num' }, c.time_ms + 'ms'),
          el('td', { class: 'num' }, c.memory_kb ? c.memory_kb + 'KB' : '-'),
          el('td', {}, c.message || ''),
        )))))
    : null;

  showModal(`提交 #${id} · ${d.student_no} · ${d.problem_title || '#' + d.problem_id}`, [
    el('div', { class: 'row' },
      el('span', { class: 'tag' }, d.language),
      el('span', { class: 'tag' }, d.kind === 'final' ? '终稿' : '快照'),
      el('span', { class: 'tag' }, fmtTime(d.created_at)),
      r ? verdictBadge(r.verdict) : verdictBadge('PD'),
      r ? el('span', { class: 'muted' }, `${r.score}/${r.max_score} 分 · ${r.time_ms}ms`) : null,
      el('span', { class: 'muted' }, `版本留痕 ${d.timeline.length} 次`)),
    r && r.compile_log ? el('div', {}, el('h3', {}, '编译/系统日志'),
      el('pre', { class: 'code' }, r.compile_log)) : null,
    caseTable ? el('div', {}, el('h3', {}, '测试点详情'), caseTable) : null,
    el('div', {}, el('h3', {}, '代码'), el('pre', { class: 'code' }, d.code)),
  ], [
    el('button', {
      onclick: async () => {
        try { await api(`/submissions/${id}/judge`, { method: 'POST' }); toast('已加入判题队列'); closeModal(); }
        catch (e) { toast(e.message, true); }
      },
    }, '重新判题'),
    el('button', { onclick: closeModal }, '关闭'),
  ]);
}

// --------------------------------------------------------------- score view

async function viewScores() {
  if (!S.examId) return setMain(el('div', { class: 'empty' }, '请先在「考试/练习」中选择一场。'));
  const d = await api(`/exams/${S.examId}/scores`);
  const head = el('tr', {},
    el('th', {}, '班级'), el('th', {}, '学号'), el('th', {}, '姓名'),
    ...d.problems.map((p) => el('th', { class: 'num', title: p.title },
      `第${p.idx}题 (${p.score})`)),
    el('th', { class: 'num' }, `总分 (${d.total_score})`));

  const body = d.rows.map((row) => el('tr', {},
    el('td', {}, row.class || '-'), el('td', {}, row.student_no), el('td', {}, row.name),
    ...d.problems.map((p) => {
      const c = row.cells[String(p.problem_id)];
      return el('td', { class: 'num' }, c
        ? el('span', { class: `v v-${c.verdict}`, title: `${c.verdict} ${c.case_score}/${c.case_max}` }, c.scaled)
        : '-');
    }),
    el('td', { class: 'num' }, el('strong', {}, row.total))));

  setMain(el('div', { class: 'panel' },
    el('div', { class: 'row' },
      el('h2', {}, `成绩: ${d.exam_name}`),
      el('button', { onclick: viewScores }, '刷新'),
      el('a', { href: `/api/teacher/exams/${S.examId}/scores.csv?t=${encodeURIComponent(S.token)}` },
        el('button', { class: 'primary' }, '导出 CSV (Excel)'))),
    el('p', { class: 'muted' },
      '单元格为折算到试卷分值后的得分；鼠标悬停可见原始测试点得分。只统计终稿提交中得分最高的一次。'),
    d.rows.length
      ? el('div', { class: 'scroll' }, el('table', {}, el('thead', {}, head), el('tbody', {}, body)))
      : el('div', { class: 'empty' }, '还没有学生登录或提交。')));
}

// -------------------------------------------------------------- system view

async function viewSystem() {
  const sys = await api('/system');

  // Built step by step rather than as one deep el() nest: the nested form is
  // easy to get wrong by a single paren and hard to read afterwards.
  const langRow = (label, path, ok) => el('tr', {},
    el('td', {}, label),
    el('td', {}, ok ? el('span', { class: 'ok' }, '可用') : el('span', { class: 'err' }, '未找到')),
    el('td', { class: 'muted' }, path || '-'));

  const addrTable = el('table', {}, el('tbody', {},
    (sys.local_ips || []).map((ip) => el('tr', {}, el('td', {}, `${ip}:${sys.port}`)))));

  const connectPanel = el('div', { class: 'panel' },
    el('h2', {}, '学生端连接信息'),
    el('p', {}, '学生端会自动发现本机；若网络禁止广播，请让学生手动输入下列地址之一：'),
    el('div', { class: 'scroll' }, addrTable),
    el('p', { class: 'muted' },
      `自动发现端口 UDP ${sys.discovery_port} · 判题线程 ${sys.workers} · 判题队列 ${sys.judge_queue}`));

  const ov = {
    gxx: el('input', { value: sys.override.gxx || '', placeholder: '留空则自动探测' }),
    gcc: el('input', { value: sys.override.gcc || '', placeholder: '留空则自动探测' }),
    python: el('input', { value: sys.override.python || '', placeholder: '留空则自动探测' }),
  };

  const saveToolchain = async () => {
    try {
      await api('/system/toolchain', {
        method: 'PUT',
        body: JSON.stringify({
          gxx: ov.gxx.value.trim(),
          gcc: ov.gcc.value.trim(),
          python: ov.python.value.trim(),
        }),
      });
      toast('已保存');
      viewSystem();
    } catch (e) {
      toast(e.message, true);
    }
  };

  const toolTable = el('table', {},
    el('thead', {}, el('tr', {},
      el('th', {}, '语言'), el('th', {}, '状态'), el('th', {}, '实际使用的路径'))),
    el('tbody', {},
      langRow('C++ (g++)', sys.toolchain.gxx, sys.available.cpp),
      langRow('C (gcc)', sys.toolchain.gcc, sys.available.c),
      langRow('Python', sys.toolchain.python, sys.available.python)));

  const judgePanel = el('div', { class: 'panel' },
    el('h2', {}, '判题环境'),
    el('div', { class: 'scroll' }, toolTable),
    el('p', { class: 'muted' },
      '建议让这里的编译器版本与机房 IDE 保持一致，否则会出现「学生本地能编译、服务端编译错误」。'),
    el('div', { class: 'grid2' },
      el('label', {}, 'g++ 路径', ov.gxx),
      el('label', {}, 'gcc 路径', ov.gcc),
      el('label', {}, 'python 路径', ov.python)),
    el('div', { class: 'row' },
      el('button', { class: 'primary', onclick: saveToolchain }, '保存编译器路径')));

  const roster = el('textarea', { rows: '8', placeholder: '每行一个：学号,姓名,班级' });

  const importRoster = async () => {
    if (!roster.value.trim()) return toast('请先粘贴名单', true);
    try {
      const r = await api('/students/import', {
        method: 'POST',
        body: JSON.stringify({
          csv: roster.value,
          batch: new Date().toISOString().slice(0, 10),
        }),
      });
      toast(`已导入 ${r.imported} 人`);
      if (r.warnings && r.warnings.length) {
        alert('以下行被跳过:\n' + r.warnings.join('\n'));
      }
      roster.value = '';
    } catch (e) {
      toast(e.message, true);
    }
  };

  const showRoster = async () => {
    const list = await api('/students');
    const table = el('table', {},
      el('thead', {}, el('tr', {},
        el('th', {}, '班级'), el('th', {}, '学号'), el('th', {}, '姓名'))),
      el('tbody', {}, list.map((s) => el('tr', {},
        el('td', {}, s.class || '-'),
        el('td', {}, s.student_no),
        el('td', {}, s.name)))));
    showModal(`名单 (${list.length} 人)`, [
      list.length ? el('div', { class: 'scroll' }, table)
                  : el('div', { class: 'empty' }, '名单为空。'),
    ], [el('button', { onclick: closeModal }, '关闭')]);
  };

  const rosterPanel = el('div', { class: 'panel' },
    el('h2', {}, '学生名单'),
    el('p', { class: 'muted' },
      '导入后，勾选了「名单校验」的考试会要求学号和姓名都与名单一致才能入场。可直接从 Excel 复制粘贴。'),
    roster,
    el('div', { class: 'row' },
      el('button', { class: 'primary', onclick: importRoster }, '导入名单'),
      el('button', { onclick: showRoster }, '查看名单')));

  setMain(connectPanel, judgePanel, rosterPanel);
}

bootGate();
