// ============== 来源预热（开关 / 游标 / 步长）==============
// 每个来源各有一套游标与步长（服务端分开存），所以这里按来源渲染控制行。
let preheatSources = {}, cfAutocontinue = false;

function srcLabel(p) {
  if (p === 'modrinth') return 'Modrinth';
  return p || '';
}

// renderPreheatSources 渲染控制行：开关 + 已缓存位置 + 每次预热数 + 立即预热 / 重置。
function renderPreheatSources(state, autocontinue) {
  const wrap = document.getElementById('preheat_sources');
  if (!wrap) return;
  preheatSources = state || {};
  cfAutocontinue = !!autocontinue;
  const rows = ['modrinth'].map(p => {
    const s = preheatSources[p] || {};
    return '<div class="preheat-row">'
      + '<label class="hint"><input type="checkbox" ' + (s.enabled ? 'checked' : '')
      + ' onchange="togglePreheatSource(\'' + p + '\', this.checked)"><span>' + srcLabel(p) + '</span></label>'
      + '<span class="hint">已缓存位置 <strong>' + (s.cursor || 0) + '</strong></span>'
      + '<span class="hint">每次预热 <input type="number" min="100" max="50000" value="' + (s.step || 5000)
      + '" style="width:80px;padding:4px;" onchange="updatePreheatStep(\'' + p + '\', this.value)"> 个</span>'
      + '<button class="sm" onclick="startOnePlatform(\'' + p + '\')">立即预热</button>'
      + '<button class="sm" onclick="resetPreheatCursor(\'' + p + '\')">重置到 Top 1</button>'
      + '</div>';
  });
  // 自动续跑（全局开关）：勾上之后一批跑完会自己开下一批，直到"没有新条目"或用户点停止/取消勾选
  rows.push('<div class="preheat-row">'
    + '<label class="hint"><input type="checkbox" ' + (cfAutocontinue ? 'checked' : '')
    + ' onchange="toggleAutocontinue(this.checked)"><span>自动续跑</span></label>'
    + '<span class="hint">一批跑完自动开下一批；无新条目或点停止即停</span>'
    + '</div>');
  wrap.innerHTML = rows.join('');
}

async function toggleAutocontinue(on) {
  try {
    const r = await fetch('/worker/toggle', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ autocontinue: on }),
    });
    if (!r.ok) { alert('切换失败: ' + r.status); return; }
    refreshWorker();
  } catch (e) { alert('切换失败: ' + e.message); }
}

async function togglePreheatSource(platform, on) {
  const body = { preheat_modrinth: on };
  try {
    const r = await fetch('/worker/toggle', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    if (!r.ok) alert('切换失败: ' + r.status);
    refreshWorker();
  } catch (e) { alert('切换失败: ' + e.message); }
}
async function updatePreheatStep(platform, v) {
  const step = parseInt(v, 10);
  if (!step || step <= 0) return;
  try {
    const r = await fetch('/worker/preheat/cursor', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ platform: platform, step: step }),
    });
    if (!r.ok) alert('保存失败');
  } catch (e) { alert('保存失败: ' + e.message); }
}
async function resetPreheatCursor(platform) {
  const s = preheatSources[platform] || {};
  // CF 的游标是"当前类型内的位置"，重置只影响当前类型（其它类型的进度保留）
  const what = s.class_name ? (srcLabel(platform) + ' 当前类型（' + s.class_name + '）') : srcLabel(platform);
  if (!confirm('把 ' + what + ' 的预热位置重置到第 1 名？（不会删除已缓存的数据）')) return;
  try {
    const r = await fetch('/worker/preheat/cursor', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ platform: platform, cursor: 0 }),
    });
    if (!r.ok) { alert('重置失败'); return; }
    refreshWorker();
  } catch (e) { alert('重置失败: ' + e.message); }
}
// startOnePlatform 只预热一个来源
async function startOnePlatform(platform) {
  await startWorker('preheat', platform);
}

// ============== 接口鉴权开关 ==============
// 逐接口开关：路径 → 是否需要账号密码。默认表与后端 authmw.go 保持一致。
const AUTH_PATHS = [
  ['/health', '健康检查（拨测用）', 0],
  ['/translate/mod', '单个 mod 翻译', 0],
  ['/translate/mods', '批量 mod 翻译', 0],
  ['/feedback/stale', '过期反馈', 0],
  ['/feedback/quality', '质量反馈', 0],
  ['/stats', '统计数据', 1],
  ['/worker/status', 'worker 状态', 1],
  ['/worker/start', '启动任务', 1],
  ['/worker/stop', '停止任务', 1],
  ['/worker/toggle', '启用 / 正文开关', 1],
  ['/worker/preheat/cursor', '预热游标与步长', 1],
  ['/worker/logs', '日志查看', 1],
  ['/worker/cache/list', '缓存列表', 1],
  ['/worker/cache/get', '缓存详情', 1],
  ['/advisor/snapshot', '管家快照', 1],
  ['/advisor/ask', '问管家', 1],
  ['/advisor/last', '上次建议', 1],
  ['/advisor/apply', '应用管家建议', 1],
  ['/advisor/rollback', '回滚管家建议', 1],
  ['/live', '实时槽位视图', 1],
  ['/worker/feedback/list', '反馈列表', 1],
  ['/worker/feedback/status', '反馈改状态', 1],
  ['/worker/feedback/retranslate', '重新翻译', 1],
  ['/config/get', '读取配置', 1],
  ['/config/set', '保存配置', 1],
  ['/config/reset', '恢复默认', 1],
  ['/config/test', '测试链路', 1],
];
function renderAuthGrid(cfg) {
  const grid = document.getElementById('auth_grid');
  if (!grid) return;
  const cur = (cfg && cfg.auth_endpoints) || {};
  grid.innerHTML = AUTH_PATHS.map(([p, t, def]) => {
    const on = (p in cur) ? !!cur[p] : !!def;
    return '<label class="auth-item">'
      + '<input type="checkbox" data-path="' + p + '"' + (on ? ' checked' : '') + ' onchange="authTogglePath(this)">'
      + '<span><b>' + p + '</b><em>' + t + (on ? '' : ' · 当前免鉴权') + '</em></span></label>';
  }).join('');
}
function authTogglePath(el) {
  if (el.checked) { renderAuthGrid({ auth_endpoints: collectAuthEndpoints() }); return; }
  if (!confirm('把 ' + el.dataset.path + ' 改为「免鉴权」？\n任何知道地址的人都能访问它。')) {
    el.checked = true;
  }
  renderAuthGrid({ auth_endpoints: collectAuthEndpoints() });
}
function authSetClientPublic() {
  const out = {};
  AUTH_PATHS.forEach(([p, , def]) => { out[p] = !!def; });
  renderAuthGrid({ auth_endpoints: out });
}
function authSetAllRequired() {
  if (!confirm('把全部接口（含客户端接口）都设为需要鉴权？\n模组客户端会全部失败，除非它们也带账号密码。')) return;
  const out = {};
  AUTH_PATHS.forEach(([p]) => { out[p] = true; });
  renderAuthGrid({ auth_endpoints: out });
}
function collectAuthEndpoints() {
  const out = {};
  document.querySelectorAll('#auth_grid input[data-path]').forEach(el => { out[el.dataset.path] = el.checked; });
  return out;
}

