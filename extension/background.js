// CookieVault background Service Worker (MV3).
// Responsibilities: open side panel, talk to the local Go server,
// collect cookies via chrome.cookies, restore snapshots.

const DEFAULT_SERVER = 'http://127.0.0.1:18765';

async function getSettings() {
  const s = await chrome.storage.local.get(['serverUrl', 'token']);
  return { serverUrl: (s.serverUrl || DEFAULT_SERVER).replace(/\/$/, ''), token: s.token || '' };
}

async function api(path, options = {}) {
  const { serverUrl, token } = await getSettings();
  const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
  if (token) headers.Authorization = `Bearer ${token}`;
  const res = await fetch(serverUrl + path, { ...options, headers });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* non-json */ }
  if (!res.ok) {
    const err = new Error((data && data.error) || `HTTP ${res.status}`);
    err.status = res.status;
    err.code = data && data.error;
    throw err;
  }
  return data;
}

// chrome.cookies returns partitionKey as an object; store it as JSON string.
function partitionKeyToString(pk) {
  if (!pk) return '';
  if (typeof pk === 'string') return pk;
  try { return JSON.stringify(pk); } catch { return ''; }
}

function partitionKeyFromString(s) {
  if (!s) return undefined;
  try { return JSON.parse(s); } catch { return undefined; }
}

async function collectCookies(domain, storeId) {
  const opts = { domain };
  if (storeId) opts.storeId = storeId;
  const cookies = await chrome.cookies.getAll(opts);
  return cookies.map((c) => ({
    name: c.name,
    value: c.value,
    domain: c.domain,
    hostOnly: c.hostOnly,
    path: c.path,
    secure: c.secure,
    httpOnly: c.httpOnly,
    sameSite: c.sameSite,
    expirationDate: c.expirationDate,
    partitionKey: partitionKeyToString(c.partitionKey),
    partitioned: !!c.partitionKey,
  }));
}

async function saveSnapshot({ label, note = '', domain, sourceUrl = '', isReplace = true, profileId = '' }) {
  const cookies = await collectCookies(domain);
  if (!cookies.length) throw new Error('no cookies found for ' + domain);
  const snap = await api('/v1/snapshots', {
    method: 'POST',
    body: JSON.stringify({
      profile_id: profileId || undefined,
      domain, label, note, source_url: sourceUrl,
      is_replace: isReplace, cookies,
    }),
  });
  await rememberFingerprint(domain, cookies);
  return snap;
}

// Local fingerprint of a cookie set, used to detect "dirty" domains for
// pre-lock auto-backup without querying the server (which would reset
// its idle timer).
function fingerprintCookies(cookies) {
  let h = 5381;
  const s = cookies.map((c) => `${c.name}\x00${c.domain}\x00${c.path}\x00${c.value}`).sort().join('\x01');
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) >>> 0;
  return h.toString(16);
}

async function rememberFingerprint(domain, cookies) {
  const s = await chrome.storage.local.get(['lastFingerprints']);
  const map = s.lastFingerprints || {};
  map[domain] = fingerprintCookies(cookies);
  await chrome.storage.local.set({ lastFingerprints: map });
}

async function isDirty(domain) {
  const cookies = await collectCookies(domain);
  if (!cookies.length) return false;
  const s = await chrome.storage.local.get(['lastFingerprints']);
  return (s.lastFingerprints || {})[domain] !== fingerprintCookies(cookies);
}

function cookieUrl(c) {
  const host = c.domain.replace(/^\./, '');
  const scheme = c.secure ? 'https' : 'http';
  return `${scheme}://${host}${c.path || '/'}`;
}

