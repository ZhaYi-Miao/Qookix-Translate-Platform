// ============== 路由（侧边栏） ==============
const VIEWS = ['overview', 'worker', 'models', 'cache', 'feedback', 'logs', 'about'];
const VIEW_TITLES = { overview: '概览', worker: '后台任务', models: '模型与配置', cache: '缓存浏览', feedback: '反馈审核', logs: '运行日志', about: '关于' };
let activeView = 'overview';

function currentRoute() {
  const h = (location.hash || '').replace(/^#\/?/, '');
  return VIEWS.includes(h) ? h : 'overview';
}
function showView(name) {
  activeView = name;
  for (const v of VIEWS) {
    const sec = document.getElementById('view-' + v);
    if (sec) sec.classList.toggle('active', v === name);
    const nav = document.getElementById('nav-' + v);
    if (nav) nav.classList.toggle('active', v === name);
  }
  const t = document.getElementById('view_title');
  if (t) t.textContent = VIEW_TITLES[name] || name;
  // 隐藏容器里的 canvas 尺寸为 0，切回来要重画一次
  if (name === 'overview') refresh();
  // 实时槽位：切进来立刻拉一次（否则会显示上一次离开时的旧状态）
  if (name === 'worker') { refreshWorker(); loadLive(); }
  if (name === 'worker') refreshWorker();
  if (name === 'cache') loadCacheList();
  if (name === 'feedback') fbLoad();
  if (name === 'logs') refreshLogs();
}
window.addEventListener('hashchange', () => showView(currentRoute()));

// ============== 主题 ==============
function setTheme(t) {
  document.documentElement.setAttribute('data-theme', t);
  try { localStorage.setItem('trans_theme', t); } catch (e) {}
  const b = document.getElementById('themeBtn');
  if (b) b.textContent = t === 'dark' ? '亮色模式' : '暗色模式';
}
function toggleTheme() {
  const cur = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
  setTheme(cur);
  if (activeView === 'overview') refresh();   // 图表网格色跟随主题
}
function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

// ============== 通用状态 ==============
let autoRefresh = true;
let statsTimer = null, logTimer = null, workerTimer = null, cacheTimer = null, fbTimer = null, liveTimer = null;
const MASK = '********';
let providers = [];  // {name,type,base_url,api_key,concurrent,timeout_sec,prevName}
let chain = [];      // {provider,model,daily_limit,out_tok,share}
const charts = { requests: null, cache: null, llm: null, feedback: null, mods: null };
const palette = ['#2563eb', '#0d9488', '#d97706', '#dc2626', '#7c3aed', '#0284c7', '#db2777', '#65a30d', '#ea580c', '#4f46e5'];
let lastModelStats = {};   // provider:model -> {calls,failed,avg_ms...}，日志页用来显示每个模型的今日状况

function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }
function setMetric(id, value, level) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = value;
  el.className = 'value' + (level ? ' ' + level : '');
}
function destroyChart(name) { if (charts[name]) { charts[name].destroy(); charts[name] = null; } }
function showEmptyText(id, text) {
  const el = document.getElementById(id);
  if (!el) return;
  const c = el.getContext('2d');
  c.clearRect(0, 0, 9999, 9999);
  c.font = '14px sans-serif';
  c.fillStyle = cssVar('--faint') || '#9ca3af';
  c.textAlign = 'center';
  c.fillText(text, el.clientWidth / 2, 130);
}
function renderKV(el, obj) {
  if (!el) return;
  const keys = Object.keys(obj || {});
  if (keys.length === 0) { el.innerHTML = '<tbody><tr><td class="empty">无数据</td></tr></tbody>'; return; }
  let html = '<thead><tr><th>项</th><th>值</th></tr></thead><tbody>';
  for (const k of keys) html += `<tr><td>${esc(k)}</td><td>${esc(obj[k])}</td></tr>`;
  el.innerHTML = html + '</tbody>';
}
function renderList(el, arr) {
  if (!el) return;
  if (!arr || arr.length === 0) { el.innerHTML = '<div class="empty">无数据</div>'; return; }
  const keys = Object.keys(arr[0]);
  let html = '<table><thead><tr>' + keys.map(k => `<th>${esc(k)}</th>`).join('') + '</tr></thead><tbody>';
  for (const item of arr) html += '<tr>' + keys.map(k => `<td>${esc(item[k])}</td>`).join('') + '</tr>';
  el.innerHTML = html + '</tbody></table>';
}
function fmtTime(ts) { return (ts && ts > 0) ? new Date(ts * 1000).toLocaleString() : '—'; }
function fmtRate(v) { return (v >= 10 ? v.toFixed(0) : v.toFixed(1)); }
function fmtDur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (h > 0) return h + ' 小时 ' + m + ' 分';
  if (m > 0) return m + ' 分 ' + s + ' 秒';
  return s + ' 秒';
}