// ============== Worker 控制 ==============
async function toggleWorker() {
  const enabled = document.getElementById('worker_toggle').checked;
  try {
    const r = await fetch('/worker/toggle', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled }) });
    if (!r.ok) { alert('切换失败: ' + r.status); document.getElementById('worker_toggle').checked = !enabled; return; }
    refreshWorker();
  } catch (e) { alert('切换失败: ' + e.message); }
}
async function toggleWorkerBody() {
  const bodyCache = document.getElementById('worker_body').checked;
  try {
    const r = await fetch('/worker/toggle', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ body_cache: bodyCache }) });
    if (!r.ok) { alert('切换失败: ' + r.status); document.getElementById('worker_body').checked = !bodyCache; return; }
    refreshWorker();
  } catch (e) { alert('切换失败: ' + e.message); }
}
async function startWorker(type, platform) {
  const what = type === 'preheat'
    ? ('开始预热？' + (platform ? '（仅 ' + srcLabel(platform) + '）' : '（按勾选的来源逐个跑，从各自已缓存位置继续）'))
    : '开始检测更新？（两个来源合起来最多 500 个）';
  if (!confirm(what)) return;
  try {
    const r = await fetch('/worker/start', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: type, platform: platform || '' }),
    });
    const data = await r.json();
    if (!r.ok) { alert('启动失败: ' + (data.error || r.status)); return; }
    refreshWorker();
  } catch (e) { alert('启动失败: ' + e.message); }
}
async function stopWorker() {
  if (!confirm('停止 Worker？当前 mod 完成后会停止。')) return;
  try {
    const r = await fetch('/worker/stop', { method: 'POST' });
    if (!r.ok) { alert('停止失败: ' + r.status); return; }
    refreshWorker();
  } catch (e) { alert('停止失败: ' + e.message); }
}

// ============== 配置：Provider ==============
function providerTypeTag(type) {
  const t = type === 'gemini' ? 'gemini' : 'openai';
  return `<span class="type-tag ${t}">${t.toUpperCase()}</span>`;
}
function renderProviders() {
  const grid = document.getElementById('providers_grid');
  if (!grid) return;
  document.getElementById('providers_count').textContent = providers.length + ' 个';
  if (providers.length === 0) {
    grid.innerHTML = '<div class="empty">暂无 Provider，点击右上角添加</div>';
  } else {
    grid.innerHTML = providers.map((p, i) => {
      const mine = chain.map((st, j) => ({ st, j })).filter(o => o.st.provider === p.name);
      return `
      <div class="provider-card">
        <div class="pc-head">
          <input class="pc-name-input" type="text" value="${esc(p.name)}" placeholder="Provider 名称" title="Provider 名称（改完回车或点空白处生效，引用它的链步骤会一起更新）" onchange="renameProvider(${i}, this.value)">
          <span class="pc-actions">
            ${providerTypeTag(p.type)}
            <button class="sm" onclick="testProvider(${i})">测试</button>
            <button class="danger sm" onclick="removeProvider(${i})">删除</button>
          </span>
        </div>
        <div class="pc-field"><label>类型</label>
          <select onchange="providers[${i}].type=this.value; renderProviders();">
            <option value="openai" ${p.type === 'openai' ? 'selected' : ''}>OpenAI 兼容</option>
            <option value="gemini" ${p.type === 'gemini' ? 'selected' : ''}>Gemini</option>
          </select>
        </div>
        <div class="pc-field"><label>Base URL</label>
          <input type="text" value="${esc(p.base_url)}" oninput="providers[${i}].base_url=this.value">
        </div>
        <div class="pc-field"><label>模型</label>
          <div class="pc-model-list">
            ${mine.length === 0
              ? '<div class="hint">未配置模型，该 provider 不会被调用</div>'
              : mine.map(o => `
                <div class="pc-model-row">
                  <input type="text" value="${esc(o.st.model)}" placeholder="如 your-model-name" title="模型名；尝试顺序由下方 Fallback 链路决定" oninput="chain[${o.j}].model=this.value" onchange="renderChain()">
                  <button class="ghost" onclick="removeModelFor(${o.j})" title="移除该模型">✕</button>
                </div>`).join('')}
            <button class="ghost pc-add-model" onclick="addModelFor(${i})">＋ 添加模型</button>
          </div>
        </div>
        <div class="pc-field"><label>API Key</label>
          <input type="password" value="${esc(p.api_key)}" placeholder="填写新 Key 以更新" oninput="providers[${i}].api_key=this.value">
          ${p.api_key === MASK ? '<span class="key-badge pc-field" style="grid-column:2;">✓ 已配置</span>' : ''}
        </div>
        <div class="pc-field"><label>并发</label>
          <input type="number" min="1" max="100" value="${p.concurrent}" oninput="providers[${i}].concurrent=+this.value">
        </div>
        <div class="pc-field"><label>超时(秒)</label>
          <input type="number" min="1" max="120" value="${p.timeout_sec}" oninput="providers[${i}].timeout_sec=+this.value">
        </div>
      </div>`;
    }).join('');
  }
  renderChain();
  renderAdvisor();
}
function addProvider() {
  const n = providers.length + 1;
  providers.push({ name: 'provider' + n, prevName: '', type: 'openai', base_url: 'https://api.openai.com/v1', api_key: '', concurrent: 2, timeout_sec: 5 });
  renderProviders();
}
// renameProvider 改名并同步引用它的链步骤；prevName 记录改名前服务端里的名字，
// 服务端据此在 key 还是掩码时找回真实 key（否则改名会把 key 存成 ********）。
function renameProvider(i, newName) {
  const p = providers[i];
  if (!p) return;
  const old = p.name;
  const name = (newName || '').trim();
  if (!name) { alert('Provider 名称不能为空'); renderProviders(); return; }
  if (name === old) { renderProviders(); return; }
  if (providers.some((q, j) => j !== i && q.name === name)) { alert('Provider 名称重复: ' + name); renderProviders(); return; }
  p.prevName = p.prevName || old;
  p.name = name;
  chain.forEach(st => { if (st.provider === old) st.provider = name; });
  renderProviders();
}
function addModelFor(i) {
  const p = providers[i];
  if (!p) return;
  chain.push({ provider: p.name, model: '', daily_limit: 0, out_tok: 0, share: 0, rpm: 0, cool: 0 });
  renderProviders();
}
function removeModelFor(j) { if (chain[j]) { chain.splice(j, 1); renderProviders(); } }
function removeProvider(i) {
  if (!confirm('删除 Provider「' + providers[i].name + '」？引用它的链步骤也会被移除。')) return;
  const name = providers[i].name;
  providers.splice(i, 1);
  chain = chain.filter(st => st.provider !== name);
  renderProviders();
}

