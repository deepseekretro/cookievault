// CookieVault side panel: virtual list + infinite scroll + search.

const $ = (id) => document.getElementById(id);
const ROW_H = 112;         // snapshot card height (106 + 6 gap)
const COOKIE_ROW_H = 44;   // cookie row height (38 + 6 gap)
const OVERSCAN = 5;
const PAGE_SIZE = 50;
const PREFETCH_PX = 400;

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function highlight(text, q) {
  const safe = esc(text);
  if (!q) return safe;
  const i = safe.toLowerCase().indexOf(esc(q).toLowerCase());
  if (i < 0) return safe;
  return safe.slice(0, i) + '<mark>' + safe.slice(i, i + esc(q).length) + '</mark>' + safe.slice(i + esc(q).length);
}
function timeAgo(ts) {
  const s = Math.floor(Date.now() / 1000) - Math.floor(ts / 1000);
  if (s < 60) return '刚刚';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.classList.remove('hidden');
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.add('hidden'), 2600);
}

async function api(path, opts = {}) {
  const r = await chrome.runtime.sendMessage({ type: 'API', path, method: opts.method, body: opts.body });
  if (!r.ok) { const e = new Error(r.error); e.code = r.code; e.status = r.status; throw e; }
  return r.data;
}

// ---------- generic virtual list ----------
class VirtualList {
  constructor({ scrollEl, spacerEl, windowEl, rowHeight, renderRow }) {
    this.scrollEl = scrollEl; this.spacerEl = spacerEl; this.windowEl = windowEl;
    this.rowHeight = rowHeight; this.renderRow = renderRow;
    this.items = [];
    this._raf = 0;
    scrollEl.addEventListener('scroll', () => {
      cancelAnimationFrame(this._raf);
      this._raf = requestAnimationFrame(() => this.render());
    }, { passive: true });
  }
  setItems(items) { this.items = items; this.updateSpacer(); this.render(); }
  appendItems(items) { this.items = this.items.concat(items); this.updateSpacer(); this.render(); }
  removeById(id) { this.items = this.items.filter((i) => i.id !== id); this.updateSpacer(); this.render(); }
  updateSpacer() { this.spacerEl.style.height = (this.items.length * this.rowHeight) + 'px'; }
  render() {
    const { scrollEl, rowHeight } = this;
    const start = Math.max(0, Math.floor(scrollEl.scrollTop / rowHeight) - OVERSCAN);
    const visible = Math.ceil(scrollEl.clientHeight / rowHeight) + OVERSCAN * 2;
    const end = Math.min(this.items.length, start + visible);
    this.windowEl.style.transform = `translateY(${start * rowHeight}px)`;
    const frag = document.createDocumentFragment();
    for (let i = start; i < end; i++) frag.appendChild(this.renderRow(this.items[i], i));
    this.windowEl.replaceChildren(frag);
  }
}

// ---------- snapshot list state ----------
const state = {
  itemsById: new Map(),
  items: [],
  cursor: '',
  hasMore: false,
  total: 0,
  loading: false,
  currentDomain: '',
  detailSnapshot: null,
  cookies: [],
  groups: [],
};

// Main (registrable) domain, mirroring the server's grouping:
// www/sub.example.com -> example.com, sub.example.co.uk -> example.co.uk.
const SECOND_LEVEL_SUFFIXES = new Set(['co.uk','org.uk','ac.uk','gov.uk','com.cn','net.cn','org.cn','gov.cn','edu.cn','com.au','net.au','org.au','co.jp','ne.jp','co.kr','com.hk','com.tw','co.nz','co.za','com.br']);
function mainDomain(host) {
  const h = String(host || '').replace(/^\./, '').toLowerCase();
  const parts = h.split('.').filter(Boolean);
  if (parts.length <= 2) return h;
  const suffix = parts.slice(-2).join('.');
  if (SECOND_LEVEL_SUFFIXES.has(suffix)) return parts.slice(-3).join('.');
  return parts.slice(-2).join('.');
}

// Auto grouping: groups come from the server (snapshots grouped by
// main domain) — the user never has to fill in a group.
async function loadGroups() {
  try {
    const data = await api('/v1/snapshots/groups');
    state.groups = data.items || [];
  } catch { state.groups = []; }
  const sel = $('filter-group');
  const cur = sel.value;
  sel.innerHTML = '<option value="">全部站点</option>' +
    state.groups.map((g) => `<option value="${esc(g.main_domain)}">${esc(g.main_domain)}（${g.count}）</option>`).join('');
  sel.value = cur;
}
$('filter-group').addEventListener('change', () => loadSnapshots(true));