// ============== 熔断横幅 ==============
function renderBreaker(status, breakerStatus) {
  const banner = document.getElementById('breaker_banner');
  const text = document.getElementById('breaker_text');
  const details = document.getElementById('breaker_details');
  const openList = (breakerStatus || []).filter(b => b.circuit_open);
  if (!status) { banner.className = 'breaker-banner ok'; text.textContent = '模型熔断器：无数据'; details.textContent = ''; return; }
  if (openList.length > 0) {
    banner.className = 'breaker-banner open';
    details.textContent = openList.map(b => {
      const until = b.open_until || 0;
      if (!until) return b.key;
      const sec = Math.max(0, until - Math.floor(Date.now() / 1000));
      return `${b.key}(${Math.floor(sec / 60)}分${sec % 60}秒)`;
    }).join('，');
    text.textContent = `模型熔断器：${openList.length} 个模型熔断中`;
  } else {
    banner.className = 'breaker-banner ok';
    text.textContent = '模型熔断器：正常';
    details.textContent = '所有模型工作正常';
  }
}

// ============== 图表 ==============
function drawDoughnut(id, labels, data, name) {
  destroyChart(name);
  charts[name] = new Chart(document.getElementById(id).getContext('2d'), {
    type: 'doughnut',
    data: { labels, datasets: [{ data, backgroundColor: palette.slice(0, labels.length), borderWidth: 0 }] },
    options: { responsive: true, maintainAspectRatio: false,
      plugins: { legend: { position: 'right', labels: { boxWidth: 12, padding: 12, font: { size: 12 }, color: cssVar('--muted') } } } },
  });
}
function drawBar(id, labels, data, name, horizontal) {
  destroyChart(name);
  const grid = cssVar('--chart-grid') || '#f3f4f6';
  charts[name] = new Chart(document.getElementById(id).getContext('2d'), {
    type: 'bar',
    data: { labels, datasets: [{ data, backgroundColor: palette[0], borderRadius: 6 }] },
    options: { indexAxis: horizontal ? 'y' : 'x', responsive: true, maintainAspectRatio: false,
      plugins: { legend: { display: false } },
      scales: { x: { grid: { display: false }, ticks: { font: { size: 11 }, color: cssVar('--muted') } },
                y: { grid: { color: grid }, ticks: { font: { size: 11 }, color: cssVar('--muted') } } } },
  });
}