// ============== 配置：Fallback 链 ==============
function renderChain() {
  const list = document.getElementById('chain_list');
  const wrap = document.getElementById('t_model_usage');
  if (!list) return;
  document.getElementById('chain_count').textContent = chain.length + ' 个';
  if (chain.length === 0) {
    list.innerHTML = '<div class="empty">链条为空，点击右上角添加模型</div>';
  } else {
    list.innerHTML = chain.map((st, i) => `
      <div class="chain-row">
        <div class="cf-order">${i + 1}</div>
        <div class="cf-field cf-provider"><label>Provider</label>
          <select onchange="chain[${i}].provider=this.value; renderChain();">
            ${providers.map(p => `<option value="${esc(p.name)}" ${p.name === st.provider ? 'selected' : ''}>${esc(p.name)}</option>`).join('')}
          </select>
        </div>
        <div class="cf-field cf-model"><label>Model</label>
          <input type="text" value="${esc(st.model)}" placeholder="如 your-model-name" oninput="chain[${i}].model=this.value">
        </div>
        <div class="cf-field cf-limit"><label>每日上限(Worker)</label>
          <input type="number" min="0" value="${st.daily_limit || 0}" title="该模型每日预算，0=不限" oninput="chain[${i}].daily_limit=+this.value">
        </div>
        <div class="cf-field cf-outtok"><label>输出Token/分</label>
          <input type="number" min="0" value="${st.out_tok || 0}" title="每分钟输出token预算（正文分块限速），0=不限速" oninput="chain[${i}].out_tok=+this.value">
        </div>
        <div class="cf-field cf-rpm"><label>RPM</label>
          <input type="number" min="0" max="100000" value="${st.rpm || 0}" title="每分钟请求数上限，0=不限。免费档常硬限（如 20）；填了之后请求会在本地按 60/RPM 秒的间隔排队，避免撞限速被强制冷却。注意：按【账号】计——同一个 provider 下挂多个模型时共用这个额度（取最小值）。" oninput="chain[${i}].rpm=+this.value">
        </div>
        <div class="cf-field cf-cool"><label>限速冷却(秒)</label>
          <input type="number" min="0" max="3600" value="${st.cool || 0}" title="命中限速后冷却多久，0=默认 15 秒。上游若规定「超限罚一分钟」就填 60。" oninput="chain[${i}].cool=+this.value">
        </div>
        <div class="cf-field cf-share"><label>正文分摊</label>
          <input type="number" min="0" value="${st.share || 0}" title="正文分摊权重：>0 才会参与正文/描述的分块分摊（按权重把 mod 分给不同模型）；0=只做兜底，worker 不会主动用它。多账号时务必都填 >0，否则新账号等于白加。" oninput="chain[${i}].share=+this.value">
        </div>
        <div class="cf-ctrl">
          <button class="ghost" ${i === 0 ? 'disabled' : ''} onclick="moveChainStep(${i},-1)" title="上移">↑</button>
          <button class="ghost" ${i === chain.length - 1 ? 'disabled' : ''} onclick="moveChainStep(${i},1)" title="下移">↓</button>
          <button class="ghost" onclick="removeChainStep(${i})" title="删除">✕</button>
        </div>
      </div>`).join('');
  }
  if (wrap) wrap.innerHTML = '<tbody><tr><td class="empty">保存或刷新后显示用量</td></tr></tbody>';
}
function addChainStep() {
  // share 默认 5（与多账号分摊的常见配置一致）：新加的步骤若 share=0，worker 的正文/描述分摊
  // 会完全绕过它 —— 只能当兜底，用户很容易以为"加了账号没用"（踩过）。想纯兜底就手动改 0。
  chain.push({ provider: providers[0] ? providers[0].name : '', model: '', daily_limit: 0, out_tok: 0, share: 5, rpm: 0, cool: 0 });
  renderProviders();
}
function removeChainStep(i) { chain.splice(i, 1); renderProviders(); }
function moveChainStep(i, dir) {
  const j = i + dir;
  if (j < 0 || j >= chain.length) return;
  const t = chain[i]; chain[i] = chain[j]; chain[j] = t;
  renderProviders();
}

// ============== 配置加载 / 保存 / 重置 / 测试 ==============
async function loadConfig() {
  try {
    const r = await fetch('/config/get');
    if (!r.ok) return;
    const cfg = await r.json();
    providers = (cfg.providers || []).map(p => ({
      name: p.name, type: p.type, base_url: p.base_url,
      api_key: p.api_key || '', concurrent: p.concurrent, timeout_sec: p.timeout_sec,
    }));
    chain = (cfg.chain || []).map(st => ({
      provider: st.provider, model: st.model, daily_limit: st.daily_limit || 0,
      out_tok: st.out_tokens_per_min || 0, share: st.body_share || 0,
      rpm: st.rpm || 0, cool: st.rate_limit_cooldown_sec || 0,
    }));
    advisorCfg = { provider: cfg.advisor_provider || '', model: cfg.advisor_model || '' };
    const aw = document.getElementById('advisor_window');
    if (aw) aw.value = cfg.advisor_window_min || 10;
    document.getElementById('cfg_breaker_threshold').value = cfg.breaker_threshold || 3;
    document.getElementById('cfg_breaker_duration').value = cfg.breaker_duration_min || 5;
    document.getElementById('cfg_body_chunk').value = cfg.body_chunk_chars || 1500;
    renderAuthGrid(cfg);
    renderProviders();
    renderModelUsage([], {});
    populateLogModels();
  } catch (e) { console.error('loadConfig', e); }
}
// ============== AI 管家（只出建议，不改配置）==============
let advisorCfg = { provider: '', model: '' };

// 观察窗分钟数：管家判断用的时间范围（服务端还会做 1~120 的兜底钳制）
function advWindowMin() {
  const el = document.getElementById('advisor_window');
  let n = parseInt((el || {}).value, 10);
  if (!(n >= 1)) n = 10;
  if (n > 120) n = 120;
  return n;
}

function renderAdvisor() {
  const sel = document.getElementById('advisor_provider');
  if (!sel) return;
  const cur = sel.value || advisorCfg.provider || '';
  sel.innerHTML = ['<option value="">（未启用）</option>'].concat(
    providers.map(p => `<option value="${esc(p.name)}" ${p.name === cur ? 'selected' : ''}>${esc(p.name)}</option>`)
  ).join('');
  const m = document.getElementById('advisor_model');
  if (m && document.activeElement !== m && !m.value) m.value = advisorCfg.model || '';
}

// advisorSnapshot 只生成并显示脱敏快照（不发任何模型），方便你自己确认里面有什么。
async function advisorSnapshot() {
  const st = document.getElementById('advisor_state');
  st.textContent = '生成中…';
  try {
    const r = await fetch('/advisor/snapshot', { headers: { 'Accept': 'application/json' } });
    if (!r.ok) { st.textContent = '✗ HTTP ' + r.status; return; }
    const d = await r.json();
    const pre = document.getElementById('advisor_raw');
    pre.style.display = '';
    pre.textContent = JSON.stringify(d, null, 2);
    document.getElementById('advisor_advice').style.display = 'none';
    st.textContent = '快照已生成（未发送给任何模型）';
  } catch (e) { st.textContent = '✗ ' + e.message; }
}