const snapList = new VirtualList({
  scrollEl: $('scroll'), spacerEl: $('spacer'), windowEl: $('window'),
  rowHeight: ROW_H,
  renderRow: (snap) => {
    const q = $('search').value.trim();
    const card = document.createElement('div');
    card.className = 'card';
    card.innerHTML = `
      <div class="row1"><span class="label">${highlight(snap.label, q)}</span>
        <span class="muted">${snap.cookie_count} 个 Cookie</span></div>
      <div class="row2"><span>${esc(mainDomain(snap.domain))} · ${highlight(snap.domain, q)} · ${timeAgo(snap.last_used_at)}</span></div>
      <div class="row3 actions">
        <button class="btn small primary" data-act="restore">恢复</button>
        <button class="btn small" data-act="compare">对比</button>
        <button class="btn small" data-act="export">导出</button>
        <button class="btn small" data-act="rename">改名</button>
        <button class="btn small danger" data-act="delete">删除</button>
      </div>`;
    card.addEventListener('click', (e) => {
      const act = e.target.closest('[data-act]')?.dataset.act;
      if (act === 'restore') { e.stopPropagation(); openDetail(snap, true); return; }
      if (act === 'compare') { e.stopPropagation(); openCompare(snap); return; }
      if (act === 'export') { e.stopPropagation(); exportSnapshot(snap); return; }
      if (act === 'rename') { e.stopPropagation(); renameSnapshot(snap); return; }
      if (act === 'delete') { e.stopPropagation(); deleteSnapshot(snap); return; }
      openDetail(snap, false);
    });
    return card;
  },
});

function syncList() {
  snapList.setItems(state.items);
  $('count-text').textContent = `已加载 ${state.items.length} / 共 ${state.total} 条`;
  $('to-top').classList.toggle('hidden', state.items.length < 20);
}

async function loadSnapshots(reset) {
  if (state.loading) return;
  if (reset) {
    await refreshCurrentDomain();
    state.itemsById.clear(); state.items = []; state.cursor = ''; state.hasMore = false; state.total = 0;
    $('scroll').scrollTop = 0;
    syncList();
    $('list-status').textContent = '加载中…';
  }
  state.loading = true;
  try {
    const params = new URLSearchParams({
      limit: String(PAGE_SIZE),
      sort: $('sort').value,
    });
    const q = $('search').value.trim();
    if (q) params.set('q', q);
    if ($('filter-group').value) params.set('domain', $('filter-group').value);
    // Filter by the current site's MAIN domain so snapshots stored under
    // the parent domain (e.g. example.com) also match a sub-domain tab
    // (e.g. sub.example.com). The server matches domain with LIKE, so
    // passing the main domain covers all its sub-domains.
    if ($('filter-current-domain').checked && state.currentDomain) params.set('domain', mainDomain(state.currentDomain));
    if (state.cursor) params.set('cursor', state.cursor);

    const data = await api('/v1/snapshots?' + params.toString());
    state.total = data.total;
    let added = 0;
    for (const it of data.items || []) {
      if (!state.itemsById.has(it.id)) { state.itemsById.set(it.id, it); state.items.push(it); added++; }
    }
    state.cursor = data.next_cursor || '';
    state.hasMore = !!data.has_more;
    syncList();
    $('list-status').textContent = state.items.length === 0
      ? '没有找到快照。点击「＋ 保存当前」保存第一个登录状态。'
      : state.hasMore ? '' : `已加载全部 ${state.total} 条`;
    $('locked-banner').classList.add('hidden');
    $('error-banner').classList.add('hidden');
  } catch (e) {
    if (e.code === 'vault_locked') {
      $('locked-banner').classList.remove('hidden');
      $('list-status').textContent = '已锁定';
    } else {
      $('error-text').textContent = '加载失败：' + e.message;
      $('error-banner').classList.remove('hidden');
      $('list-status').textContent = '加载失败 [重试]';
    }
  } finally {
    state.loading = false;
  }
}

// Infinite scroll: IntersectionObserver sentinel + scroll proximity fallback.
const sentinelIO = new IntersectionObserver((entries) => {
  if (entries.some((e) => e.isIntersecting) && state.hasMore && !state.loading) loadSnapshots(false);
}, { root: $('scroll'), rootMargin: '400px' });
sentinelIO.observe($('end-sentinel'));
$('scroll').addEventListener('scroll', () => {
  const el = $('scroll');
  if (el.scrollHeight - el.scrollTop - el.clientHeight < PREFETCH_PX && state.hasMore && !state.loading) {
    loadSnapshots(false);
  }
}, { passive: true });

// ---------- inline unlock (data tab, no need to go to settings) ----------
async function doUnlock() {
  const pw = $('unlock-pw').value;
  if (!pw) { $('unlock-status').textContent = '请输入主密码'; return; }
  $('unlock-status').textContent = '解锁中…';
  try {
    await api('/v1/auth/unlock', { method: 'POST', body: { password: pw } });
    $('unlock-pw').value = '';
    $('unlock-status').textContent = '';
    $('locked-banner').classList.add('hidden');
    toast('已解锁');
    await loadGroups();
    loadSnapshots(true);
  } catch (e) {
    $('unlock-status').textContent = '解锁失败：' + e.message;
  }
}
$('unlock-btn').addEventListener('click', doUnlock);
$('unlock-pw').addEventListener('keydown', (e) => { if (e.key === 'Enter') doUnlock(); });

