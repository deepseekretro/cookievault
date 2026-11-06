const $ = (id) => document.getElementById(id);
(async function () {
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  let domain = '';
  try { domain = new URL(tabs[0].url).hostname; } catch { /* ignore */ }
  $('info').textContent = '当前域名：' + (domain || '(无法识别)');
  $('save').onclick = async () => {
    const label = $('label').value.trim();
    if (!label) { $('msg').textContent = '请填写账号标签'; return; }
    $('msg').textContent = '保存中…';
    const r = await chrome.runtime.sendMessage({
      type: 'SAVE',
      payload: { label, note: $('note').value.trim(), domain, sourceUrl: tabs[0].url },
    });
    $('msg').textContent = r.ok ? `已保存（${r.snapshot.cookie_count} 个 Cookie）` : '失败：' + r.error;
  };
  $('clear').onclick = async () => {
    if (!domain) { $('msg').textContent = '无法识别当前域名'; return; }
    if (!confirm(`确定清空 ${domain} 的全部 Cookie？刷新页面后即为未登录状态。`)) return;
    const r = await chrome.runtime.sendMessage({ type: 'CLEAR', domain });
    $('msg').textContent = r.ok ? `已清空 ${r.result.removed} 个 Cookie，请刷新页面` : '失败：' + r.error;
  };
  $('open').onclick = async () => {
    try { await chrome.sidePanel.open({ windowId: tabs[0].windowId }); }
    catch { await chrome.tabs.create({ url: chrome.runtime.getURL('sidepanel.html') }); }
  };
  $('options').onclick = () => chrome.runtime.openOptionsPage();
})();
