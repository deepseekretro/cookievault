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
  try { await rawApi('/v1/auth/lock', { method: 'POST' }); msg('已锁定'); refresh(); }
  catch (e) { msg('锁定失败：' + e.message); }
};
$('change').onclick = async () => {
  try { await rawApi('/v1/auth/change-password', { method: 'POST', body: { old_password: $('old-pw').value, new_password: $('new-pw').value } }); msg('主密码已修改'); }
  catch (e) { msg('修改失败：' + e.message); }
};

refresh();