async function advisorAsk() {
  const st = document.getElementById('advisor_state');
  st.textContent = '管家思考中…';
  try {
    const r = await fetch('/advisor/ask', { method: 'POST' });
    const d = await r.json().catch(() => ({}));
    if (!r.ok || !d.ok) { st.textContent = '✗ ' + (d.error || ('HTTP ' + r.status)); return; }
    st.textContent = '✓ ' + d.model + ' · ' + d.ms + 'ms';
    renderAdvisorAdvice(d);
  } catch (e) { st.textContent = '✗ ' + e.message; }
}

async function advisorLast() {
  const st = document.getElementById('advisor_state');
  try {
    const r = await fetch('/advisor/last', { headers: { 'Accept': 'application/json' } });
    const d = await r.json().catch(() => ({}));
    if (!d.ok) { st.textContent = d.error || '还没有问过管家'; return; }
    st.textContent = '上次提问：' + new Date(d.asked_at * 1000).toLocaleString('zh-CN') + ' · ' + d.model;
    renderAdvisorAdvice(d);
  } catch (e) { st.textContent = '✗ ' + e.message; }
}

// renderAdvisorAdvice 展示管家建议；每条改动都带服务端的确定性校验结果（通过校验 / 非法原因）。
function renderAdvisorAdvice(d) {
  const box = document.getElementById('advisor_advice');
  box.style.display = '';
  const a = d.advice;
  let html = '';
  if (!a) {
    html = '<div class="hint" style="color:var(--bad)">管家输出不是合法 JSON：' + esc(d.parse_error || '') + '</div>';
  } else {
    html += '<div style="margin-bottom:6px;">结论：<strong>' + esc(a.summary || '（无）') + '</strong>'
      + ' <span class="hint">health=' + esc(a.health || '?') + ' · 置信度 ' + (a.confidence != null ? a.confidence : '?') + '</span></div>';
    if ((a.diagnosis || []).length) {
      html += '<div class="hint" style="margin-bottom:6px;">' + a.diagnosis.map(x => '· ' + esc(x)).join('<br>') + '</div>';
    }
    if ((a.changes || []).length) {
      html += '<table><thead><tr><th>账号</th><th>字段</th><th>当前</th><th>建议</th><th>理由</th><th>校验</th></tr></thead><tbody>';
      a.changes.forEach(c => {
        html += '<tr><td class="mono">' + esc(c.target) + '</td><td class="mono">' + esc(c.field) + '</td>'
          + '<td class="hint">' + esc(c.from || '') + '</td><td><strong>' + esc(c.to) + '</strong></td>'
          + '<td class="hint">' + esc(c.reason || '') + '</td>'
          + '<td>' + (c.valid
            ? '<span class="tag body">通过校验</span>'
            : '<span class="tag" style="background:rgba(220,38,38,.12);color:var(--bad)">' + esc(c.error || '非法') + '</span>')
          + '</td></tr>';
      });
      html += '</tbody></table>'
        + '<div class="cf-actions" style="margin-top:8px;">'
        + '<button class="primary sm" onclick="advisorApply()">应用「通过校验」的项</button>'
        + '<button class="sm" onclick="advisorRollback()" title="回到应用建议之前的配置">回滚上次应用</button>'
        + '<span class="hint">应用前会自动备份；并发/RPM 立即生效</span></div>';
    } else {
      html += '<div class="hint">管家认为不需要改动。</div>';
    }
    if ((a.warnings || []).length) {
      html += '<div class="hint" style="margin-top:6px;color:var(--bad)">' + a.warnings.map(x => '⚠ ' + esc(x)).join('<br>') + '</div>';
    }
  }
  box.innerHTML = html;
}

// advisorApply 应用建议里「通过校验」的项；服务端会再校验一次并先备份。
async function advisorApply() {
  const st = document.getElementById('advisor_state');
  if (!confirm('应用管家建议里「通过校验」的项？\n（服务端会再校验一次，并自动备份当前配置，可回滚）')) return;
  try {
    const r = await fetch('/advisor/apply', { method: 'POST' });
    const d = await r.json().catch(() => ({}));
    if (!r.ok || !d.ok) { st.textContent = '✗ ' + (d.error || ('HTTP ' + r.status)); return; }
    const n = (d.changed || []).length;
    st.textContent = n ? ('✓ 已应用 ' + n + ' 项') : '没有可应用项';
    alert(n ? ('已应用：\n' + d.changed.join('\n')) : (d.note || '没有可应用项'));
    await loadConfig();
    refreshWorker();
  } catch (e) { st.textContent = '✗ ' + e.message; }
}

async function advisorRollback() {
  const st = document.getElementById('advisor_state');
  if (!confirm('回滚到「应用建议之前」的配置？')) return;
  try {
    const r = await fetch('/advisor/rollback', { method: 'POST' });
    const d = await r.json().catch(() => ({}));
    if (!r.ok || !d.ok) { st.textContent = '✗ ' + (d.error || ('HTTP ' + r.status)); return; }
    st.textContent = '✓ ' + (d.note || '已回滚');
    await loadConfig();
    refreshWorker();
  } catch (e) { st.textContent = '✗ ' + e.message; }
}