// ---------- search (debounced) ----------
let searchTimer = 0;
$('search').addEventListener('input', () => {
  $('search-clear').classList.toggle('hidden', !$('search').value);
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => loadSnapshots(true), 300);
});
$('search-clear').addEventListener('click', () => { $('search').value = ''; $('search-clear').classList.add('hidden'); loadSnapshots(true); $('search').focus(); });
document.addEventListener('keydown', (e) => {
  if (e.key === '/' && document.activeElement.tagName !== 'INPUT') { e.preventDefault(); $('search').focus(); }
  if (e.key === 'Escape' && document.activeElement === $('search')) { $('search').value = ''; loadSnapshots(true); }
});
$('sort').addEventListener('change', () => loadSnapshots(true));
$('filter-current-domain').addEventListener('change', async () => {
  if ($('filter-current-domain').checked) await refreshCurrentDomain();
  loadSnapshots(true);
});

// Always read the active tab's domain fresh (it changes as the user navigates).
async function refreshCurrentDomain() {
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs[0]?.url) state.currentDomain = new URL(tabs[0].url).hostname;
  } catch { /* ignore */ }
  return state.currentDomain;
}
$('error-retry').addEventListener('click', () => loadSnapshots(true));
$('to-top').addEventListener('click', () => $('scroll').scrollTo({ top: 0, behavior: 'smooth' }));

// ---------- save current ----------
$('save-btn').addEventListener('click', () => $('save-row').classList.toggle('hidden'));
$('save-cancel').addEventListener('click', () => $('save-row').classList.add('hidden'));
$('save-confirm').addEventListener('click', async () => {
  const label = $('save-label').value.trim();
  if (!label) { toast('请填写账号标签'); return; }
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    const tab = tabs[0];
    const url = new URL(tab.url);
    const r = await chrome.runtime.sendMessage({
      type: 'SAVE',
      payload: { label, note: $('save-note').value.trim(), domain: url.hostname, sourceUrl: url.href },
    });
    if (!r.ok) throw new Error(r.error);
    toast(`已保存「${label}」（${r.snapshot.cookie_count} 个 Cookie）`);
    $('save-label').value = ''; $('save-note').value = '';
    $('save-row').classList.add('hidden');
    await loadGroups();
    loadSnapshots(true);
  } catch (e) { toast('保存失败：' + e.message); }
});

// ---------- clear current site cookies ----------
// Direct fallback: delete browser cookies from this page itself, without
// going through the background worker (used if the background call fails).
function parentDomains(hostname) {
  const parts = String(hostname || '').replace(/^\./, '').split('.').filter(Boolean);
  const out = [];
  while (parts.length >= 2) { out.push(parts.join('.')); parts.shift(); }
  return out;
}
async function getAllCookiesForDomainDirect(domain, tabUrl) {
  const byKey = new Map();
  const add = (list) => { for (const c of list || []) { const k = [c.name, c.domain, c.path, c.storeId || ''].join('\x00'); if (!byKey.has(k)) byKey.set(k, c); } };
  for (const d of parentDomains(domain)) { try { add(await chrome.cookies.getAll({ domain: d })); } catch { /* ignore */ } }
  if (tabUrl) { try { add(await chrome.cookies.getAll({ url: tabUrl })); } catch { /* ignore */ } }
  return [...byKey.values()];
}
async function removeCookieDirect(c) {
  const host = String(c.domain || '').replace(/^\./, '');
  const schemes = [c.secure ? 'https' : 'http', c.secure ? 'http' : 'https'];
  for (const scheme of schemes) {
    for (const path of [c.path || '/', '/']) {
      for (const details of [
        { url: `${scheme}://${host}${path}`, name: c.name, storeId: c.storeId },
        { url: `${scheme}://${host}${path}`, name: c.name },
      ]) {
        try { const r = await chrome.cookies.remove(details); if (r) return true; } catch { /* next */ }
      }
    }
  }
  return false;
}
async function clearCookiesDirect(domain, tabUrl) {
  const existing = await getAllCookiesForDomainDirect(domain, tabUrl);
  let removed = 0; const failures = [];
  for (const c of existing) { if (await removeCookieDirect(c)) removed++; else failures.push(c.name); }
  const remaining = await getAllCookiesForDomainDirect(domain, tabUrl);
  return { domain, removed, failed: failures.length, failures, total: existing.length, remaining: remaining.length };
}

// Always clears the cookies of the page in the currently active browser
// tab (resolved fresh in the background at click time), never the
// configured server address or a snapshot's domain.
async function clearSiteCookies() {
  let domain = '';
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs[0]?.url) domain = new URL(tabs[0].url).hostname;
  } catch { /* background will resolve it as a fallback */ }
  showModal({
    title: '清空当前站点 Cookie', danger: true, okText: '清空',
    body: `确定清空当前标签页所在站点${domain ? `（${domain}）` : ''}的全部 Cookie？\n清空后刷新页面即可测试未登录状态，之后再点「恢复」恢复某个快照的登录态。`,
    onOk: async () => {
      let res = null; let bgError = '';
      try {
        const r = await chrome.runtime.sendMessage({ type: 'CLEAR', domain: domain || undefined });
        if (r && r.ok) res = r.result; else bgError = (r && r.error) || 'background 无响应';
      } catch (e) { bgError = e.message || String(e); }
      let prefix = '';
      if (!res) {
        // Fallback: delete directly from this page.
        try {
          const tabs2 = await chrome.tabs.query({ active: true, currentWindow: true });
          const t = tabs2[0];
          const d = domain || (t?.url ? new URL(t.url).hostname : '');
          if (!d) throw new Error('无法识别当前标签页域名');
          res = await clearCookiesDirect(d, t?.url || undefined);
          prefix = `（background 失败[${bgError}]，已改直连删除）`;
        } catch (e2) {
          toast(`清空失败：background 报错 ${bgError}；直连也失败 ${e2.message || e2}`);
          return;
        }
      }
      toast(`已清空 ${res.domain}：删除 ${res.removed}/${res.total} 个${res.failed ? `，失败 ${res.failed}（${res.failures.join(', ')}）` : ''}${res.remaining ? `，仍剩 ${res.remaining} 个（站点可能重新写入）` : ''}${prefix}，请刷新页面测试未登录状态`);
    },
  });
}
$('clear-current-btn').addEventListener('click', () => clearSiteCookies());
$('clear-btn').addEventListener('click', () => clearSiteCookies());