// ============== /stats ==============
async function refresh() {
  try {
    const sel = document.getElementById('stat_range');
    const rng = (sel && sel.value) || 'today';
    const r = await fetch('/stats?range=' + encodeURIComponent(rng), { headers: { 'Accept': 'application/json' } });
    if (!r.ok) { document.getElementById('updated').textContent = '错误: HTTP ' + r.status; return; }
    const data = await r.json();
    const rangeTxt = (data.range_label || '今天') +
      (data.range_from && data.range_from !== data.range_to
        ? `（${data.range_from} ~ ${data.range_to}，${data.days || 0} 天）`
        : (data.range_from ? `（${data.range_from}）` : ''));
    document.getElementById('updated').textContent =
      '最后更新: ' + new Date().toLocaleTimeString() + ' · 统计区间: ' + rangeTxt;
    const mut = document.getElementById('model_usage_title');
    if (mut) mut.textContent = '模型用量与熔断（' + (data.range_label || '今天') + '）';

    renderBreaker(data.breaker_overview, data.breaker_status);
    renderModelUsage(data.breaker_status, data.model_stats);

    const reqs = data.requests || {};
    setMetric('m_total', reqs.total || 0);
    const hitRateText = data.cache_hit_rate || 'N/A';
    setMetric('m_hitrate', hitRateText, hitRateText === 'N/A' ? '' : (parseFloat(hitRateText) > 80 ? 'good' : 'warn'));

    // 一律按 provider:model 聚合：同一个类型下往往挂了多个账号，不能按类型合并
    const mstats = data.model_stats || {};
    lastModelStats = mstats;
    let okSum = 0, failSum = 0, callSum = 0, msWeighted = 0;
    for (const f of Object.keys(mstats)) {
      const s = mstats[f] || {};
      const c = s.calls || 0, fa = s.failed || 0;
      okSum += Math.max(0, c - fa);
      failSum += fa;
      callSum += c;
      msWeighted += (s.avg_ms || 0) * c;
    }
    setMetric('m_llm_ok', okSum);
    setMetric('m_llm_failed', failSum, failSum > 0 ? 'bad' : 'good');
    const rl = data.rate_limit_hits || 0;
    setMetric('m_ratelimit', rl, rl > 100 ? 'bad' : rl > 20 ? 'warn' : '');
    setMetric('m_llmlatency', (callSum > 0 ? Math.round(msWeighted / callSum) : 0) + ' ms');

    const pickBody = (ep, flag) => reqs[ep + (flag ? '_with_body' : '_no_body')] || 0;
    setMetric('m_withbody', pickBody('translate_mod', true) + pickBody('translate_mods', true));
    setMetric('m_nobody', pickBody('translate_mod', false) + pickBody('translate_mods', false));

    const kl = data.kind_llm || {};
    const cache = data.cache || {};
    renderKV(document.getElementById('t_kind_llm'), {
      '描述 调用次数': kl.desc_calls || 0, '描述 失败数': kl.desc_failed || 0, '描述 输入字符': kl.desc_chars || 0,
      '正文 调用次数': kl.body_calls || 0, '正文 失败数': kl.body_failed || 0, '正文 输入字符': kl.body_chars || 0,
    });
    const descTotal = (cache.desc_hit || 0) + (cache.desc_miss || 0);
    const bodyTotal = (cache.body_hit || 0) + (cache.body_miss || 0);
    renderKV(document.getElementById('t_kind_cache'), {
      '描述 命中': cache.desc_hit || 0, '描述 未命中': cache.desc_miss || 0,
      '描述 命中率': descTotal > 0 ? ((cache.desc_hit / descTotal) * 100).toFixed(1) + '%' : 'N/A',
      '正文 命中': cache.body_hit || 0, '正文 未命中': cache.body_miss || 0,
      '正文 命中率': bodyTotal > 0 ? ((cache.body_hit / bodyTotal) * 100).toFixed(1) + '%' : 'N/A',
    });

    const reqKeys = Object.keys(reqs).filter(k => k !== 'total' && reqs[k] > 0);
    if (reqKeys.length > 0) drawDoughnut('chart_requests', reqKeys.map(k => k + ': ' + reqs[k]), reqKeys.map(k => reqs[k]), 'requests');
    else { destroyChart('requests'); showEmptyText('chart_requests', '暂无请求'); }

    const cacheHit = cache.hit || 0, cacheMiss = cache.miss || 0;
    document.getElementById('hitrate_sub').textContent = data.cache_hit_rate || '';
    if (cacheHit + cacheMiss > 0) drawDoughnut('chart_cache', ['命中', '未命中'], [cacheHit, cacheMiss], 'cache');
    else { destroyChart('cache'); showEmptyText('chart_cache', '暂无缓存数据'); }

    // 按模型名称分布；Modrinth 调用/404/429 属于另一类数据，不混进这张图
    const llmRows = Object.keys(mstats)
      .map(f => ({ name: f, calls: (mstats[f] || {}).calls || 0, failed: (mstats[f] || {}).failed || 0 }))
      .filter(x => x.calls > 0)
      .sort((a, b) => b.calls - a.calls);
    if (llmRows.length > 0) {
      drawBar('chart_llm',
        llmRows.map(x => x.name + (x.failed > 0 ? '（失败 ' + x.failed + '）' : '')),
        llmRows.map(x => x.calls), 'llm');
    } else { destroyChart('llm'); showEmptyText('chart_llm', '暂无 LLM 数据'); }

    const fLabels = [], fValues = [];
    for (const [k, v] of Object.entries(data.stale_feedback || {})) if (v > 0) { fLabels.push('stale:' + k); fValues.push(v); }
    for (const [k, v] of Object.entries(data.quality_feedback || {})) if (v > 0) { fLabels.push('quality:' + k); fValues.push(v); }
    if (fLabels.length > 0) drawBar('chart_feedback', fLabels, fValues, 'feedback');
    else { destroyChart('feedback'); showEmptyText('chart_feedback', '暂无反馈数据'); }

    const mods = data.top_mods || [];
    document.getElementById('mods_count').textContent = mods.length + ' 个';
    if (mods.length > 0) drawBar('chart_mods', mods.slice(0, 15).map(m => m.mod_id), mods.slice(0, 15).map(m => m.count), 'mods', true);
    else { destroyChart('mods'); showEmptyText('chart_mods', '暂无 Mod 数据'); }

    renderKV(document.getElementById('t_requests'), reqs);
    const llm = data.llm || {};
    renderKV(document.getElementById('t_llm'), {
      openai_calls: llm.openai_calls || 0, openai_failed: llm.openai_failed || 0,
      gemini_calls: llm.gemini_calls || 0, gemini_failed: llm.gemini_failed || 0,
      modrinth_calls: llm.modrinth_calls || 0, modrinth_404: llm.modrinth_404 || 0, modrinth_429: llm.modrinth_429 || 0,
      openai_avg_ms: data.openai_avg_ms || 0, gemini_avg_ms: data.gemini_avg_ms || 0,
    });
    renderKV(document.getElementById('t_stale'), data.stale_feedback);
    renderKV(document.getElementById('t_quality'), data.quality_feedback);

    const ips = data.top_ips || [];
    document.getElementById('ips_count').textContent = ips.length + ' 个';
    renderList(document.getElementById('ips_wrap'), ips);
  } catch (e) {
    document.getElementById('updated').textContent = '错误: ' + e.message;
  }
}