function validateConfig() {
  const err = (m) => ({ ok: false, msg: m });
  if (providers.length === 0) return err('至少需要一个 Provider');
  const seen = {};
  for (const p of providers) {
    if (!p.name.trim()) return err('Provider 名称不能为空');
    if (seen[p.name]) return err('Provider 名称重复: ' + p.name);
    seen[p.name] = true;
    if (p.type !== 'openai' && p.type !== 'gemini') return err('Provider 类型非法: ' + p.name);
    if (!p.base_url.trim()) return err('Provider Base URL 不能为空: ' + p.name);
    if (p.concurrent < 1 || p.concurrent > 100) return err(p.name + ' 并发需在 1-100');
    if (p.timeout_sec < 1 || p.timeout_sec > 120) return err(p.name + ' 超时需在 1-120');
  }
  if (chain.length === 0) return err('Fallback 链不能为空');
  for (const st of chain) {
    if (!seen[st.provider]) return err('链引用了不存在的 Provider: ' + st.provider);
    if (!st.model.trim()) return err('链上模型名不能为空');
    if (st.daily_limit < 0) return err('每日上限不能为负: ' + st.model);
    if (st.out_tok < 0) return err('输出Token/分不能为负: ' + st.model);
    if (st.share < 0) return err('正文分摊权重不能为负: ' + st.model);
    if (st.rpm < 0 || st.rpm > 100000) return err('RPM 需在 0-100000: ' + st.model);
    if (st.cool < 0 || st.cool > 3600) return err('限速冷却需在 0-3600 秒: ' + st.model);
  }
  const bc = cfgBodyChunk();
  if (bc < 200 || bc > 8000) return err('正文分块字符数需在 200-8000');
  return { ok: true };
}
function cfgBodyChunk() {
  const v = parseInt(document.getElementById('cfg_body_chunk').value, 10);
  return v > 0 ? v : 1500;
}
// buildConfigPayload 汇总面板当前表单值；保存与测试共用，保证"测试的就是你填的内容"。
function buildConfigPayload() {
  return {
    providers: providers.map(p => ({
      name: p.name.trim(), type: p.type, base_url: p.base_url.trim(),
      api_key: p.api_key || '', concurrent: p.concurrent, timeout_sec: p.timeout_sec,
      prev_name: p.prevName || '',
    })),
    chain: chain.map(st => ({
      provider: st.provider, model: st.model.trim(), daily_limit: st.daily_limit || 0,
      out_tokens_per_min: st.out_tok || 0, body_share: st.share || 0,
      rpm: st.rpm || 0, rate_limit_cooldown_sec: st.cool || 0,
    })),
    breaker_threshold: parseInt(document.getElementById('cfg_breaker_threshold').value, 10),
    breaker_duration_min: parseInt(document.getElementById('cfg_breaker_duration').value, 10),
    body_chunk_chars: cfgBodyChunk(),
    auth_endpoints: collectAuthEndpoints(),
    advisor_provider: (document.getElementById('advisor_provider') || {}).value || '',
    advisor_model: ((document.getElementById('advisor_model') || {}).value || '').trim(),
    advisor_window_min: advWindowMin(),
  };
}
async function saveConfig() {
  const resultEl = document.getElementById('config_result');
  resultEl.textContent = '';
  const check = validateConfig();
  if (!check.ok) { resultEl.textContent = '✗ ' + check.msg; resultEl.style.color = 'var(--bad)'; return; }
  try {
    const r = await fetch('/config/set', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(buildConfigPayload()) });
    if (!r.ok) { resultEl.textContent = '✗ 保存失败: ' + (await r.text()); resultEl.style.color = 'var(--bad)'; return; }
    resultEl.textContent = '✓ 已保存并生效';
    resultEl.style.color = 'var(--good)';
    await loadConfig();
    refresh();
    refreshWorker();
  } catch (e) { resultEl.textContent = '✗ ' + e.message; resultEl.style.color = 'var(--bad)'; }
}
async function resetConfig() {
  if (!confirm('恢复所有配置到环境变量默认值？')) return;
  try {
    const r = await fetch('/config/reset', { method: 'POST' });
    if (!r.ok) { alert('重置失败: ' + r.status); return; }
    await loadConfig();
    resultHint('✓ 已恢复默认');
    document.getElementById('config_result').style.color = 'var(--good)';
    refresh();
    refreshWorker();
  } catch (e) { alert('重置失败: ' + e.message); }
}
async function testRequest(body, label) {
  resultHint(label + '…');
  const r = await fetch('/config/test', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const data = await r.json();
  if (data.ok) {
    const e = document.getElementById('config_result');
    e.textContent = `✓ ${data.model || '整条链'} 正常（${data.elapsed}ms）${data.result ? '：' + data.result : ''}`;
    e.style.color = 'var(--good)';
  } else {
    resultErr(label + ' 失败: ' + data.error);
  }
}
async function testProvider(i) {
  const p = providers[i];
  if (!p) return;
  const check = validateConfig();
  if (!check.ok) { resultErr(check.msg); return; }
  resultHint(`测试 ${p.name}（当前表单内容）…`);
  try { await testRequest({ provider: p.name, model: '', config: buildConfigPayload() }, '测试 ' + p.name); }
  catch (e) { resultErr(e.message); }
}
async function testConfigAll() {
  const check = validateConfig();
  if (!check.ok) { resultErr(check.msg); return; }
  try { await testRequest({ provider: '', model: '', config: buildConfigPayload() }, '测试整条链'); }
  catch (e) { resultErr(e.message); }
}
function resultHint(t) { const e = document.getElementById('config_result'); e.textContent = t; e.style.color = 'var(--muted)'; }
function resultErr(t) { const e = document.getElementById('config_result'); e.textContent = '✗ ' + t; e.style.color = 'var(--bad)'; }

// ============== 缓存浏览 ==============
let cacheOffset = 0, cacheLimit = 50, cacheQ = '', cacheKind = 'all', cacheTotal = 0, cacheRows = [];

// srcTag 渲染来源标识（未知来源原样显示，方便将来接新平台时一眼看出）
function srcTag(p) {
  if (p === 'modrinth') return '<span class="tag src-mr">Modrinth</span>';
  return '<span class="hint">' + esc(p || '?') + '</span>';
}
let rawMode = false, currentDetail = null;

// renderMarkdown：marked 渲染 + DOMPurify 净化（mod 正文含原始 HTML，必须净化）
function renderMarkdown(text) {
  if (!window.marked || !window.DOMPurify) return null;
  try {
    window.marked.setOptions({ gfm: true, breaks: true });
    return window.DOMPurify.sanitize(window.marked.parse(text), { ADD_ATTR: ['target', 'rel'] });
  } catch (e) { return null; }
}
function renderSection(which, entry) {
  const el = document.getElementById('text_' + which);
  const sub = document.getElementById('sub_' + which);
  if (!entry || !entry.text) {
    sub.textContent = '';
    el.classList.remove('raw');
    el.innerHTML = '<div class="empty">这个 mod 还没有缓存' + (which === 'desc' ? '描述' : '正文') + '</div>';
    return;
  }
  sub.textContent = entry.chars + ' 字符 · 翻译于 ' + fmtTime(entry.translated_at);
  if (rawMode) { el.classList.add('raw'); el.textContent = entry.text; return; }
  const html = renderMarkdown(entry.text);
  if (html === null) { el.classList.add('raw'); el.textContent = entry.text; return; }
  el.classList.remove('raw');
  el.innerHTML = html;
  el.querySelectorAll('a').forEach(a => { a.target = '_blank'; a.rel = 'noopener noreferrer'; });
  el.querySelectorAll('img').forEach(img => { img.loading = 'lazy'; });
}
function toggleRaw() {
  rawMode = !rawMode;
  document.getElementById('rawBtn').textContent = rawMode ? '渲染 Markdown' : '显示源码';
  document.getElementById('md_state').textContent = rawMode ? '源码模式' : '';
  if (currentDetail) { renderSection('desc', currentDetail.desc); renderSection('body', currentDetail.body); }
}
async function loadCacheDetail(i) {
  const it = cacheRows[i];
  if (!it) return;
  try {
    const r = await fetch('/worker/cache/get?key=' + encodeURIComponent(it.key), { headers: { 'Accept': 'application/json' } });
    if (!r.ok) return;
    const d = await r.json();
    const sib = (d.sibling && d.sibling.key) ? d.sibling : null;
    const desc = d.kind === 'desc' ? d : sib;
    const body = d.kind === 'body' ? d : sib;
    currentDetail = { desc: desc, body: body };
    document.getElementById('detail_panel').style.display = '';
    document.getElementById('detail_meta').textContent =
      d.mod_id + ' · ' + (d.platform || '') + ' · 描述 ' + (desc ? desc.chars + ' 字符' : '未缓存') +
      ' · 正文 ' + (body ? body.chars + ' 字符' : '未缓存');
    // 外链按来源走（未知来源不给链接，避免拼出 404 地址）
    const link = document.getElementById('project_link');
    if (link) {
      if (d.platform === 'modrinth') {
        link.href = 'https://modrinth.com/mod/' + encodeURIComponent(d.mod_id);
        link.querySelector('button').textContent = '在 Modrinth 打开';
        link.style.display = '';
      } else {
        link.style.display = 'none';
      }
    }
    document.getElementById('col_desc').classList.toggle('active', d.kind === 'desc');
    document.getElementById('col_body').classList.toggle('active', d.kind === 'body');
    renderSection('desc', desc);
    renderSection('body', body);
    document.getElementById('detail_panel').scrollIntoView({ behavior: 'smooth', block: 'start' });
  } catch (e) { /* 忽略 */ }
}
function copyDetail() {
  const btn = document.getElementById('copyBtn');
  const t = (currentDetail && currentDetail.desc ? currentDetail.desc.text : '') + '\n\n' + (currentDetail && currentDetail.body ? currentDetail.body.text : '');
  navigator.clipboard.writeText(t).then(() => {
    btn.textContent = '已复制 ' + t.length + ' 字符';
    setTimeout(() => { btn.textContent = '复制全文'; }, 1500);
  }).catch(() => { btn.textContent = '复制失败'; });
}
async function loadCacheList() {
  try {
    const url = '/worker/cache/list?q=' + encodeURIComponent(cacheQ) + '&kind=' + cacheKind
      + '&limit=' + cacheLimit + '&offset=' + cacheOffset;
    const r = await fetch(url, { headers: { 'Accept': 'application/json' } });
    if (!r.ok) return;
    const d = await r.json();
    cacheTotal = d.total || 0;
    cacheLimit = d.limit || cacheLimit;
    cacheOffset = d.offset || 0;
    document.getElementById('counts').textContent = '匹配 ' + (d.desc_count || 0) + ' 条描述 · ' + (d.body_count || 0) + ' 条正文';
    const wrap = document.getElementById('t_list');
    cacheRows = d.items || [];
    if (!cacheRows.length) {
      wrap.innerHTML = '<tbody><tr><td class="empty">没有匹配的缓存条目</td></tr></tbody>';
    } else {
      let html = '<thead><tr><th>类型</th><th>Mod</th><th>语言</th><th>字符数</th><th>翻译时间</th><th>译文预览</th><th></th></tr></thead><tbody>';
      cacheRows.forEach((it, i) => {
        const isBody = it.kind === 'body';
        html += '<tr style="cursor:pointer" onclick="loadCacheDetail(' + i + ')">'
          + '<td>' + (isBody ? '<span class="tag body">正文</span>' : '<span class="tag desc">描述</span>') + '</td>'
          + '<td class="mono">' + esc(it.mod_id) + '</td>'
          + '<td>' + esc(it.lang) + '</td>'
          + '<td>' + (it.chars || 0) + '</td>'
          + '<td class="hint">' + fmtTime(it.translated_at) + '</td>'
          + '<td class="preview">' + esc(it.preview) + '</td>'
          + '<td><button class="ghost" onclick="event.stopPropagation();loadCacheDetail(' + i + ')">查看</button></td>'
          + '</tr>';
      });
      wrap.innerHTML = html + '</tbody>';
    }
    const cur = Math.floor(cacheOffset / cacheLimit) + 1;
    const pages = Math.max(1, Math.ceil(cacheTotal / cacheLimit));
    document.getElementById('page_label').textContent = '第 ' + cur + ' / ' + pages + ' 页';
    document.getElementById('pageinfo').textContent = cacheTotal > 0
      ? ('共 ' + cacheTotal + ' 条，当前 ' + (cacheOffset + 1) + '-' + Math.min(cacheOffset + cacheLimit, cacheTotal) + ' 条') : '';
    document.getElementById('btn_prev').disabled = cacheOffset <= 0;
    document.getElementById('btn_next').disabled = cacheOffset + cacheLimit >= cacheTotal;
  } catch (e) { /* 忽略 */ }
}
function cacheSearch() {
  cacheQ = (document.getElementById('q').value || '').trim();
  cacheKind = document.getElementById('cache_kind').value;
  cacheLimit = parseInt(document.getElementById('cache_limit').value, 10) || 50;
  cacheOffset = 0;
  loadCacheList();
}
function cacheReset() {
  document.getElementById('q').value = '';
  document.getElementById('cache_kind').value = 'all';
  document.getElementById('cache_limit').value = '50';
  cacheSearch();
}
function cachePrev() { if (cacheOffset > 0) { cacheOffset = Math.max(0, cacheOffset - cacheLimit); loadCacheList(); } }
function cacheNext() { if (cacheOffset + cacheLimit < cacheTotal) { cacheOffset += cacheLimit; loadCacheList(); } }

// ============== 日志（按模型筛选：服务端读日志尾部） ==============
let logModel = '', logQ = '', logLines = 400;

function logLineClass(ln) {
  if (/failed|✗|panic|\[ERROR\]|错误/.test(ln)) return 'lg-bad';
  if (/限速|RATE-LIMIT|COOLDOWN|BREAKER|熔断|deadline/.test(ln)) return 'lg-warn';
  if (/✓/.test(ln)) return 'lg-ok';
  if (/\[TIMING\]|\[LLM\]/.test(ln)) return 'lg-dim';
  return '';
}
function renderLogLines(lines) {
  const pre = document.getElementById('logs_pre');
  pre.innerHTML = lines.map(l => `<span class="${logLineClass(l)}">${esc(l)}</span>`).join('\n');
}
// populateLogModels 用链路里的 provider/model 填充下拉，并顺手标上今日调用次数
function populateLogModels() {
  const sel = document.getElementById('log_model');
  if (!sel) return;
  const cur = sel.value;
  const opts = ['<option value="">全部模型 / 全局日志</option>'];
  const seen = {};
  chain.forEach(st => {
    if (!st.model) return;
    const key = st.provider + '/' + st.model;
    if (seen[key]) return;
    seen[key] = 1;
    const s = lastModelStats[key] || {};
    const tail = s.calls ? `（今日 ${s.calls} 次${s.failed ? '，失败 ' + s.failed : ''}）` : '';
    opts.push(`<option value="${esc(key)}">${esc(key + tail)}</option>`);
  });
  sel.innerHTML = opts.join('');
  sel.value = (cur && [...sel.options].some(o => o.value === cur)) ? cur : '';
}
async function loadLogs() {
  const pre = document.getElementById('logs_pre');
  if (!pre) return;
  try {
    const keepBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
    logModel = (document.getElementById('log_model') || {}).value || '';
    logQ = ((document.getElementById('log_q') || {}).value || '').trim();
    logLines = parseInt((document.getElementById('log_lines') || {}).value, 10) || 400;
    const url = '/worker/logs?lines=' + logLines + '&model=' + encodeURIComponent(logModel) + '&q=' + encodeURIComponent(logQ);
    const r = await fetch(url, { headers: { 'Accept': 'application/json' } });
    if (!r.ok) { pre.textContent = '读取日志失败: HTTP ' + r.status; return; }
    const d = await r.json();
    const lines = d.lines || [];
    renderLogLines(lines);
    const sum = document.getElementById('log_summary');
    if (sum) {
      sum.textContent = (logModel || '全部模型') + ' · 尾部 ' + (d.total_lines || 0) + ' 行里匹配 ' + (d.matched || 0) +
        ' 行，显示最近 ' + lines.length + ' 行' + (logQ ? ' · 关键词 “' + logQ + '”' : '');
    }
    if (keepBottom) pre.scrollTop = pre.scrollHeight;
  } catch (e) { pre.textContent = '日志读取异常: ' + e.message; }
}
function refreshLogs() { if (activeIs('logs')) loadLogs(); }
function restartLogTimer() {
  if (logTimer) { clearInterval(logTimer); logTimer = null; }
  const live = document.getElementById('log_live');
  if (!live || !live.checked) return;
  logTimer = setInterval(() => { if (document.visibilityState === 'visible' && activeIs('logs')) loadLogs(); }, 3000);
}

// ============== 反馈审核 ==============
// 用户报"翻译有问题"的反馈落库后（status=pending），这里做人工审核闭环：
// 看译文（跳缓存浏览并直接展开）→ 重新翻译（清缓存 + 后台重译）→ 标记已处理 / 忽略。
const ISSUE_LABELS = {
  wrong_translation: '翻译错误',
  wrong_meaning: '意思不对',
  mistranslation: '翻译错误',
  incomplete: '翻译不完整',
  typo: '错别字',
  format: '格式排版',
  other: '其他',
};
const FB_STATUS_TEXT = { pending: '待处理', retranslating: '重译中', done: '已处理', ignored: '已忽略', failed: '重译失败' };
let fbOffset = 0, fbLimit = 20, fbStatusFilter = 'pending', fbQ = '', fbRows = [];

function issueLabel(t) { return ISSUE_LABELS[t] || t || '未标注'; }

function renderFbCards(c) {
  c = c || {};
  let total = 0;
  for (const k in c) total += c[k];
  const set = (id, v) => { const e = document.getElementById(id); if (e) e.textContent = v; };
  set('fb_n_pending', c.pending || 0);
  set('fb_n_retranslating', c.retranslating || 0);
  set('fb_n_failed', c.failed || 0);
  set('fb_n_done', c.done || 0);
  set('fb_n_ignored', c.ignored || 0);
  set('fb_n_total', total);
}
// fbCounts 只拉计数（limit=1），用来更新侧边栏角标，代价很小
async function fbCounts() {
  try {
    const r = await fetch('/worker/feedback/list?status=pending&limit=1', { headers: { 'Accept': 'application/json' } });
    if (!r.ok) return;
    const d = await r.json();
    const c = d.counts || {};
    renderFbCards(c);
    const pending = (c.pending || 0) + (c.retranslating || 0) + (c.failed || 0);
    const badge = document.getElementById('fb_badge');
    if (badge) {
      badge.textContent = pending > 99 ? '99+' : String(pending);
      badge.style.display = pending > 0 ? '' : 'none';
    }
  } catch (e) { /* 忽略 */ }
}
async function fbReload() {
  fbStatusFilter = (document.getElementById('fb_status') || {}).value || 'pending';
  fbQ = ((document.getElementById('fb_q') || {}).value || '').trim();
  fbLimit = parseInt((document.getElementById('fb_limit') || {}).value, 10) || 20;
  fbOffset = 0;
  await fbLoad();
}
async function fbLoad() {
  const wrap = document.getElementById('t_feedback');
  if (!wrap) return;
  try {
    const url = '/worker/feedback/list?status=' + encodeURIComponent(fbStatusFilter) + '&q=' + encodeURIComponent(fbQ) +
      '&limit=' + fbLimit + '&offset=' + fbOffset;
    const r = await fetch(url, { headers: { 'Accept': 'application/json' } });
    if (!r.ok) { wrap.innerHTML = '<tbody><tr><td class="empty">读取失败 HTTP ' + r.status + '</td></tr></tbody>'; return; }
    const d = await r.json();
    fbRows = d.items || [];
    renderFbCards(d.counts);
    const total = d.total || 0;
    const sub = document.getElementById('fb_sub');
    if (sub) sub.textContent = (fbStatusFilter === 'all' ? '全部状态' : (FB_STATUS_TEXT[fbStatusFilter] || fbStatusFilter)) +
      ' · 共 ' + total + ' 条' + (fbQ ? ' · 匹配 “' + fbQ + '”' : '');
    if (!fbRows.length) {
      wrap.innerHTML = '<tbody><tr><td class="empty">没有反馈</td></tr></tbody>';
    } else {
      let html = '<thead><tr><th style="width:30px;"></th><th>时间</th><th>Mod</th><th>问题</th><th>用户反馈</th>'
        + '<th>译文缓存</th><th>状态</th><th style="text-align:right;">操作</th></tr></thead><tbody>';
      fbRows.forEach((it, i) => { html += fbRowHtml(it, i); });
      wrap.innerHTML = html + '</tbody>';
    }
    const lbl = document.getElementById('fb_pagelabel');
    if (lbl) lbl.textContent = total > 0 ? ('第 ' + (Math.floor(fbOffset / fbLimit) + 1) + ' / ' + Math.ceil(total / fbLimit) + ' 页') : '';
    const prev = document.getElementById('fb_prev'), next = document.getElementById('fb_next');
    if (prev) prev.disabled = fbOffset <= 0;
    if (next) next.disabled = fbOffset + fbLimit >= total;
    updateFbSelAll();
  } catch (e) { wrap.innerHTML = '<tbody><tr><td class="empty">读取异常: ' + esc(e.message) + '</td></tr></tbody>'; }
}
function fbRowHtml(it, i) {
  const desc = it.desc_updated_at ? fmtTime(it.desc_updated_at) : '';
  const body = it.body_updated_at ? fmtTime(it.body_updated_at) : '';
  // 缓存时间晚于反馈时间 => 这份译文是收到反馈之后重新翻的
  const fresh = Math.max(it.desc_updated_at || 0, it.body_updated_at || 0) > (it.created_at || 0);
  const cacheCell = (desc || body)
    ? '<span class="fb-cache">' + (fresh ? '<b>已刷新</b><br>' : '') + '描述 ' + (desc || '未缓存') + '<br>正文 ' + (body || '未缓存') + '</span>'
    : '<span class="fb-cache">未缓存</span>';
  const parts = [];
  if (it.user_suggestion) parts.push('建议：' + esc(it.user_suggestion));
  if (it.user_comment) parts.push('备注：' + esc(it.user_comment));
  const fbText = parts.length ? parts.join('\n') : '<em>（用户没写说明）</em>';
  const st = it.status || 'pending';
  let actions = '<button class="ghost" onclick="fbViewIdx(' + i + ')">查看译文</button>';
  if (st !== 'retranslating') actions += '<button class="ghost" onclick="fbRetranslate([' + it.id + '])">重新翻译</button>';
  if (st === 'done' || st === 'ignored') {
    actions += '<button class="ghost" onclick="fbMark([' + it.id + '], \'pending\')">退回待处理</button>';
  } else {
    actions += '<button class="ghost" onclick="fbMark([' + it.id + '], \'done\')">已处理</button>'
      + '<button class="ghost" onclick="fbMark([' + it.id + '], \'ignored\')">忽略</button>';
  }
  return '<tr>'
    + '<td><input type="checkbox" class="fb-select fb-row" data-id="' + it.id + '" onchange="updateFbSelAll()"></td>'
    + '<td class="hint">' + fmtTime(it.created_at) + '</td>'
    + '<td class="mono">' + esc(it.mod_id) + '<br><span class="hint">' + esc(it.lang || '') + '</span></td>'
    + '<td><span class="st retranslating" title="' + esc(it.issue_type) + '">' + esc(issueLabel(it.issue_type)) + '</span></td>'
    + '<td class="fb-feedback" title="来自 IP ' + esc(it.ip) + '">' + fbText + '</td>'
    + '<td>' + cacheCell + '</td>'
    + '<td><span class="st ' + esc(st) + '">' + (FB_STATUS_TEXT[st] || esc(st)) + '</span></td>'
    + '<td class="fb-actions">' + actions + '</td>'
    + '</tr>';
}
function fbSelected() {
  return [...document.querySelectorAll('.fb-row:checked')].map(c => parseInt(c.getAttribute('data-id'), 10)).filter(n => n > 0);
}
function fbToggleSelectAll() {
  const boxes = [...document.querySelectorAll('.fb-row')];
  const allChecked = boxes.length > 0 && boxes.every(b => b.checked);
  boxes.forEach(b => { b.checked = !allChecked; });
  updateFbSelAll();
}
function updateFbSelAll() {
  const boxes = [...document.querySelectorAll('.fb-row')];
  const n = boxes.filter(b => b.checked).length;
  const btn = document.getElementById('fb_selall');
  if (btn) btn.textContent = (boxes.length > 0 && n === boxes.length) ? '取消全选' : ('全选本页' + (n ? '（已选 ' + n + '）' : ''));
}
async function fbMark(ids, status) {
  if (!ids || !ids.length) return;
  try {
    const r = await fetch('/worker/feedback/status', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids, status }),
    });
    if (!r.ok) { alert('操作失败: HTTP ' + r.status); return; }
    await fbLoad();
    fbCounts();
  } catch (e) { alert('操作失败: ' + e.message); }
}
async function fbBatch(status) {
  const ids = fbSelected();
  if (!ids.length) { alert('先勾选要处理的反馈'); return; }
  if (!confirm('把选中的 ' + ids.length + ' 条标记为「' + (FB_STATUS_TEXT[status] || status) + '」？')) return;
  await fbMark(ids, status);
}
async function fbRetranslate(ids) {
  if (!ids || !ids.length) return;
  if (!confirm('重新翻译这 ' + ids.length + ' 个 mod？\n会清掉旧的描述+正文缓存后重翻（正文按块翻，可能要几十秒），完成后状态自动变「已处理」。')) return;
  try {
    const r = await fetch('/worker/feedback/retranslate', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) { alert('重新翻译失败: ' + (d.error || ('HTTP ' + r.status))); return; }
    await fbLoad();
    fbCounts();
  } catch (e) { alert('重新翻译失败: ' + e.message); }
}
function fbPage(delta) {
  const next = fbOffset + delta * fbLimit;
  if (next < 0) return;
  fbOffset = next;
  fbLoad();
}
// fbViewIdx 用行索引取 mod id，避免把 mod id 拼进 onclick 字符串
function fbViewIdx(i) {
  const it = fbRows[i];
  if (it) fbView(it.mod_id, it.platform);
}
// fbView 复用「缓存浏览」的详情面板：搜该 mod → 加载 → 展开第一条（描述/正文成对展示）
async function fbView(modID, platform) {
  location.hash = '#/cache';
  showView('cache');
  const q = document.getElementById('q');
  if (q) q.value = modID;
  cacheQ = modID;
  cacheKind = 'all';
  cacheOffset = 0;
  await loadCacheList();
  if (cacheRows && cacheRows.length) loadCacheDetail(0);
}

