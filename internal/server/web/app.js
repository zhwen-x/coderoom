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
  system: null,     // /system payload, incl. the server's policy defaults
  stream: null,     // EventSource for the invigilation feed
  focusStudent: '', // drilled-down student in the monitor view
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
  // The policy defaults come from the server, so "restore defaults" restores
  // what the server would actually have chosen rather than a copy that drifts.
  try { S.system = await api('/system'); } catch { S.system = null; }
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
  closeStream();
  render();
}

function render() {
  const views = {
    exams: viewExams, problems: viewProblems, templates: viewTemplates,
    monitor: viewMonitor, submissions: viewSubmissions, scores: viewScores,
    reconcile: viewReconcile, system: viewSystem,
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
    policyRow(e),
  );
  api(`/exams/${e.id}`).then((d) => {
    if (e.per_student_paper) {
      // Each student draws their own paper at login, so there is no single
      // paper to show here; the template preview is where the rules live.
      box.append(el('p', { class: 'muted' },
        '本场为「每人一卷」：每个学生登录时按组卷模板各抽一套。' +
        '要查看出题规则和模拟抽取结果，请到「组卷」中点「预览组卷」。'));
      return;
    }
    if (!d.items.length) {
      box.append(el('p', { class: 'warn' }, '⚠ 这场考试还没有题目。'));
      return;
    }
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

// ------------------------------------------------------------ monitor policy

// Enforcement is kept visually and textually apart from monitoring: these two
// change the student's machine, and the teacher has to understand that before
// ticking them, not after a room full of machines behaves oddly.
const ENFORCE_FIELDS = [
  ['block_usb_storage', '禁用 U 盘存储',
    '考试期间停止加载 USB 存储驱动。已经插着的 U 盘不会被弹出（驱动已加载），拔掉后才无法再用。'],
  ['block_internet', '禁止访问外网',
    '只封锁公网地址；局域网全部放通，判题服务端、极域、文件共享照常可用。'],
];

const MONITOR_FIELDS = [
  ['monitor_usb', 'U盘/存储设备', '出现或移走一个可用的驱动器盘符时上报'],
  ['monitor_process', '黑名单进程', '浏览器、聊天工具、远程控制软件在运行就上报'],
  ['monitor_foreground', '前台窗口', `切到非白名单窗口并停留 20 秒以上才上报，并在看板显示当前前台程序`],
  ['monitor_paste', '粘贴留痕', '记录每次粘贴的字符数与行数（代码靠粘贴录入，不能禁用剪贴板）'],
  ['monitor_network', '外网连接', '枚举 TCP 连接，发现通往公网的目标就上报；噪音最大，默认关闭'],
];

function policyRow(e) {
  const p = e.policy || {};
  const on = MONITOR_FIELDS.filter(([k]) => p[k]).map(([, label]) => label);
  const blocking = ENFORCE_FIELDS.filter(([k]) => p[k]).map(([, label]) => label);
  return el('div', {},
    el('div', { class: 'row' },
      el('strong', {}, '监控策略'),
      on.length ? el('span', {}, on.join(' · ')) : el('span', { class: 'muted' }, '全部关闭'),
      el('span', { class: 'muted' }, `v${e.policy_version || 1}`),
      el('button', { onclick: () => openPolicyEditor(e) }, '编辑')),
    el('div', { class: 'row' },
      el('strong', {}, '强制管控'),
      blocking.length
        ? el('span', { class: 'err' }, blocking.join(' · '))
        : el('span', { class: 'muted' }, '未启用'),
      blocking.length
        ? el('button', {
            class: 'danger',
            title: '立即让全班学生机恢复 U 盘与网络',
            onclick: async () => {
              if (!confirm(
                `立即解除「${e.name}」的全部强制管控？\n\n` +
                '所有学生机会在下一次心跳（默认 10 秒内）恢复 U 盘与网络访问。\n' +
                '监控不受影响，考试可以继续。')) return;
              try {
                const r = await api(`/exams/${e.id}/release-guard`, { method: 'POST' });
                await refreshExams(); render();
                toast(r.message || '已解除');
              } catch (err) { toast(err.message, true); }
            },
          }, '紧急解除管控')
        : null));
}

function openPolicyEditor(e) {
  const p = JSON.parse(JSON.stringify(e.policy || {}));
  const makeBox = ([key, label, hint], strong) => {
    const cb = el('input', { type: 'checkbox' });
    cb.checked = !!p[key];
    cb._key = key;
    return el('div', {},
      el('label', { class: 'row' }, cb,
        el('strong', { class: strong ? 'err' : null }, label)),
      el('div', { class: 'muted small' }, hint));
  };
  const boxes = MONITOR_FIELDS.map((f) => makeBox(f, false));
  const enforceBoxes = ENFORCE_FIELDS.map((f) => makeBox(f, true));
  // Not one of the enforcement switches: it does not change what is enforced,
  // only which recovery layer is worth installing in this room.
  const restoreBox = el('input', { type: 'checkbox' });
  restoreBox.checked = !!p.reboot_restores_system;

  const black = el('textarea', { rows: '6' });
  black.value = (p.process_blacklist || []).join('\n');
  const white = el('textarea', { rows: '6' });
  white.value = (p.process_whitelist || []).join('\n');
  const snap = el('input', { type: 'number', value: p.snapshot_interval_sec || 30, min: '10', max: '600' });
  const beat = el('input', { type: 'number', value: p.heartbeat_interval_sec || 10, min: '5', max: '120' });

  const lines = (ta) => ta.value.split('\n').map((x) => x.trim()).filter(Boolean);

  showModal(`监控策略: ${e.name}`, [
    el('p', { class: 'muted' },
      '保存后策略版本号 +1。学生端在下一次心跳（默认 10 秒内）自己把新策略拉下来——' +
      '包括保存那一刻还没开机的机器，和之后才重启的机器。不需要逐台操作。'),
    el('div', { class: 'grid2' }, boxes),
    el('hr'),
    el('div', { class: 'grid2' },
      el('label', {}, '黑名单进程（每行一个，可省略 .exe）', black),
      el('label', {}, '白名单进程（每行一个，不上报；黑白名单冲突时白名单优先）', white)),
    el('p', { class: 'muted small' },
      '白名单默认含极域电子教室与机房 IDE：学生本来就该待在 IDE 里，' +
      '把它当异常会把真正的告警淹掉。'),
    el('div', { class: 'grid2' },
      el('label', {}, '代码快照间隔（秒）', snap),
      el('label', {}, '心跳间隔（秒）', beat)),
    el('hr'),
    el('div', { class: 'row' }, el('h3', {}, '强制管控'),
      el('span', { class: 'err' }, '会修改学生机的系统设置')),
    el('div', { class: 'grid2' }, enforceBoxes),
    el('p', { class: 'warn small' },
      '需要学生端以管理员身份运行。没有管理员权限的机器不会被管控，' +
      '并会在监考看板上标红——不会假装成功。'),
    el('div', {},
      el('label', { class: 'row' }, restoreBox, el('strong', {}, '机房重启会自动还原系统')),
      el('div', { class: 'muted small' },
        '装了还原卡 / 冰点还原 / 共享盘保护的机房勾选。勾选后不再创建"开机恢复"' +
        '计划任务——在这类机房里它本来也跑不起来：还原会把任务本身一起抹掉。' +
        '其余恢复手段全部保留（每分钟恢复任务、看门狗、正常释放、紧急解除、-restore）。' +
        '不确定就别勾，多装一个任务没有坏处。'),),
    el('p', { class: 'muted small' },
      '恢复有五重保障：考试结束/登出/退出自动恢复；客户端被强杀由看门狗恢复；' +
      '断电重启由开机计划任务恢复；教师可在上方「紧急解除管控」一键解除；' +
      '最后还可在学生机上运行 coderoom-client.exe -restore 手动恢复。' +
      '所有失败路径都是「恢复」，不会把机器锁死。'),
  ], [
    el('button', { onclick: closeModal }, '取消'),
    el('button', {
      onclick: () => {
        const d = ((S.system || {}).default_policy || {})[e.type] || {};
        [...boxes, ...enforceBoxes].forEach((b) => {
          const cb = b.querySelector('input');
          cb.checked = !!d[cb._key];
        });
        black.value = (d.process_blacklist || []).join('\n');
        white.value = (d.process_whitelist || []).join('\n');
        restoreBox.checked = !!d.reboot_restores_system;
      },
    }, '恢复默认'),
    el('button', {
      class: 'primary',
      onclick: async () => {
        const body = {
          snapshot_interval_sec: Number(snap.value),
          heartbeat_interval_sec: Number(beat.value),
          process_blacklist: lines(black),
          process_whitelist: lines(white),
          reboot_restores_system: restoreBox.checked,
        };
        [...boxes, ...enforceBoxes].forEach((b) => {
          const cb = b.querySelector('input');
          body[cb._key] = cb.checked;
        });
        try {
          const saved = await api(`/exams/${e.id}/policy`, {
            method: 'PUT', body: JSON.stringify(body),
          });
          closeModal();
          await refreshExams();
          render();
          toast(`策略已保存（v${saved.version}），学生端将在下次心跳时自动应用`);
        } catch (err) { toast(err.message, true); }
      },
    }, '保存'),
  ]);
}

async function openExamEditor() {
  const [problems, templates] = await Promise.all([api('/problems'), api('/templates')]);
  S.problems = problems;
  if (!problems.length) {
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

  // Problems come either from a hand-picked list or from a template. Only a
  // template can produce a different paper per student, because each draw
  // happens when that student logs in.
  const source = el('select', {},
    el('option', { value: 'manual' }, '手动选题（全班同一套卷）'),
    el('option', { value: 'template', disabled: !templates.length },
      templates.length ? '使用组卷模板' : '使用组卷模板（还没有模板）'));
  const tplSel = el('select', {}, ...templates.map((t) =>
    el('option', { value: t.id }, `${t.name}（${t.items.length} 题 / ${t.total_score} 分）`)));
  const perStudent = el('input', { type: 'checkbox' });

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

  const manualWrap = el('div', {},
    el('div', { class: 'row' }, el('h3', {}, '试卷题目'),
      el('button', { onclick: () => addPick() }, '添加题目')),
    picks);
  const templateWrap = el('div', {},
    el('label', {}, '组卷模板', tplSel),
    el('label', { class: 'row' }, perStudent, '每人一卷（每个学生登录时各抽一套）'),
    el('p', { class: 'muted small' },
      '不勾选则按模板生成一套全班共用的卷。建议先在「组卷」里点「预览组卷」确认能正常出题。'));

  const syncSource = () => {
    manualWrap.hidden = source.value !== 'manual';
    templateWrap.hidden = source.value !== 'template';
  };
  source.addEventListener('change', syncSource);
  syncSource();

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
    el('label', {}, '题目来源', source),
    manualWrap,
    templateWrap,
  ], [
    el('button', { onclick: closeModal }, '取消'),
    el('button', {
      class: 'primary',
      onclick: async () => {
        if (!name.value.trim()) return toast('请填写名称', true);
        const useTemplate = source.value === 'template';
        const items = useTemplate
          ? []
          : [...picks.children].map((r) => r._get()).filter((x) => x.problem_id);
        if (!useTemplate && !items.length) return toast('请至少添加一道题目', true);
        if (useTemplate && !tplSel.value) return toast('请选择组卷模板', true);
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
              paper_template_id: useTemplate ? Number(tplSel.value) : null,
              per_student_paper: useTemplate && perStudent.checked,
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

function closeStream() {
  if (S.stream) { S.stream.close(); S.stream = null; }
}

const EVENT_LABEL = {
  usb_inserted: '插入存储设备', usb_removed: '移除存储设备',
  process_blacklisted: '运行黑名单程序', foreground_lost: '切出本窗口',
  paste_large: '大段粘贴', paste: '粘贴',
  external_network: '连接外网', clock_skew: '本机时间异常',
  login_conflict: '同学号异机登录', heartbeat_lost: '客户端失联',
  client_started: '客户端启动', client_exiting: '客户端退出',
  monitor_degraded: '监控项不可用',
};
const DRIVE_KIND = {
  removable: '可移动磁盘', cdrom: '光驱', fixed: '固定磁盘',
  remote: '网络驱动器', ramdisk: '内存盘', unknown: '未知',
};
const SEV_CLASS = { alert: 'err', warn: 'warn', info: 'muted' };
const secs = (ms) => `${Math.round((ms || 0) / 1000)} 秒`;

// Events are rendered as sentences, not as JSON. A dashboard is read while
// walking around a room: "F: 可移动磁盘" is actionable at a glance and
// {"drive":"F:","kind":"removable"} is not.
function describeEvent(ev) {
  const p = ev.payload || {};
  switch (ev.type) {
    case 'usb_inserted':
    case 'usb_removed':
      return `${p.drive || '?'} ${DRIVE_KIND[p.kind] || p.kind || ''}`;
    case 'process_blacklisted':
      return `${p.process || '?'}${p.pid ? ` (PID ${p.pid})` : ''}`;
    case 'foreground_lost':
      if (!p.process) return '本窗口失去焦点';
      return `${p.process}${p.title ? ` — ${p.title}` : ''}` +
        `${p.dwell_ms ? `，停留 ${secs(p.dwell_ms)}` : ''}`;
    case 'paste':
    case 'paste_large':
      return `${p.chars || 0} 字符 / ${p.lines || 0} 行`;
    case 'external_network':
      return `${p.process || '?'} → ${p.remote || '?'}`;
    case 'clock_skew':
      return `与服务端相差 ${secs(p.skew_ms)}`;
    case 'login_conflict':
      return `原机器 ${p.previous_machine || '?'} → 现机器 ${p.current_machine || '?'}`;
    case 'heartbeat_lost':
      return `已失联 ${secs(p.silent_ms)}${p.machine ? `，机器 ${p.machine}` : ''}`;
    case 'monitor_degraded':
      return `${p.check || '?'}：${p.reason || '不可用'}`;
    case 'client_started':
      return `机器 ${p.machine || '?'}${p.ip ? ` (${p.ip})` : ''}`;
    case 'client_exiting':
      return '学生正常关闭了客户端';
    default: {
      const keys = Object.keys(p);
      return keys.length ? JSON.stringify(p) : '';
    }
  }
}

function eventRow(ev) {
  return el('tr', { class: ev.severity === 'alert' ? 'row-alert' : null },
    el('td', {}, fmtTime(ev.server_time)),
    el('td', {},
      el('a', {
        class: 'link',
        title: '只看这名学生',
        onclick: () => { S.focusStudent = ev.student_no; render(); },
      }, ev.student_no || '-')),
    el('td', {}, EVENT_LABEL[ev.type] || ev.type),
    el('td', {}, el('span', { class: SEV_CLASS[ev.severity] || 'muted' },
      ev.severity === 'alert' ? '告警' : ev.severity === 'warn' ? '注意' : '信息')),
    el('td', {}, describeEvent(ev)));
}

function viewMonitor() {
  if (!S.examId) return setMain(el('div', { class: 'empty' }, '请先在「考试/练习」中选择一场。'));

  const sessionsBox = el('div', { class: 'panel' }, el('h2', {}, '在线学生'));
  const eventsBox = el('div', { class: 'panel' }, el('h2', {}, '行为事件'));
  const feedState = el('span', { class: 'muted' }, '连接中…');
  setMain(sessionsBox, eventsBox);

  // The event table is append-only in the UI: the stream delivers only rows
  // newer than the cursor, and they are prepended. Nothing the teacher is
  // currently reading gets rebuilt under their cursor, which is the reason
  // this view is a stream rather than a poll.
  const tbody = el('tbody', {});
  const shown = new Set();
  let total = 0;

  const addEvents = (rows, { prepend }) => {
    const fresh = rows.filter((ev) => !shown.has(ev.id));
    if (!fresh.length) return;
    fresh.forEach((ev) => shown.add(ev.id));
    total += fresh.length;
    const nodes = fresh.map(eventRow);
    if (prepend) tbody.prepend(...nodes.reverse());
    else tbody.append(...nodes);
    // Bound the DOM: an exam can run for hours and nobody scrolls past a
    // few hundred rows.
    while (tbody.children.length > 500) tbody.lastChild.remove();
    renderEventsHeader();
  };

  const renderEventsHeader = () => {
    const title = S.focusStudent ? `行为事件 · 仅 ${S.focusStudent}` : '行为事件';
    setKids(header,
      el('h2', {}, title),
      el('span', { class: 'muted' }, `${total} 条`),
      feedState,
      S.focusStudent
        ? el('button', { onclick: () => { S.focusStudent = ''; render(); } }, '看全部')
        : null);
  };
  const header = el('div', { class: 'row' });

  eventsBox.replaceChildren(header,
    el('div', { class: 'scroll tall' }, el('table', {},
      el('thead', {}, el('tr', {},
        ...['时间', '学号', '类型', '级别', '详情'].map((h) => el('th', {}, h)))),
      tbody)));
  renderEventsHeader();

  const renderSessions = (sessions) => {
    const online = sessions.filter((x) => x.online).length;
    const alerts = sessions.filter((x) => (x.events || {}).alert > 0).length;
    setKids(sessionsBox,
      el('div', { class: 'row' },
        el('h2', {}, '在线学生'),
        el('span', { class: 'muted' }, `${online} / ${sessions.length} 在线`),
        alerts ? el('span', { class: 'err' }, `${alerts} 人有告警`) : null),
      sessions.length ? el('div', { class: 'scroll' }, el('table', {},
        el('thead', {}, el('tr', {},
          ...['', '学号', '姓名', '班级', '机器', '当前前台', '管控', '事件', '最后心跳', '交卷']
            .map((h) => el('th', {}, h)))),
        el('tbody', {}, sessions.map((x) => el('tr', {
          class: S.focusStudent === x.student_no ? 'row-sel' : null,
          onclick: () => {
            S.focusStudent = S.focusStudent === x.student_no ? '' : x.student_no;
            render();
          },
          title: '点击只看这名学生的事件',
        },
          el('td', {}, el('span', {
            class: `dot ${x.online ? 'on' : 'off'}`,
            title: x.online ? '在线' : `失联 ${fmtDur(x.silent_for_ms)}`,
          })),
          el('td', {}, x.student_no), el('td', {}, x.name), el('td', {}, x.class || '-'),
          el('td', {}, x.machine_name || '-'),
          el('td', {}, foregroundCell(x)),
          el('td', {}, guardCell(x)),
          el('td', {}, tallyCell(x.events)),
          el('td', {}, x.online ? '正常'
            : el('span', { class: 'warn' }, `失联 ${fmtDur(x.silent_for_ms)}`)),
          el('td', {}, x.submitted_at ? fmtTime(x.submitted_at) : '-'),
        )))))
        : el('div', { class: 'empty' }, '还没有学生登录。'));
  };

  // Initial page, then the stream takes over from that cursor. Doing it in
  // this order means no gap: anything logged between the two is delivered by
  // the stream, because the cursor is the id of the newest row already shown.
  (async () => {
    try {
      const [sessions, events] = await Promise.all([
        api(`/exams/${S.examId}/sessions`),
        api(`/exams/${S.examId}/events?limit=200` +
          (S.focusStudent ? `&student=${encodeURIComponent(S.focusStudent)}` : '')),
      ]);
      renderSessions(sessions);
      addEvents(events, { prepend: false }); // already newest-first
      const cursor = events.length ? Math.max(...events.map((e) => e.id)) : 0;
      openStream(cursor, renderSessions, addEvents, feedState);
    } catch (e) { toast(e.message, true); }
  })();
}

function foregroundCell(x) {
  // For a student who has gone silent this is the last thing seen rather than
  // the current one, so it is shown muted with the tense spelled out - the
  // 最后心跳 column next to it says how stale.
  if (!x.online) {
    return x.foreground
      ? el('span', { class: 'muted', title: '失联前最后所在窗口' }, `${x.foreground}（失联前）`)
      : el('span', { class: 'muted' }, '-');
  }
  // Our own window is the one place the student is meant to be, so it reads
  // as a state rather than as a process name nobody needs to parse.
  if (!x.foreground || /^coderoom-client/i.test(x.foreground)) {
    return el('span', { class: 'muted' }, x.focused ? '本窗口' : '未知');
  }
  return el('span', { class: x.focused ? 'muted' : 'warn' }, x.foreground);
}

// guardCell shows what the machine reports it is enforcing, which is the only
// thing that counts. A policy saying "USB disabled" tells the teacher what was
// asked for; this column tells them whether it happened.
function guardCell(x) {
  const g = x.guard;
  if (!g) return el('span', { class: 'muted' }, '-');
  if (!g.supported) {
    return el('span', { class: 'err', title: g.error || '该机无法执行管控' }, '无法管控');
  }
  if (g.error) {
    return el('span', { class: 'err', title: g.error }, '管控出错');
  }
  if (!g.active) return el('span', { class: 'muted' }, '未管控');
  const parts = [];
  if (g.usb_blocked) parts.push('禁U盘');
  if (g.internet_blocked) parts.push('断外网');
  return el('span', { class: 'ok' }, parts.join(' · '));
}

function tallyCell(t) {
  const tally = t || {};
  if (!tally.alert && !tally.warn) return el('span', { class: 'muted' }, '-');
  return el('span', {},
    tally.alert ? el('span', { class: 'err' }, `告警 ${tally.alert}`) : null,
    tally.alert && tally.warn ? ' · ' : null,
    tally.warn ? el('span', { class: 'warn' }, `注意 ${tally.warn}` ) : null);
}

// openStream subscribes to the server's SSE feed. EventSource cannot set a
// request header, so the admin token rides in the query string - the same way
// the console's own URL carries it.
function openStream(cursor, renderSessions, addEvents, feedState) {
  closeStream();
  const url = `/api/teacher/exams/${S.examId}/stream` +
    `?since=${cursor}&t=${encodeURIComponent(S.token)}`;
  const es = new EventSource(url);
  S.stream = es;

  es.addEventListener('open', () => { feedState.textContent = '实时'; feedState.className = 'ok'; });
  es.addEventListener('sessions', (m) => {
    if (S.view !== 'monitor') return;
    try { renderSessions(JSON.parse(m.data)); } catch { /* malformed frame */ }
  });
  es.addEventListener('events', (m) => {
    if (S.view !== 'monitor') return;
    try {
      let rows = JSON.parse(m.data);
      if (S.focusStudent) rows = rows.filter((e) => e.student_no === S.focusStudent);
      addEvents(rows, { prepend: true });
      const fresh = rows.filter((e) => e.severity === 'alert');
      if (fresh.length) {
        toast(`${fresh[0].student_no} ${EVENT_LABEL[fresh[0].type] || fresh[0].type}`, true);
      }
    } catch { /* malformed frame */ }
  });
  // The browser reconnects on its own; all this has to do is stop claiming to
  // be live, so a dead feed is never mistaken for a quiet room.
  es.addEventListener('error', () => {
    feedState.textContent = '连接中断，正在重连…';
    feedState.className = 'warn';
  });
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
  const std = {
    cpp: el('select', {}, ...['c++98', 'c++11', 'c++14', 'c++17', 'c++20'].map((v) =>
      el('option', { value: v, selected: v === sys.options.cpp_std }, v))),
    c: el('select', {}, ...['c89', 'c99', 'c11', 'c17'].map((v) =>
      el('option', { value: v, selected: v === sys.options.c_std }, v))),
  };

  const saveToolchain = async () => {
    try {
      await api('/system/toolchain', {
        method: 'PUT',
        body: JSON.stringify({
          gxx: ov.gxx.value.trim(),
          gcc: ov.gcc.value.trim(),
          python: ov.python.value.trim(),
          cpp_std: std.cpp.value,
          c_std: std.c.value,
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
    ...(sys.warnings || []).map((w) => el('p', { class: 'err' }, '⚠ ' + w)),
    el('div', { class: 'scroll' }, toolTable),
    el('div', { class: 'grid2' },
      el('label', {}, 'g++ 路径', ov.gxx),
      el('label', {}, 'gcc 路径', ov.gcc),
      el('label', {}, 'python 路径', ov.python),
      el('label', {}, 'C++ 标准', std.cpp),
      el('label', {}, 'C 标准', std.c)),
    el('p', { class: 'muted' },
      '机房使用 Visual Studio / Dev-C++ / PyCharm / IDLE。标准选得比机房 IDE 更旧是安全的；' +
      '选得更新则可能出现「学生本地能编译、服务端编译错误」。'),
    el('div', { class: 'row' },
      el('button', { class: 'primary', onclick: saveToolchain }, '保存判题设置')));

  const sandboxPanel = el('div', { class: 'panel' },
    el('h2', {}, '判题沙箱'),
    el('p', {}, sys.sandbox),
    el('p', { class: 'muted' },
      '沙箱防的是意外（死循环、内存耗尽、输出爆炸、误开子进程），不防刻意编写的恶意代码。' +
      '考试场景下每份提交都与学号绑定、可追责。'));

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

  setMain(connectPanel, judgePanel, sandboxPanel, rosterPanel);
}

bootGate();

// ------------------------------------------------------------ 组卷模板视图

const PICK_LABEL = { fixed: '指定题目', pool: '从题目池抽取', filter: '按条件抽取' };

async function viewTemplates() {
  const [templates, problems] = await Promise.all([api('/templates'), api('/problems')]);
  S.problems = problems;

  const rows = templates.map((t) => el('tr', {},
    el('td', {}, t.name),
    el('td', { class: 'num' }, t.items.length + ' 题'),
    el('td', { class: 'num' }, t.total_score + ' 分'),
    el('td', {}, t.items.map((it) => PICK_LABEL[it.pick_type] || it.pick_type).join(' / ')),
    el('td', { class: 'row' },
      el('button', { onclick: () => openTemplateEditor(t) }, '编辑'),
      el('button', { onclick: () => previewTemplate(t.id) }, '预览组卷'),
      el('button', {
        onclick: async () => {
          try {
            await api(`/templates/${t.id}/reroll`, { method: 'POST' });
            toast('已重新随机，之后生成的试卷会换一批题目');
          } catch (e) { toast(e.message, true); }
        },
      }, '重新随机'),
      el('button', {
        class: 'danger',
        onclick: async () => {
          if (!confirm(`删除组卷模板「${t.name}」？已据此生成的试卷不受影响。`)) return;
          try { await api(`/templates/${t.id}`, { method: 'DELETE' }); viewTemplates(); toast('已删除'); }
          catch (e) { toast(e.message, true); }
        },
      }, '删除')),
  ));

  setMain(el('div', { class: 'panel' },
    el('div', { class: 'row' },
      el('h2', {}, '组卷模板'),
      el('button', { class: 'primary', onclick: () => openTemplateEditor(null) }, '新建模板'),
      el('button', { onclick: viewTemplates }, '刷新')),
    el('p', { class: 'muted' },
      '模板描述的是「怎么出题」而不是具体题目。创建考试时选用模板，可以生成全班同一套卷，' +
      '也可以为每个学生各抽一套。每题位的难度固定，所以不同学生的成绩仍然可比。'),
    templates.length
      ? el('div', { class: 'scroll' }, el('table', {},
          el('thead', {}, el('tr', {},
            ...['名称', '题数', '总分', '取题方式', '操作'].map((h) => el('th', {}, h)))),
          el('tbody', {}, rows)))
      : el('div', { class: 'empty' }, '还没有组卷模板。固定题目的考试可以直接在「考试/练习」里手动选题。')));
}

// templateItemRow builds the editor for one slot of a template.
function templateItemRow(item, onRemove) {
  const pick = el('select', {}, ...Object.entries(PICK_LABEL).map(([v, label]) =>
    el('option', { value: v, selected: v === item.pick_type }, label)));
  const score = el('input', { type: 'number', value: item.score || 10, min: '1' });

  const fixedSel = el('select', {}, ...S.problems.map((p) =>
    el('option', { value: p.id, selected: p.id === item.fixed_problem_id },
      `#${p.id} ${p.title}${p.case_count ? '' : ' (无测试数据)'}`)));

  const poolBox = el('div', { class: 'picklist' });
  const poolChecks = S.problems.map((p) => {
    const cb = el('input', { type: 'checkbox' });
    cb.checked = (item.pool || []).includes(p.id);
    cb._pid = p.id;
    poolBox.append(el('label', { class: 'row' }, cb,
      `#${p.id} ${p.title}`,
      el('span', { class: 'tag' }, '难度 ' + p.difficulty),
      p.case_count ? null : el('span', { class: 'warn' }, '无测试数据')));
    return cb;
  });

  const f = item.filter || {};
  const minD = el('input', { type: 'number', value: f.min_difficulty || 1, min: '1', max: '5' });
  const maxD = el('input', { type: 'number', value: f.max_difficulty || 5, min: '1', max: '5' });
  const tags = el('input', { value: (f.tags || []).join(','), placeholder: '留空表示不限，逗号分隔' });
  const allTags = el('input', { type: 'checkbox' });
  allTags.checked = !!f.require_all_tags;

  const fixedWrap = el('div', {}, el('label', {}, '题目', fixedSel));
  const poolWrap = el('div', {},
    el('div', { class: 'muted small' }, '从下列题目中随机抽 1 道（适合针对某个知识点出题）'),
    poolBox);
  const filterWrap = el('div', {},
    el('div', { class: 'grid2' },
      el('label', {}, '最低难度', minD),
      el('label', {}, '最高难度', maxD),
      el('label', {}, '知识点标签', tags)),
    el('label', { class: 'row' }, allTags, '必须同时具备全部标签（默认命中任一即可）'));

  const sync = () => {
    fixedWrap.hidden = pick.value !== 'fixed';
    poolWrap.hidden = pick.value !== 'pool';
    filterWrap.hidden = pick.value !== 'filter';
  };
  pick.addEventListener('change', sync);
  sync();

  const box = el('div', { class: 'case' },
    el('div', { class: 'row' },
      el('span', { class: 'idx' }),
      el('label', { class: 'row' }, '取题方式', pick),
      el('label', { class: 'row' }, '分值', score),
      el('div', { class: 'spacer' }),
      el('button', { class: 'danger', onclick: () => { box.remove(); onRemove(); } }, '移除题位')),
    fixedWrap, poolWrap, filterWrap);

  box._get = () => ({
    pick_type: pick.value,
    score: Number(score.value),
    fixed_problem_id: pick.value === 'fixed' ? Number(fixedSel.value) : null,
    pool: pick.value === 'pool' ? poolChecks.filter((c) => c.checked).map((c) => c._pid) : [],
    filter: pick.value === 'filter' ? {
      min_difficulty: Number(minD.value),
      max_difficulty: Number(maxD.value),
      tags: tags.value.split(',').map((t) => t.trim()).filter(Boolean),
      require_all_tags: allTags.checked,
    } : {},
  });
  return box;
}

async function openTemplateEditor(existing) {
  if (!S.problems.length) S.problems = await api('/problems');
  if (!S.problems.length) return toast('请先在「题库」中创建题目', true);

  const name = el('input', { value: existing ? existing.name : '' });
  const list = el('div', { class: 'caselist' });
  const total = el('span', { class: 'muted' });

  const renumber = () => {
    [...list.children].forEach((box, i) => {
      box.querySelector('.idx').textContent = `第 ${i + 1} 题`;
    });
    const sum = [...list.children].reduce((n, b) => n + (b._get().score || 0), 0);
    total.textContent = `共 ${list.children.length} 题，总分 ${sum}`;
  };
  const addItem = (item = {}) => { list.append(templateItemRow(item, renumber)); renumber(); };

  (existing ? existing.items : [{ pick_type: 'filter', score: 50 }]).forEach(addItem);
  list.addEventListener('change', renumber);

  showModal(existing ? `编辑模板「${existing.name}」` : '新建组卷模板', [
    el('label', {}, '模板名称', name),
    el('div', { class: 'row' },
      el('h3', {}, '题位'),
      el('button', { onclick: () => addItem({ pick_type: 'filter', score: 10 }) }, '添加题位'),
      total),
    list,
  ], [
    el('button', { onclick: closeModal }, '取消'),
    el('button', {
      class: 'primary',
      onclick: async () => {
        if (!name.value.trim()) return toast('请填写模板名称', true);
        const items = [...list.children].map((b) => b._get());
        if (!items.length) return toast('请至少添加一个题位', true);
        const bad = items.find((it) => it.pick_type === 'pool' && !it.pool.length);
        if (bad) return toast('有题位选择了「从题目池抽取」但没有勾选任何题目', true);
        const body = { name: name.value.trim(), items };
        try {
          if (existing) {
            await api(`/templates/${existing.id}`, { method: 'PUT', body: JSON.stringify({ ...body, id: existing.id }) });
          } else {
            await api('/templates', { method: 'POST', body: JSON.stringify(body) });
          }
          closeModal(); viewTemplates(); toast('已保存，建议点「预览组卷」确认能正常出题');
        } catch (e) { toast(e.message, true); }
      },
    }, '保存'),
  ]);
}

async function previewTemplate(id) {
  let p;
  try { p = await api(`/templates/${id}/preview`, { method: 'POST' }); }
  catch (e) { return toast(e.message, true); }

  const itemRows = p.items.map((it) => el('tr', {},
    el('td', { class: 'num' }, it.idx),
    el('td', {}, PICK_LABEL[it.pick_type] || it.pick_type),
    el('td', { class: 'num' }, it.score),
    el('td', {}, it.rule),
    el('td', { class: 'num' },
      it.candidates ? it.candidates : el('span', { class: 'err' }, '0 ⚠')),
    el('td', { class: 'muted small' }, it.samples.join('、')),
  ));

  showModal(`组卷预览：${p.name}（总分 ${p.total_score}）`, [
    p.error ? el('div', { class: 'err' }, '无法组卷：' + p.error) : null,
    ...p.warnings.map((w) => el('div', { class: 'warn' }, '⚠ ' + w)),
    !p.error && !p.warnings.length
      ? el('div', { class: 'ok' }, '✓ 模板可以正常组卷')
      : null,
    el('div', {}, el('h3', {}, '各题位的可选范围'),
      el('div', { class: 'scroll' }, el('table', {},
        el('thead', {}, el('tr', {},
          ...['题位', '取题方式', '分值', '规则', '可选题数', '示例题目'].map((h) => el('th', {}, h)))),
        el('tbody', {}, itemRows)))),
    p.samples.length ? el('div', {},
      el('h3', {}, '模拟抽取（不同学生会抽到不同题目）'),
      el('div', { class: 'scroll' }, el('table', {},
        el('tbody', {}, p.samples.map((s) => el('tr', {},
          el('td', { class: 'muted' }, s.student_no),
          el('td', {}, s.titles.join('　|　')))))))) : null,
  ], [el('button', { onclick: closeModal }, '关闭')]);
}

// ---------------------------------------------------------------- 对账视图

const RECONCILE_LABEL = {
  recoverable: { text: '有代码未提交', cls: 'err' },
  absent: { text: '缺考', cls: 'warn' },
  no_work: { text: '登录但无代码', cls: 'warn' },
  incomplete: { text: '部分未答', cls: 'muted' },
  complete: { text: '已完成', cls: 'ok' },
};

async function viewReconcile() {
  if (!S.examId) return setMain(el('div', { class: 'empty' }, '请先在「考试/练习」中选择一场。'));
  const d = await api(`/exams/${S.examId}/reconcile`);

  const summary = el('div', { class: 'row' },
    ...Object.entries(RECONCILE_LABEL)
      .filter(([k]) => d.summary[k])
      .map(([k, v]) => el('span', { class: 'tag' },
        el('span', { class: v.cls }, v.text), ` ${d.summary[k]} 人`)));

  const rows = d.rows.map((row) => {
    const label = RECONCILE_LABEL[row.status] || { text: row.status, cls: 'muted' };
    const cells = row.problems.map((p) => {
      if (p.has_final) return el('span', { class: 'tag ok' }, `第${p.idx}题 已交`);
      if (p.has_snapshot && p.snapshot_bytes > 0) {
        return el('button', {
          class: 'danger',
          title: `最后快照 ${fmtTime(p.snapshot_at)}，${p.snapshot_bytes} 字节`,
          onclick: async () => {
            if (!confirm(
              `用 ${row.name}(${row.student_no}) 第${p.idx}题的最后快照补交？\n\n` +
              `快照时间：${fmtTime(p.snapshot_at)}\n代码长度：${p.snapshot_bytes} 字节\n\n` +
              `补交的提交会标注来源，并记入操作日志。`)) return;
            try {
              const r = await api(`/sessions/${row.session_id}/promote`,
                { method: 'POST', body: JSON.stringify({ problem_id: p.problem_id }) });
              toast(r.message || '已补交');
              viewReconcile();
            } catch (e) { toast(e.message, true); }
          },
        }, `第${p.idx}题 补交`);
      }
      return el('span', { class: 'tag' }, `第${p.idx}题 无`);
    });
    return el('tr', {},
      el('td', {}, el('span', { class: label.cls }, label.text)),
      el('td', {}, row.class || '-'),
      el('td', {}, row.student_no),
      el('td', {}, row.name),
      el('td', {}, row.login_at ? fmtTime(row.login_at) : '-'),
      el('td', {}, row.submitted_at ? fmtTime(row.submitted_at) : '-'),
      el('td', { class: 'row' }, cells.length ? cells : el('span', { class: 'muted' }, '-')));
  });

  setMain(el('div', { class: 'panel' },
    el('div', { class: 'row' },
      el('h2', {}, `考后对账：${d.exam_name}`),
      el('button', { onclick: viewReconcile }, '刷新'),
      el('a', { href: `/api/teacher/exams/${S.examId}/code.zip?t=${encodeURIComponent(S.token)}` },
        el('button', { class: 'primary' }, '导出全部代码 (zip)'))),
    summary,
    el('p', { class: 'muted' },
      '「有代码未提交」是学生写了代码但没点「提交判题」。可逐人用最后一次定时快照补交，' +
      '补交的提交会标注来源并记入操作日志。'),
    d.expected_classes.length
      ? el('p', { class: 'muted small' },
          `缺考是按已登录学生所在的班级（${d.expected_classes.map((c) => c || '未分班').join('、')}）` +
          '与名单比对得出的；名单中其他班级的学生不计入。')
      : null,
    d.rows.length
      ? el('div', { class: 'scroll' }, el('table', {},
          el('thead', {}, el('tr', {},
            ...['状态', '班级', '学号', '姓名', '登录时间', '交卷时间', '各题情况'].map((h) => el('th', {}, h)))),
          el('tbody', {}, rows)))
      : el('div', { class: 'empty' }, '还没有学生参加这场考试。')));
}