// ---------- modal ----------
function showModal({ title, body, okText = '确认', danger = false, onOk }) {
  $('modal-title').textContent = title;
  $('modal-body').textContent = body;
  const ok = $('modal-ok');
  ok.textContent = okText;
  ok.className = 'btn ' + (danger ? 'danger' : 'primary');
  $('modal').classList.remove('hidden');
  ok.onclick = () => { $('modal').classList.add('hidden'); onOk && onOk(); };
  $('modal-cancel').onclick = () => $('modal').classList.add('hidden');
}

async function renameSnapshot(snap) {
  const label = prompt('新标签', snap.label);
  if (!label || !label.trim()) return;
  try {
    await api(`/v1/snapshots/${snap.id}`, { method: 'PATCH', body: { label: label.trim(), note: snap.note } });
    state.itemsById.get(snap.id).label = label.trim();
    state.items = [...state.itemsById.values()];
    syncList();
  } catch (e) { toast('改名失败：' + e.message); }
}

function deleteSnapshot(snap) {
  showModal({
    title: '删除快照', danger: true, okText: '删除',
    body: `确定删除「${snap.label}」？\n将永久删除该快照（含 ${snap.cookie_count} 个 Cookie），此操作不可撤销。`,
    onOk: async () => {
      try {
        await api(`/v1/snapshots/${snap.id}`, { method: 'DELETE' });
        state.itemsById.delete(snap.id);
        state.items = [...state.itemsById.values()];
        state.total = Math.max(0, state.total - 1);
        syncList();
        toast('已删除');
      } catch (e) { toast('删除失败：' + e.message); }
    },
  });
}

// ---------- detail view ----------
const cookieList = new VirtualList({
  scrollEl: $('cookie-scroll'), spacerEl: $('cookie-spacer'), windowEl: $('cookie-window'),
  rowHeight: COOKIE_ROW_H,
  renderRow: (c) => {
    const q = $('cookie-search').value.trim();
    const row = document.createElement('div');
    row.className = 'cookie-row';
    const tags = [];
    if (c.httpOnly) tags.push('<span class="tag ho">HttpOnly</span>');
    if (c.secure) tags.push('<span class="tag">Secure</span>');
    if (c.sameSite) tags.push(`<span class="tag">${esc(c.sameSite)}</span>`);
    if (!c.expirationDate) tags.push('<span class="tag">Session</span>');
    const masked = c.value ? c.value.slice(0, 4) + '•••' : '';
    row.innerHTML = `<span class="name">${highlight(c.name, q)}</span>
      <span class="domain">${highlight(c.domain, q)}</span>
      <span class="muted">${esc(masked)}</span>
      <span class="tags">${tags.join('')}</span>`;
    return row;
  },
});

function renderCookies() {
  const q = $('cookie-search').value.trim().toLowerCase();
  const filtered = q
    ? state.cookies.filter((c) => c.name.toLowerCase().includes(q) || c.domain.toLowerCase().includes(q))
    : state.cookies;
  cookieList.setItems(filtered);
  $('cookie-status').textContent = q ? `匹配 ${filtered.length} / 共 ${state.cookies.length} 个` : `共 ${state.cookies.length} 个 Cookie`;
}

async function openDetail(snap, autoRestore) {
  state.detailSnapshot = snap;
  $('view-list').classList.add('hidden');
  $('view-detail').classList.remove('hidden');
  $('detail-label').textContent = snap.label;
  $('detail-meta').textContent = `${snap.domain} · ${snap.cookie_count} 个 Cookie · ${timeAgo(snap.created_at)}`;
  $('cookie-search').value = '';
  $('cookie-status').textContent = '加载中…';
  try {
    const data = await api(`/v1/snapshots/${snap.id}/cookies`);
    state.cookies = data.items || [];
    renderCookies();
    if (autoRestore) doRestore();
  } catch (e) { $('cookie-status').textContent = '加载失败：' + e.message; }
}

$('back-btn').addEventListener('click', () => {
  $('view-detail').classList.add('hidden');
  $('view-list').classList.remove('hidden');
  // Refresh last_used_at ordering after a restore.
  loadSnapshots(true);
});

let cookieSearchTimer = 0;
$('cookie-search').addEventListener('input', () => {
  clearTimeout(cookieSearchTimer);
  cookieSearchTimer = setTimeout(renderCookies, 150);
});