async function restoreSnapshot(id, { replace = true, backupBefore = true } = {}) {
  const { items } = await api(`/v1/snapshots/${id}/cookies`);
  const snap = await api(`/v1/snapshots/${id}`);

  let backupId = null;
  if (backupBefore) {
    const current = await collectCookies(snap.domain);
    if (current.length) {
      const b = await api('/v1/snapshots', {
        method: 'POST',
        body: JSON.stringify({
          domain: snap.domain,
          label: `自动备份 ${new Date().toLocaleString()}（恢复「${snap.label}」前）`,
          note: 'auto-backup before restore',
          is_replace: true,
          cookies: current,
        }),
      });
      backupId = b.id;
    }
  }

  let removed = 0;
  if (replace) {
    const existing = await chrome.cookies.getAll({ domain: snap.domain });
    for (const c of existing) {
      try {
        await chrome.cookies.remove({ url: cookieUrl(c), name: c.name, storeId: c.storeId });
        removed++;
      } catch { /* best effort */ }
    }
  }

  let ok = 0, failed = 0;
  const failures = [];
  for (const c of items) {
    const details = {
      url: cookieUrl(c),
      name: c.name,
      value: c.value,
      path: c.path || '/',
      secure: !!c.secure,
      httpOnly: !!c.httpOnly,
      sameSite: c.sameSite || 'lax',
    };
    if (c.expirationDate) details.expirationDate = c.expirationDate;
    const pk = partitionKeyFromString(c.partitionKey);
    if (pk) details.partitionKey = pk;
    try {
      const set = await chrome.cookies.set(details);
      if (set) ok++; else { failed++; failures.push(c.name); }
    } catch {
      // Retry without partitionKey (some Chromium versions disagree on shape).
      try {
        delete details.partitionKey;
        const set = await chrome.cookies.set(details);
        if (set) ok++; else { failed++; failures.push(c.name); }
      } catch { failed++; failures.push(c.name); }
    }
  }

  await api(`/v1/snapshots/${id}/touch`, { method: 'POST' }).catch(() => {});
  await rememberFingerprint(snap.domain, items).catch(() => {});
  return { restored: ok, failed, removed, backupId, failures };
}

chrome.runtime.onInstalled.addListener(() => {
  chrome.runtime.setUninstallURL('https://example.invalid/cookievault-uninstall');
  chrome.alarms.create('idleWatch', { periodInMinutes: 1 });
});
chrome.runtime.onStartup.addListener(() => chrome.alarms.create('idleWatch', { periodInMinutes: 1 }));

// Pre-lock auto-backup watcher.
// Every minute, check (via /auth/status, which does NOT reset the server's
// idle timer) whether the vault has been unlocked for close to the idle
// limit. If so, back up the active tab's domain when its cookies changed
// since the last save/restore, before the server locks.
chrome.alarms.onAlarm.addListener(async (alarm) => {
  if (alarm.name !== 'idleWatch') return;
  try {
    const s = await chrome.storage.local.get(['autoBackupBeforeLock', 'firstUnlockedAt']);
    if (s.autoBackupBeforeLock === false) return; // opt-out
    const { serverUrl, token } = await getSettings();
    const status = await (await fetch(serverUrl + '/v1/auth/status')).json();
    if (!status.unlocked) {
      await chrome.storage.local.set({ firstUnlockedAt: 0 });
      return;
    }
    const now = Date.now();
    let first = s.firstUnlockedAt || 0;
    if (!first) { first = now; await chrome.storage.local.set({ firstUnlockedAt: first }); return; }
    let idleMinutes = 15;
    if (token) {
      try {
        const r = await fetch(serverUrl + '/v1/settings', { headers: { Authorization: `Bearer ${token}` } });
        if (r.ok) idleMinutes = (await r.json()).idle_lock_minutes ?? 15;
      } catch { /* keep default */ }
    }
    if (!idleMinutes || idleMinutes <= 0) return;
    // Back up 60s before the server would auto-lock.
    if (now - first < idleMinutes * 60 * 1000 - 60 * 1000) return;
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    const tab = tabs[0];
    if (!tab?.url) return;
    let domain;
    try { domain = new URL(tab.url).hostname; } catch { return; }
    if (!domain || !(await isDirty(domain))) return;
    await saveSnapshot({
      label: `闲置锁定前自动备份 ${new Date().toLocaleString()}`,
      note: 'auto-backup before idle lock',
      domain, sourceUrl: tab.url,
    });
    await chrome.storage.local.set({ firstUnlockedAt: 0 });
  } catch { /* best effort; never crash the worker */ }
});

// Clicking the toolbar icon opens the side panel.
chrome.action.onClicked.addListener(async (tab) => {
  try {
    await chrome.sidePanel.open({ windowId: tab.windowId });
  } catch {
    await chrome.tabs.create({ url: chrome.runtime.getURL('sidepanel.html') });
  }
});

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      switch (msg.type) {
        case 'GET_SETTINGS': return sendResponse({ ok: true, settings: await getSettings() });
        case 'SAVE': return sendResponse({ ok: true, snapshot: await saveSnapshot(msg.payload) });
        case 'RESTORE': return sendResponse({ ok: true, result: await restoreSnapshot(msg.id, msg.options) });
        case 'API': return sendResponse({ ok: true, data: await api(msg.path, { method: msg.method || 'GET', body: msg.body ? JSON.stringify(msg.body) : undefined }) });
        default: return sendResponse({ ok: false, error: 'unknown message' });
      }
    } catch (e) {
      return sendResponse({ ok: false, error: e.message, code: e.code, status: e.status });
    }
  })();
  return true; // async response
});