// ============== 模型用量与熔断明细 ==============
function renderModelUsage(breakerStatus, modelStats) {
  const wrap = document.getElementById('t_model_usage');
  if (!wrap) return;
  modelStats = modelStats || {};
  const breakerMap = {};
  for (const b of (breakerStatus || [])) breakerMap[b.key] = b;

  const rows = [];
  chain.forEach((st, idx) => {
    const stat = modelStats[st.provider + ':' + st.model];
    rows.push({
      idx: idx + 1, provider: st.provider, model: st.model,
      calls: stat ? stat.calls : 0, failed: stat ? stat.failed : 0,
      avg_ms: (stat && stat.avg_ms) || 0,
      limit: (stat && stat.daily_limit) || st.daily_limit || 0,
      breaker: breakerMap[st.provider + '/' + st.model],
    });
  });
  for (const field of Object.keys(modelStats)) {
    if (rows.some(r => r.provider + ':' + r.model === field)) continue;
    const [pv, mo] = field.split(':');
    if (!pv || !mo) continue;
    rows.push({
      idx: '—', provider: pv, model: mo,
      calls: modelStats[field].calls, failed: modelStats[field].failed,
      avg_ms: modelStats[field].avg_ms || 0,
      limit: modelStats[field].daily_limit || 0,
      breaker: breakerMap[field.replace(':', '/')],
    });
  }
  if (rows.length === 0) { wrap.innerHTML = '<tbody><tr><td class="empty">无模型 / 配置尚未加载</td></tr></tbody>'; return; }

  let html = '<thead><tr><th>#</th><th>Provider</th><th>Model</th><th>调用</th><th>失败</th><th>平均延迟</th><th>每日上限(Worker)</th><th>熔断</th></tr></thead><tbody>';
  for (const r of rows) {
    const b = r.breaker || {};
    let brHtml;
    if (b.circuit_open) {
      const sec = Math.max(0, (b.open_until || 0) - Math.floor(Date.now() / 1000));
      brHtml = `<span style="color:var(--bad);font-weight:600;">● 熔断 ${Math.floor(sec / 60)}分${sec % 60}秒</span>`;
    } else if (b.fail_count) {
      brHtml = `<span style="color:var(--warn);font-weight:600;">${b.fail_count} 连续失败</span>`;
    } else {
      brHtml = '<span style="color:var(--good);">−</span>';
    }
    const failedPct = r.calls > 0 ? Math.round(r.failed / Math.max(r.calls, 1) * 100) + '%' : '—';
    const limitTxt = r.limit > 0 ? r.limit : '不限';
    const over = r.limit > 0 && r.calls >= r.limit ? ' style="color:var(--bad);font-weight:600;"' : '';
    html += `<tr>
      <td class="mono">${r.idx}</td>
      <td class="mono">${esc(r.provider)}</td>
      <td class="mono">${esc(r.model)}</td>
      <td>${r.calls}</td>
      <td>${r.failed} (<span ${r.failed > 0 ? 'style="color:var(--bad);"' : ''}>${failedPct}</span>)</td>
      <td>${r.avg_ms ? r.avg_ms + ' ms' : '—'}</td>
      <td${over}>${limitTxt}</td>
      <td>${brHtml}</td>
    </tr>`;
  }
  wrap.innerHTML = html + '</tbody>';
}

