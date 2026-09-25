/* PoolGate 原型 · 交互（主题切换 + 图表提示气泡） */
(function () {
  // 主题：跟随系统 → 手动切换后记住
  var saved = null;
  try { saved = localStorage.getItem('pg-theme'); } catch (e) {}
  if (saved) document.documentElement.setAttribute('data-theme', saved);

  window.pgToggleTheme = function () {
    var cur = document.documentElement.getAttribute('data-theme');
    if (!cur) {
      cur = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    }
    var next = cur === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try { localStorage.setItem('pg-theme', next); } catch (e) {}
    var b = document.getElementById('themeBtn');
    if (b) b.textContent = next === 'dark' ? '☀ 亮色' : '☾ 暗色';
  };

  // 提示气泡：任何带 data-tip 的元素
  document.addEventListener('DOMContentLoaded', function () {
    var b = document.getElementById('themeBtn');
    if (b) {
      var cur = document.documentElement.getAttribute('data-theme');
      if (!cur) cur = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
      b.textContent = cur === 'dark' ? '☀ 亮色' : '☾ 暗色';
    }
    var tip = document.createElement('div');
    tip.id = 'tip';
    document.body.appendChild(tip);
    document.addEventListener('mouseover', function (e) {
      var t = e.target.closest('[data-tip]');
      if (!t) return;
      tip.innerHTML = t.getAttribute('data-tip');
      tip.classList.add('on');
    });
    document.addEventListener('mousemove', function (e) {
      if (!tip.classList.contains('on')) return;
      tip.style.left = Math.min(e.clientX + 12, innerWidth - tip.offsetWidth - 8) + 'px';
      tip.style.top = (e.clientY + 14) + 'px';
    });
    document.addEventListener('mouseout', function (e) {
      if (e.target.closest('[data-tip]')) tip.classList.remove('on');
    });
  });
})();