async function doRestore() {
  const snap = state.detailSnapshot;
  if (!snap) return;
  // Main-domain check: only restore when the snapshot's main domain
  // matches the main domain of the page in the active tab.
  const tabDomain = await refreshCurrentDomain();
  if (tabDomain) {
    const snapMain = mainDomain(snap.domain);
    const tabMain = mainDomain(tabDomain);
    if (snapMain !== tabMain) {
      toast(`主域名不匹配：当前站点 ${tabDomain}（主域名 ${tabMain}），该快照属于 ${snapMain}，不能恢复。请先打开该站点的页面再恢复。`);
      return;
    }
  }
  const backup = $('restore-backup').checked;
  showModal({
    title: '恢复快照',
    body: `恢复「${snap.label}」（${snap.domain}）？\n恢复时会自动先清空当前站点 Cookie，再写入快照 Cookie。\n${backup ? '恢复前会自动备份当前 Cookie。' : ''}`,
    okText: '开始恢复',
    onOk: async () => {
      try {
        toast('恢复中…');
        const r = await chrome.runtime.sendMessage({ type: 'RESTORE', id: snap.id, options: { backupBefore: backup } });
        if (!r.ok) throw new Error(r.error);
        const { restored, failed, removed, backupId } = r.result;
        toast(`恢复完成：成功 ${restored}，失败 ${failed}${removed ? `，清除旧 ${removed}` : ''}${backupId ? '，已自动备份' : ''}`);
        loadSnapshots(true);
      } catch (e) { toast('恢复失败：' + e.message); }
    },
  });
}
$('restore-btn').addEventListener('click', doRestore);

// ---------- compare & single export ----------
function openCompare(snap) {
  state.detailSnapshot = snap;
  $('compare-base').textContent = `基准：${snap.label}（${snap.domain}）`;
  const others = state.items.filter((i) => i.id !== snap.id);
  $('compare-target').innerHTML = others.length
    ? others.map((i) => `<option value="${esc(i.id)}">${esc(i.label)} (${esc(i.domain)})</option>`).join('')
    : '<option value="">（当前列表没有其他快照）</option>';
  $('compare-result').innerHTML = '';
  $('compare-modal').classList.remove('hidden');
}
$('compare-btn').addEventListener('click', () => state.detailSnapshot && openCompare(state.detailSnapshot));
$('compare-close').addEventListener('click', () => $('compare-modal').classList.add('hidden'));
$('compare-run').addEventListener('click', async () => {
  const base = state.detailSnapshot;
  const targetId = $('compare-target').value;
  if (!base || !targetId) return;
  $('compare-result').textContent = '对比中…';
  try {
    const d = await api(`/v1/snapshots/compare?a=${encodeURIComponent(base.id)}&b=${encodeURIComponent(targetId)}`);
    const fmt = (arr, fields) => arr.length
      ? '<div>' + arr.map((c) => `<div>· ${esc(c.name)} <span class="muted">${esc(c.domain)}${c.path}</span>${fields && c.fields ? ` <span class="muted">（变化：${esc(c.fields.join(', '))}）</span>` : ''}</div>`).join('') + '</div>'
      : '<div class="muted">无</div>';
    $('compare-result').innerHTML = `
      <div class="muted">${esc(d.a.label)} → ${esc(d.b.label)}：新增 ${d.added.length}，删除 ${d.removed.length}，变化 ${d.changed.length}，未变 ${d.unchanged}</div>
      <div style="margin-top:8px"><strong>新增（仅对比方有）</strong>${fmt(d.added)}</div>
      <div style="margin-top:8px"><strong>删除（仅基准有）</strong>${fmt(d.removed)}</div>
      <div style="margin-top:8px"><strong>变化（值/属性）</strong>${fmt(d.changed, true)}</div>`;
  } catch (e) { $('compare-result').textContent = '对比失败：' + e.message; }
});