// ============== 定时器 ==============
function activeIs(name) { return activeView === name; }
function startTimers() {
  stopTimers();
  statsTimer = setInterval(() => { if (document.visibilityState === 'visible' && activeIs('overview')) refresh(); }, 10000);
  workerTimer = setInterval(() => {
    if (document.visibilityState === 'visible' && (activeIs('worker') || activeIs('overview') || activeIs('cache'))) refreshWorker();
  }, 2000);
  cacheTimer = setInterval(() => { if (document.visibilityState === 'visible' && activeIs('cache')) loadCacheList(); }, 5000);
  // 实时槽位：只在停在 worker 页且勾了自动刷新时拉，别让后台标签页一直打接口
  liveTimer = setInterval(() => {
    const box = document.getElementById('live_auto');
    if (document.visibilityState === 'visible' && activeIs('worker') && (!box || box.checked)) loadLive();
  }, 2000);
  // 反馈：角标常刷；停在审核页时连列表一起刷，方便看「重译中 -> 已处理」
  fbTimer = setInterval(() => {
    if (document.visibilityState !== 'visible') return;
    fbCounts();
    if (activeIs('feedback')) fbLoad();
  }, 10000);
  restartLogTimer();   // 日志用页面自己的"实时刷新"开关，间隔 3s
}
function stopTimers() {
  [statsTimer, workerTimer, cacheTimer, logTimer, fbTimer, liveTimer].forEach(t => { if (t) clearInterval(t); });
  statsTimer = workerTimer = cacheTimer = logTimer = fbTimer = liveTimer = null;
}
function toggleAuto() {
  autoRefresh = !autoRefresh;
  document.getElementById('autoBtn').textContent = '自动刷新: ' + (autoRefresh ? '开' : '关');
  if (autoRefresh) startTimers(); else stopTimers();
}
function refreshAll() {
  refresh();
  refreshWorker();
  fbCounts();
  if (activeIs('logs')) refreshLogs();
  if (activeIs('cache')) loadCacheList();
  if (activeIs('feedback')) fbLoad();
}

// ============== 启动 ==============
(function boot() {
  let t = null;
  try { t = localStorage.getItem('trans_theme'); } catch (e) {}
  if (!t) t = (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) ? 'dark' : 'light';
  setTheme(t);

  document.getElementById('q').addEventListener('keydown', e => { if (e.key === 'Enter') cacheSearch(); });
  document.getElementById('log_q').addEventListener('keydown', e => { if (e.key === 'Enter') loadLogs(); });
  if (!window.marked || !window.DOMPurify) {
    rawMode = true;
    document.getElementById('rawBtn').textContent = '渲染 Markdown';
    document.getElementById('md_state').textContent = 'Markdown 库未加载，已回退源码模式';
  }
  showView(currentRoute());
  loadConfig();
  refresh();
  refreshWorker();
  populateLogModels();
  loadLogs();
  fbCounts();
  startTimers();
})();
