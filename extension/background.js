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

async function saveSnapshot({ label, note = '', domain, sourceUrl = '', isReplace = true }) {
  const cookies = await collectCookies(domain);
  if (!cookies.length) throw new Error('no cookies found for ' + domain);
  return api('/v1/snapshots', {
    method: 'POST',
    body: JSON.stringify({
      domain, label, note, source_url: sourceUrl,
      is_replace: isReplace, cookies,
    }),
  });
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
  return { restored: ok, failed, removed, backupId, failures };
}

chrome.runtime.onInstalled.addListener(() => {
  chrome.runtime.setUninstallURL('https://example.invalid/cookievault-uninstall');
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