async function exportSnapshot(snap) {
  const pw = prompt(`导出「${snap.label}」\n请输入备份密码（至少 8 位）`);
  if (!pw) return;
  if (pw.length < 8) { toast('备份密码至少 8 位'); return; }
  try {
    const data = await api(`/v1/snapshots/${snap.id}/export`, { method: 'POST', body: { backup_password: pw } });
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `cookievault-${snap.label}-${new Date().toISOString().slice(0, 10)}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
    toast('已导出加密快照包');
  } catch (e) { toast('导出失败：' + e.message); }
}
$('export-btn').addEventListener('click', () => state.detailSnapshot && exportSnapshot(state.detailSnapshot));

// ---------- tabs (数据 / 邮箱生成 / 设置) ----------
document.querySelectorAll('.tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach((b) => b.classList.toggle('active', b === btn));
    const tab = btn.dataset.tab;
    $('tab-data').classList.toggle('hidden', tab !== 'data');
    $('tab-email').classList.toggle('hidden', tab !== 'email');
    $('tab-settings').classList.toggle('hidden', tab !== 'settings');
    if (tab === 'settings') refreshSettingsTab();
    if (tab === 'data') snapList.render();
  });
});

// ---------- settings tab ----------
function setMsg(t) { $('set-msg').textContent = t; }

async function refreshSettingsTab() {
  try {
    const s = await chrome.storage.local.get(['serverUrl']);
    $('set-serverUrl').value = s.serverUrl || 'http://127.0.0.1:18765';
  } catch { /* ignore */ }
  try {
    const st = await api('/v1/auth/status');
    $('set-status').textContent = st.initialized
      ? (st.unlocked ? '已初始化 · 已解锁' : '已初始化 · 已锁定')
      : '未初始化：请设置主密码';
    $('set-setup-box').style.display = st.initialized ? 'none' : 'block';
    $('set-unlock-box').style.display = st.initialized ? 'block' : 'none';
    $('set-change-box').style.display = st.initialized ? 'block' : 'none';
    $('set-conn-status').textContent = '已连接';
  } catch (e) {
    $('set-status').textContent = '无法连接服务端：' + e.message;
    $('set-conn-status').textContent = '';
    return;
  }
  try {
    const cfg = await api('/v1/settings');
    $('set-idle-minutes').value = cfg.idle_lock_minutes;
  } catch { /* ignore */ }
  try {
    const local = await chrome.storage.local.get(['autoBackupBeforeLock']);
    $('set-auto-backup-before-lock').checked = local.autoBackupBeforeLock !== false;
  } catch { /* ignore */ }
}

async function saveServerUrl() {
  const url = $('set-serverUrl').value.trim().replace(/\/$/, '');
  if (url) await chrome.storage.local.set({ serverUrl: url });
  return url;
}

$('set-pair').addEventListener('click', async () => {
  await saveServerUrl();
  const code = $('set-pairCode').value.trim();
  if (!code) { setMsg('请输入配对码（服务端日志里有）'); return; }
  try {
    const data = await api('/v1/pair/verify', { method: 'POST', body: { code } });
    await chrome.storage.local.set({ token: data.token });
    setMsg('连接成功，Token 已保存');
    refreshSettingsTab();
    loadSnapshots(true);
  } catch (e) { setMsg('配对失败：' + e.message); }
});

$('set-setup').addEventListener('click', async () => {
  await saveServerUrl();
  try {
    const data = await api('/v1/auth/setup', { method: 'POST', body: { password: $('set-setup-pw').value } });
    if (data.token) await chrome.storage.local.set({ token: data.token });
    setMsg('主密码已设置并解锁；如未配对，请用服务端日志的配对码连接');
    refreshSettingsTab();
  } catch (e) { setMsg('设置失败：' + e.message); }
});

$('set-unlock').addEventListener('click', async () => {
  await saveServerUrl();
  try { await api('/v1/auth/unlock', { method: 'POST', body: { password: $('set-unlock-pw').value } }); setMsg('已解锁'); refreshSettingsTab(); loadSnapshots(true); }
  catch (e) { setMsg('解锁失败：' + e.message); }
});

$('set-lock').addEventListener('click', async () => {
  // Pre-lock auto-backup (same behaviour as the options page).
  const s = await chrome.storage.local.get(['autoBackupBeforeLock']);
  if (s.autoBackupBeforeLock !== false) {
    try {
      const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
      const tab = tabs[0];
      if (tab?.url) {
        const domain = new URL(tab.url).hostname;
        if (domain) {
          const r = await chrome.runtime.sendMessage({ type: 'SAVE', payload: { label: `锁前备份·${(()=>{const d=new Date(),p=n=>String(n).padStart(2,'0');return `${p(d.getMonth()+1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`})()}`, note: 'auto-backup before manual lock', domain, sourceUrl: tab.url } });
          if (r.ok) setMsg(`锁定前已自动备份当前域名（${r.snapshot.cookie_count} 个 Cookie）`);
        }
      }
    } catch { /* best effort */ }
  }
  try { await api('/v1/auth/lock', { method: 'POST' }); setMsg(($('set-msg').textContent || '') + ' · 已锁定'); refreshSettingsTab(); loadSnapshots(true); }
  catch (e) { setMsg('锁定失败：' + e.message); }
});

$('set-change').addEventListener('click', async () => {
  try { await api('/v1/auth/change-password', { method: 'POST', body: { old_password: $('set-old-pw').value, new_password: $('set-new-pw').value } }); setMsg('主密码已修改'); }
  catch (e) { setMsg('修改失败：' + e.message); }
});

$('set-save-settings').addEventListener('click', async () => {
  try {
    const data = await api('/v1/settings', { method: 'POST', body: { idle_lock_minutes: Number($('set-idle-minutes').value || 0) } });
    await chrome.storage.local.set({ autoBackupBeforeLock: $('set-auto-backup-before-lock').checked });
    setMsg(`已保存：闲置 ${data.idle_lock_minutes} 分钟自动锁定`);
  } catch (e) { setMsg('保存设置失败：' + e.message); }
});

$('set-export').addEventListener('click', async () => {
  const pw = $('set-backup-pw').value;
  if (!pw || pw.length < 8) { $('set-backup-status').textContent = '备份密码至少 8 位'; return; }
  try {
    $('set-backup-status').textContent = '导出中…';
    const data = await api('/v1/backup/export', { method: 'POST', body: { backup_password: pw } });
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `cookievault-backup-${new Date().toISOString().slice(0, 10)}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
    $('set-backup-status').textContent = '已导出（文件已加密）';
  } catch (e) { $('set-backup-status').textContent = '导出失败：' + e.message; }
});