// ============== 速度 / 剩余时间 ==============
// 速度以后端口径为准：/worker/status 的 throughput（服务端每 10s 采样"累计已翻译字符数"算出的
// 字符/分 + 个/分）。前端只在后端样本不足（服务刚起 / Redis 不可用）时用轮询差值兜底。
let speedSamples = [];
function fmtChars(n) {
  n = Math.round(n || 0);
  if (n >= 10000) return (n / 1000).toFixed(1) + 'k';
  return n.toLocaleString('zh-CN');
}
function updateSpeed(p, total, current, tp) {
  const now = Date.now() / 1000;
  speedSamples.push({ t: now, current: current, ok: p.ok || 0 });
  while (speedSamples.length > 2 && now - speedSamples[0].t > 180) speedSamples.shift();

  document.getElementById('worker_elapsed').textContent = p.started_at ? fmtDur(now - p.started_at) : '—';

  const first = speedSamples[0];
  const dtLocal = first ? now - first.t : 0;
  const enoughLocal = !!(first && dtLocal >= 20);
  const localOK = enoughLocal ? ((p.ok || 0) - first.ok) / dtLocal * 60 : 0;      // 真翻成功的 mod/分
  const localProc = enoughLocal ? (current - first.current) / dtLocal * 60 : 0;  // 处理速率（含跳过缓存）

  const speedEl = document.getElementById('worker_speed');
  const etaEl = document.getElementById('worker_eta');
  const srcEl = document.getElementById('worker_speed_src');
  const serverReady = !!(tp && tp.ready);
  const remain = total - current;
  // 剩余时间按"真翻成功的 mod/分"估：用处理速率会把跳过缓存也算进去，估得过于乐观
  const modsPerMin = serverReady ? (tp.mods_per_min || 0) : localOK;
  const eta = modsPerMin > 0.02 ? remain / modsPerMin * 60 : 0;

  if (serverReady) {
    speedEl.textContent = fmtChars(tp.chars_per_min) + ' 字符/分';
    etaEl.textContent = eta > 0 ? fmtDur(eta) : '—';
    srcEl.textContent = `（服务端 · 近 ${Math.round(tp.window_sec || 0)} 秒 ${fmtChars(tp.chars_per_min)} 字符/分、`
      + `${fmtRate(tp.mods_per_min)} 个/分 · 今日累计 ${fmtChars(tp.chars_total)} 字符 / ${tp.mods_total} 个 mod）`;
  } else if (enoughLocal && (localOK > 0.02 || localProc > 0.02)) {
    speedEl.textContent = fmtRate(localOK) + ' 个/分';
    etaEl.textContent = eta > 0 ? fmtDur(eta) : '—';
    srcEl.textContent = `（后端样本累积中，暂用本页估算：近 ${Math.round(dtLocal)} 秒处理 ${fmtRate(localProc)} 个/分）`;
  } else {
    speedEl.textContent = enoughLocal ? '0' : '计算中…';
    etaEl.textContent = '计算中…';
    srcEl.textContent = '（后端每 10s 采样一次，约需运行 20 秒后才有速率）';
  }

  const ovP = document.getElementById('ov_worker_progress');
  const ovS = document.getElementById('ov_worker_speed');
  const ovE = document.getElementById('ov_worker_eta');
  if (ovP) ovP.textContent = `${current}/${total}`;
  if (ovS) ovS.textContent = serverReady ? (fmtChars(tp.chars_per_min) + ' 字符/分')
    : (localOK > 0.02 ? fmtRate(localOK) + ' 个/分' : '—');
  if (ovE) ovE.textContent = eta > 0 ? fmtDur(eta) : '—';
}

