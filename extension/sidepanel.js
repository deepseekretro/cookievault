// CookieVault side panel: virtual list + infinite scroll + search.

const $ = (id) => document.getElementById(id);
const ROW_H = 76;          // snapshot card height (70 + 6 gap)
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
};

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
      <div class="row2"><span>${highlight(snap.domain, q)} · ${timeAgo(snap.last_used_at)}</span>
        <span class="actions">
          <button class="btn small primary" data-act="restore">恢复</button>
          <button class="btn small" data-act="rename">改名</button>
          <button class="btn small danger" data-act="delete">删除</button>
        </span></div>`;
    card.addEventListener('click', (e) => {
      const act = e.target.closest('[data-act]')?.dataset.act;
      if (act === 'restore') { e.stopPropagation(); openDetail(snap, true); return; }
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
    if ($('filter-current-domain').checked && state.currentDomain) params.set('domain', state.currentDomain);
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
$('filter-current-domain').addEventListener('change', () => loadSnapshots(true));
$('error-retry').addEventListener('click', () => loadSnapshots(true));
$('to-top').addEventListener('click', () => $('scroll').scrollTo({ top: 0, behavior: 'smooth' }));
$('goto-unlock').addEventListener('click', () => chrome.runtime.openOptionsPage());

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
    loadSnapshots(true);
  } catch (e) { toast('保存失败：' + e.message); }
});

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
  $('restore-replace').checked = snap.is_replace !== false;
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
  const replace = $('restore-replace').checked;
  const backup = $('restore-backup').checked;
  showModal({
    title: '恢复快照',
    body: `恢复「${snap.label}」（${snap.domain}）？\n${replace ? '将先清除该域名下现有 Cookie，再写入快照 Cookie。' : '将合并写入，不清除现有 Cookie。'}\n${backup ? '恢复前会自动备份当前 Cookie。' : ''}`,
    okText: '开始恢复',
    onOk: async () => {
      try {
        toast('恢复中…');
        const r = await chrome.runtime.sendMessage({ type: 'RESTORE', id: snap.id, options: { replace, backupBefore: backup } });
        if (!r.ok) throw new Error(r.error);
        const { restored, failed, removed, backupId } = r.result;
        toast(`恢复完成：成功 ${restored}，失败 ${failed}${removed ? `，清除旧 ${removed}` : ''}${backupId ? '，已自动备份' : ''}`);
        loadSnapshots(true);
      } catch (e) { toast('恢复失败：' + e.message); }
    },
  });
}
$('restore-btn').addEventListener('click', doRestore);

// Persist the restore-mode preference per snapshot.
$('restore-replace').addEventListener('change', async () => {
  const snap = state.detailSnapshot;
  if (!snap) return;
  try {
    await api(`/v1/snapshots/${snap.id}`, { method: 'PATCH', body: { is_replace: $('restore-replace').checked } });
    snap.is_replace = $('restore-replace').checked;
    toast($('restore-replace').checked ? '已设为替换模式' : '已设为合并模式');
  } catch (e) { toast('保存模式偏好失败：' + e.message); }
});

// ---------- init ----------
(async function init() {
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs[0]?.url) {
      try { state.currentDomain = new URL(tabs[0].url).hostname; } catch { /* ignore */ }
    }
  } catch { /* tabs permission may be absent */ }
  loadSnapshots(true);
})();