$('set-import').addEventListener('click', async () => {
  const file = $('set-import-file').files[0];
  const pw = $('set-backup-pw').value;
  if (!file) { $('set-backup-status').textContent = '先选择备份文件'; return; }
  if (!pw || pw.length < 8) { $('set-backup-status').textContent = '备份密码至少 8 位'; return; }
  try {
    $('set-backup-status').textContent = '导入中…';
    const data = JSON.parse(await file.text());
    const res = await api('/v1/backup/import', { method: 'POST', body: { backup_password: pw, data } });
    $('set-backup-status').textContent = `导入完成：快照 ${res.imported_snapshots}，Cookie ${res.imported_cookies}，跳过重复 ${res.skipped}`;
    loadSnapshots(true);
  } catch (e) { $('set-backup-status').textContent = '导入失败：' + e.message; }
});

// ---------- 邮箱生成 (Japanese name + random email) ----------
const JP_STORAGE_KEY = 'jp-name-email-generator.v2';
const jpParse = (text) => text.trim().split('\n').map((line) => line.trim().split('|'));
const jpSurnames = jpParse(`
佐藤|sato
鈴木|suzuki
高橋|takahashi
田中|tanaka
伊藤|ito
渡辺|watanabe
山本|yamamoto
中村|nakamura
小林|kobayashi
加藤|kato
吉田|yoshida
山田|yamada
佐々木|sasaki
山口|yamaguchi
松本|matsumoto
井上|inoue
木村|kimura
林|hayashi
斎藤|saito
清水|shimizu
山崎|yamazaki
森|mori
池田|ikeda
橋本|hashimoto
阿部|abe
石川|ishikawa
山下|yamashita
中島|nakajima
石井|ishii
小川|ogawa
前田|maeda
岡田|okada
長谷川|hasegawa
藤田|fujita
後藤|goto
近藤|kondo
村上|murakami
遠藤|endo
青木|aoki
坂本|sakamoto
`);
const jpMaleNames = jpParse(`
翔太|shota
大輔|daisuke
健太|kenta
拓也|takuya
直樹|naoki
和也|kazuya
達也|tatsuya
悠太|yuta
翔|sho
蓮|ren
陸|riku
大和|yamato
悠真|yuma
湊|minato
樹|itsuki
颯太|sota
陽太|yota
健一|kenichi
誠|makoto
智也|tomoya
亮介|ryosuke
俊介|shunsuke
直人|naoto
雄大|yudai
海斗|kaito
`);
const jpFemaleNames = jpParse(`
美咲|misaki
由美|yumi
優子|yuko
陽子|yoko
愛|ai
愛美|manami
彩|aya
彩香|ayaka
香織|kaori
真由美|mayumi
恵子|keiko
直美|naomi
美穂|miho
七海|nanami
葵|aoi
凛|rin
結衣|yui
結菜|yuina
陽菜|hina
美月|mizuki
さくら|sakura
ひなた|hinata
琴音|kotone
花音|kanon
莉子|riko
`);
const JP_DEFAULTS = { gender: 'mixed', domain: 'gmail.com', count: 10, pattern: 'given.surname', randomLength: 6, output: 'both' };
let jpSettings = { ...JP_DEFAULTS };
let jpRecords = [];