// ============== /live 实时槽位 ==============
// 一个方块 = 一个并发槽位：实心=正在调用上游，空白=空闲，斜纹=在等每分钟名额。
// 斜纹单独列出而不是占槽位，因为"等名额"发生在拿槽位之前 —— 这正是免费账号的排队真相。
const LIVE_KIND = { desc: '描述', body: '正文', test: '测试', other: '其他' };
function liveKindName(k) { return LIVE_KIND[k] || k || '—'; }

async function loadLive() {
  const wrap = document.getElementById('live_wrap');
  if (!wrap) return;
  try {
    const r = await fetch('/live');
    if (!r.ok) { wrap.innerHTML = '<div class="hint">读取失败：HTTP ' + r.status + '</div>'; return; }
    renderLive(await r.json());
  } catch (e) {
    wrap.innerHTML = '<div class="hint">读取失败：' + esc(e.message) + '</div>';
  }
}

function renderLive(d) {
  const t = d.totals || {}, w = d.worker || {};
  const sub = document.getElementById('live_sub');
  if (sub) {
    const parts = [d.now || ''];
    if (t.inflight || t.waiting_rpm || t.waiting_slot) {
      parts.push('在飞 ' + (t.inflight || 0), '等名额 ' + (t.waiting_rpm || 0), '等槽位 ' + (t.waiting_slot || 0));
    } else {
      parts.push('空闲');
    }
    if (w.running) parts.push('worker 运行中' + (w.phase ? '（' + esc(w.phase) + '）' : ''));
    sub.textContent = parts.join(' · ');
  }

  const wrap = document.getElementById('live_wrap');
  if (!wrap) return;
  const lim = d.limits || {};
  let html = (d.accounts || []).map(a => {
    const conc = a.concurrent || 0, inflight = a.inflight || 0;
    const rpmW = a.rpm_waiting ? a.rpm_waiting : null;
    const slotW = a.slot_waiting ? a.slot_waiting : null;

    let blocks = '';
    for (let i = 0; i < conc; i++) blocks += (i < inflight) ? '<i class="sl b"></i>' : '<i class="sl"></i>';
    const rpmN = rpmW ? rpmW.count : 0;
    for (let i = 0; i < Math.min(rpmN, 10); i++) blocks += '<i class="sl w"></i>';
    if (rpmN > 10) blocks += '<span class="live-tag warn">+' + (rpmN - 10) + ' 等名额</span>';

    const tags = [];
    (a.calls || []).forEach(c => {
      tags.push('<span class="live-tag">' + esc(c.model) + ' · ' + liveKindName(c.kind) + ' ' + c.sec + 's</span>');
    });
    if (rpmW) tags.push('<span class="live-tag warn" title="' + esc(rpmW.note || '') + '">等每分钟名额 ' + rpmN + ' 个（最久 ' + rpmW.max_sec + 's）</span>');
    if (slotW) tags.push('<span class="live-tag bad" title="' + esc(slotW.note || '') + '">等并发槽位 ' + slotW.count + ' 个（最久 ' + slotW.max_sec + 's）</span>');
    if (a.rpm) tags.push('<span class="live-tag">名额 ' + a.rpm.tokens + '/' + a.rpm.limit + '/分</span>');
    (a.cooling || []).forEach(c => tags.push('<span class="live-tag bad">冷却 ' + esc(c.model) + ' 还剩 ' + c.left_sec + 's</span>'));
    (a.breakers || []).forEach(b => tags.push('<span class="live-tag bad">熔断 ' + esc(b.model) + ' 还剩 ' + b.left_sec + 's</span>'));
    if (!tags.length) tags.push('<span class="live-tag idle">空闲</span>');

    const peak = a.peak_inflight || 0;
    return '<div class="live-row">'
      + '<div class="live-name">' + esc(a.account)
      + '<span class="sub">' + inflight + '/' + conc + ' 在用 · 峰值 ' + peak + '</span></div>'
      + '<div><div class="slots">' + blocks + '</div><div class="live-tags">' + tags.join('') + '</div></div>'
      + '</div>';
  }).join('');

  html += '<div class="hint" style="margin-top:8px;">等名额/等槽位超过 '
    + (lim.rpm_wait_timeout_sec || '—') + ' 秒就会放弃当前模型、换下一个（这就是"排队失败"的边界）。峰值是本次进程内见过的最高占用。</div>';
  wrap.innerHTML = html;
}

