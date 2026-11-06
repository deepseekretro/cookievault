const $ = (id) => document.getElementById(id);
const DEFAULT_SERVER = 'http://127.0.0.1:18765';

async function settings() {
  const s = await chrome.storage.local.get(['serverUrl', 'token']);
  return { serverUrl: (s.serverUrl || DEFAULT_SERVER).replace(/\/$/, ''), token: s.token || '' };
}
async function saveSettings(patch) {
  await chrome.storage.local.set(patch);
}
async function rawApi(path, { method = 'GET', body, token } = {}) {
  const { serverUrl, token: saved } = await settings();
  const t = token !== undefined ? token : saved;
  const res = await fetch(serverUrl + path, {
    method,
    headers: { 'Content-Type': 'application/json', ...(t ? { Authorization: `Bearer ${t}` } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}
function msg(t) { $('msg').textContent = t; }

async function refresh() {
  const { serverUrl } = await settings();
  $('serverUrl').value = serverUrl;
  try {
    const st = await rawApi('/v1/auth/status');
    $('status').textContent = st.initialized
      ? (st.unlocked ? '已初始化 · 已解锁' : '已初始化 · 已锁定')
      : '未初始化：请设置主密码';
    $('setup-box').style.display = st.initialized ? 'none' : 'block';
    $('unlock-box').style.display = st.initialized ? 'block' : 'none';
    $('change-box').style.display = st.initialized ? 'block' : 'none';
  } catch (e) {
    $('status').textContent = '无法连接服务端：' + e.message;
  }
}

$('pair').onclick = async () => {
  const url = $('serverUrl').value.trim() || DEFAULT_SERVER;
  await saveSettings({ serverUrl: url.replace(/\/$/, '') });
  const code = $('pairCode').value.trim();
  if (!code) { msg('请输入配对码（服务端日志里有）'); return; }
  try {
    const data = await rawApi('/v1/pair/verify', { method: 'POST', body: { code }, token: '' });
    await saveSettings({ token: data.token });
    msg('连接成功，Token 已保存');
    refresh();
  } catch (e) { msg('配对失败：' + e.message); }
};

$('setup').onclick = async () => {
  try {
    const data = await rawApi('/v1/auth/setup', { method: 'POST', body: { password: $('setup-pw').value }, token: '' });
    if (data.token) await saveSettings({ token: data.token });
    msg('主密码已设置并解锁；如未配对，请用服务端日志的配对码连接');
    refresh();
  } catch (e) { msg('设置失败：' + e.message); }
};

$('unlock').onclick = async () => {
  try { await rawApi('/v1/auth/unlock', { method: 'POST', body: { password: $('unlock-pw').value }, token: '' }); msg('已解锁'); refresh(); }
  catch (e) { msg('解锁失败：' + e.message); }
};
$('lock').onclick = async () => {
  // Pre-lock auto-backup: save the active tab's cookies first (best effort).
  const s = await chrome.storage.local.get(['autoBackupBeforeLock']);
  if (s.autoBackupBeforeLock !== false) {
    try {
      const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
      const tab = tabs[0];
      if (tab?.url) {
        const domain = new URL(tab.url).hostname;
        if (domain) {
          const r = await chrome.runtime.sendMessage({
            type: 'SAVE',
            payload: { label: `手动锁定前自动备份 ${new Date().toLocaleString()}`, note: 'auto-backup before manual lock', domain, sourceUrl: tab.url },
          });
          if (r.ok) msg(`锁定前已自动备份当前域名（${r.snapshot.cookie_count} 个 Cookie）`);
        }
      }
    } catch { /* best effort */ }
  }
  try { await rawApi('/v1/auth/lock', { method: 'POST' }); msg($('msg').textContent + ' · 已锁定'); refresh(); }
  catch (e) { msg('锁定失败：' + e.message); }
};
$('change').onclick = async () => {
  try { await rawApi('/v1/auth/change-password', { method: 'POST', body: { old_password: $('old-pw').value, new_password: $('new-pw').value } }); msg('主密码已修改'); }
  catch (e) { msg('修改失败：' + e.message); }
};

$('save-settings').onclick = async () => {
  try {
    const data = await rawApi('/v1/settings', { method: 'POST', body: { idle_lock_minutes: Number($('idle-minutes').value || 0) } });
    await chrome.storage.local.set({ autoBackupBeforeLock: $('auto-backup-before-lock').checked });
    msg(`已保存：闲置 ${data.idle_lock_minutes} 分钟自动锁定`);
  } catch (e) { msg('保存设置失败：' + e.message); }
};

$('export').onclick = async () => {
  const pw = $('backup-pw').value;
  if (!pw || pw.length < 8) { $('backup-status').textContent = '备份密码至少 8 位'; return; }
  try {
    $('backup-status').textContent = '导出中…';
    const data = await rawApi('/v1/backup/export', { method: 'POST', body: { backup_password: pw } });
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `cookievault-backup-${new Date().toISOString().slice(0, 10)}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
    $('backup-status').textContent = '已导出（文件已加密）';
  } catch (e) { $('backup-status').textContent = '导出失败：' + e.message; }
};

$('import').onclick = async () => {
  const file = $('import-file').files[0];
  const pw = $('backup-pw').value;
  if (!file) { $('backup-status').textContent = '先选择备份文件'; return; }
  if (!pw || pw.length < 8) { $('backup-status').textContent = '备份密码至少 8 位'; return; }
  try {
    $('backup-status').textContent = '导入中…';
    const data = JSON.parse(await file.text());
    const res = await rawApi('/v1/backup/import', { method: 'POST', body: { backup_password: pw, data } });
    $('backup-status').textContent = `导入完成：快照 ${res.imported_snapshots}，Cookie ${res.imported_cookies}，跳过重复 ${res.skipped}`;
  } catch (e) { $('backup-status').textContent = '导入失败：' + e.message; }
};

async function loadSettings() {
  try {
    const s = await rawApi('/v1/settings');
    $('idle-minutes').value = s.idle_lock_minutes;
  } catch { /* ignore */ }
  const local = await chrome.storage.local.get(['autoBackupBeforeLock']);
  $('auto-backup-before-lock').checked = local.autoBackupBeforeLock !== false;
}
loadSettings();
refresh();