function jpSetStatus(message, isError = false) {
  const el = $('email-status');
  el.textContent = message;
  el.classList.toggle('error', !!isError);
}
function jpSetSelect(id, value, fallback) {
  const el = $(id);
  const valid = [...el.options].some((o) => o.value === String(value));
  el.value = valid ? String(value) : String(fallback);
}
async function jpLoadSettings() {
  try {
    const saved = await chrome.storage.local.get([JP_STORAGE_KEY]);
    const s = saved[JP_STORAGE_KEY];
    if (s && typeof s === 'object') jpSettings = { ...JP_DEFAULTS, ...s };
  } catch { /* ignore */ }
  jpSetSelect('email-gender', jpSettings.gender, JP_DEFAULTS.gender);
  jpSetSelect('email-pattern', jpSettings.pattern, JP_DEFAULTS.pattern);
  jpSetSelect('email-randomLength', jpSettings.randomLength, JP_DEFAULTS.randomLength);
  jpSetSelect('email-outputMode', jpSettings.output, JP_DEFAULTS.output);
  $('email-domain').value = typeof jpSettings.domain === 'string' ? jpSettings.domain : JP_DEFAULTS.domain;
  $('email-count').value = String(jpSettings.count ?? JP_DEFAULTS.count);
}
function jpReadSettings() {
  const raw = Number($('email-count').value);
  const count = Number.isFinite(raw) ? Math.max(1, Math.min(500, Math.trunc(raw))) : JP_DEFAULTS.count;
  $('email-count').value = String(count);
  return {
    gender: $('email-gender').value,
    domain: $('email-domain').value.trim(),
    count,
    pattern: $('email-pattern').value,
    randomLength: Number($('email-randomLength').value),
    output: $('email-outputMode').value,
  };
}
async function jpSaveSettings() {
  jpSettings = jpReadSettings();
  try { await chrome.storage.local.set({ [JP_STORAGE_KEY]: jpSettings }); } catch { /* ignore */ }
  return jpSettings;
}
function jpNormalizeDomain(value) {
  const domain = String(value).trim().replace(/^@/, '').toLowerCase();
  if (!domain) throw new Error('请填写邮箱后缀，例如 gmail.com。');
  const labels = domain.split('.');
  const valid = domain.length <= 253 && labels.length >= 2 &&
    labels.every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)) &&
    !/^\d+$/.test(labels[labels.length - 1]);
  if (!valid) throw new Error('邮箱后缀格式不正确。请填写类似 example.com 的域名，不要包含 https://、路径、空格或完整邮箱。');
  return domain;
}
function jpRandomInt(max) {
  const range = 0x100000000;
  const limit = range - (range % max);
  const buffer = new Uint32Array(1);
  do { crypto.getRandomValues(buffer); } while (buffer[0] >= limit);
  return buffer[0] % max;
}
function jpRandomString(length) {
  const alphabet = 'abcdefghijklmnopqrstuvwxyz0123456789';
  return Array.from({ length }, () => alphabet[jpRandomInt(alphabet.length)]).join('');
}
function jpMakeAccount(surname, given, options) {
  const suffix = jpRandomString(options.randomLength);
  switch (options.pattern) {
    case 'surname.given': return `${surname}.${given}.${suffix}`;
    case 'given_surname': return `${given}_${surname}_${suffix}`;
    case 'surnamegiven': return `${surname}${given}${suffix}`;
    case 'random': return suffix;
    default: return `${given}.${surname}.${suffix}`;
  }
}
function jpRenderOutput() {
  const mode = $('email-outputMode').value;
  $('email-output').value = jpRecords.map((r) => {
    if (mode === 'email') return r.email;
    if (mode === 'detail') return `${r.name}\t${r.romaji}\t${r.email}`;
    return `${r.name}\t${r.email}`;
  }).join('\n');
}
function jpGenerate() {
  try {
    const options = jpReadSettings();
    const domain = jpNormalizeDomain(options.domain);
    $('email-domain').value = domain;
    jpSaveSettings();
    const names = options.gender === 'male' ? jpMaleNames : options.gender === 'female' ? jpFemaleNames : [...jpMaleNames, ...jpFemaleNames];
    const pool = jpSurnames.flatMap((surname) => names.map((given) => ({ surname, given })));
    const count = Math.min(options.count, pool.length);
    const nextRecords = [];
    const usedEmails = new Set();
    for (let i = 0; i < count; i++) {
      const j = i + jpRandomInt(pool.length - i);
      [pool[i], pool[j]] = [pool[j], pool[i]];
      const { surname, given } = pool[i];
      let email = '';
      for (let attempt = 0; attempt < 100; attempt++) {
        const candidate = `${jpMakeAccount(surname[1], given[1], options)}@${domain}`;
        if (!usedEmails.has(candidate)) { email = candidate; break; }
      }
      if (!email) throw new Error('随机邮箱去重失败，请增加随机字符长度后重试。');
      usedEmails.add(email);
      nextRecords.push({ name: `${surname[0]} ${given[0]}`, romaji: `${surname[1]} ${given[1]}`, email });
    }
    jpRecords = nextRecords;
    jpRenderOutput();
    jpSetStatus(`已生成 ${jpRecords.length} 条；本批姓名和邮箱均不重复；后缀：@${domain}。`);
  } catch (error) {
    jpSetStatus(error.message || '生成失败，请检查设置。', true);
  }
}
async function jpCopyText(text) {
  if (!text) { jpSetStatus('请先生成结果。', true); return; }
  try {
    await navigator.clipboard.writeText(text);
    jpSetStatus(`已复制 ${text.split('\n').length} 条记录。`);
  } catch {
    // Fallback for clipboard permission issues.
    try {
      const ta = document.createElement('textarea');
      ta.value = text; document.body.appendChild(ta); ta.select();
      const ok = document.execCommand('copy'); ta.remove();
      jpSetStatus(ok ? `已复制 ${text.split('\n').length} 条记录。` : '复制失败，请在结果框中手动选择并复制。', !ok);
    } catch { jpSetStatus('复制失败，请在结果框中手动选择并复制。', true); }
  }
}
$('email-generate').addEventListener('click', jpGenerate);
$('email-copyAll').addEventListener('click', () => jpCopyText($('email-output').value));
$('email-copyEmails').addEventListener('click', () => jpCopyText(jpRecords.map((r) => r.email).join('\n')));
for (const id of ['email-domain', 'email-gender', 'email-count', 'email-pattern', 'email-randomLength', 'email-outputMode']) {
  $(id).addEventListener('change', () => { jpSaveSettings(); if (id === 'email-outputMode') jpRenderOutput(); });
}
$('email-domain').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); jpGenerate(); } });

// ---------- init ----------
(async function init() {
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs[0]?.url) {
      try { state.currentDomain = new URL(tabs[0].url).hostname; } catch { /* ignore */ }
    }
  } catch { /* tabs permission may be absent */ }
  await jpLoadSettings();
  await loadGroups();
  loadSnapshots(true);
})();