// ============== /worker/status ==============
const PHASE_NAMES = { preheat: '预热', refresh: '检测更新', 'body-retry': '正文补翻回访' };

async function refreshWorker() {
  try {
    const r = await fetch('/worker/status');
    if (!r.ok) return;
    const s = await r.json();
    const p = s.progress || {};

    document.getElementById('worker_toggle').checked = !!s.enabled;
    document.getElementById('worker_body').checked = !!s.body_cache;
    document.getElementById('worker_parallelism').textContent = s.parallelism || 0;

    const phaseText = p.phase || PHASE_NAMES[p.type] || p.type || '';
    // 多来源预热时要能一眼看出当前跑的是哪个来源
    const srcPrefix = p.platform ? srcLabel(p.platform) + ' · ' : '';
    // 停止时补一句"下一步怎么办"：开关还开着但没在跑，最容易让人以为卡死了
    const stoppedHint = s.enabled ? '（开关仍开着，点「预热」继续）' : '';
    const statusText = document.getElementById('worker_status_text');
    if (s.running) statusText.innerHTML = '<span class="running-dot"></span>运行中 · ' + srcPrefix + phaseText;
    else if (p.status === 'done') statusText.textContent = '已完成' + stoppedHint;
    else if (p.status === 'stopped') statusText.textContent = '已停止' + stoppedHint;
    else statusText.textContent = '空闲';
    const ovStatus = document.getElementById('ov_worker_status');
    if (ovStatus) ovStatus.textContent = s.running ? '运行中 · ' + srcPrefix + phaseText : (p.status === 'done' ? '已完成' : '空闲');

    const total = p.total || 0, current = p.current || 0;
    // 只要还有进度快照就显示（含刚停止的情况），避免"任务跑过却一片空白"
    const showProgress = total > 0 && (s.running || p.status === 'running' || p.status === 'stopped');
    if (showProgress) {
      document.getElementById('worker_progress_wrap').style.display = 'block';
      const pct = Math.round(current / total * 100);
      const fill = document.getElementById('worker_progress_fill');
      fill.style.width = pct + '%';
      fill.textContent = pct + '%';
      document.getElementById('worker_progress_text').textContent =
        (s.running && phaseText ? srcPrefix + phaseText + ' · ' : '') + `${current}/${total}` + (p.mod_id ? ` · ${p.mod_id}` : '') + (s.running ? '' : '（当前未在运行）');
      if (s.running) updateSpeed(p, total, current, s.throughput);
      else {
        document.getElementById('worker_speed').textContent = '未运行';
        document.getElementById('worker_eta').textContent = '—';
        document.getElementById('worker_speed_src').textContent = '';
        speedSamples = [];
      }
    } else {
      document.getElementById('worker_progress_wrap').style.display = 'none';
      speedSamples = [];
    }

    document.getElementById('worker_ok').textContent = p.ok || 0;
    document.getElementById('worker_fail').textContent = p.fail || 0;
    document.getElementById('worker_today_ok').textContent = s.today_ok || 0;
    document.getElementById('worker_today_fail').textContent = s.today_fail || 0;
    document.getElementById('worker_last_run').textContent = s.last_run > 0
      ? new Date(s.last_run * 1000).toLocaleString('zh-CN') : '从未运行';

    const running = !!s.running, enabled = !!s.enabled;
    document.getElementById('btn_preheat').disabled = running || !enabled;
    document.getElementById('btn_refresh').disabled = running || !enabled;
    document.getElementById('btn_stop').disabled = !running;

    const recent = p.recent || [];
    if (recent.length > 0) {
      document.getElementById('worker_recent_wrap').style.display = 'block';
      document.getElementById('worker_recent').textContent = recent.slice().reverse().join('\n');
    } else {
      document.getElementById('worker_recent_wrap').style.display = 'none';
    }
    // 来源预热控制行随状态一起刷新（开关/游标/步长/自动续跑都在里面）
    renderPreheatSources(s.preheat, s.autocontinue);
  } catch (e) { /* 忽略 */ }
}
