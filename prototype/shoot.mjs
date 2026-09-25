// PoolGate 原型截图：6 屏 × 明暗两主题 + 1 张移动端。
// 运行：node shoot.mjs
import { createRequire } from 'node:module';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// playwright 解析：优先 PW 环境变量（项目里没有 node_modules 时用），否则按常规包名加载。
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PW || 'playwright');

const HERE = path.dirname(fileURLToPath(import.meta.url));
const OUT = path.join(HERE, 'shots');
fs.mkdirSync(OUT, { recursive: true });

// [文件, 标签, hash] —— hash 用于登录页切换视图
const PAGES = [
  ['index', '原型导航', ''],
  ['07-login', '登录', '#signin'],
  ['07-login', '首次设置', '#setup'],
  ['07-login', '渠道授权', '#oauth'],
  ['01-overview', '总览', ''],
  ['02-accounts', '账号', ''],
  ['03-models', '模型与费率', ''],
  ['04-benchmark', '测速与体检', ''],
  ['05-logs', '日志与诊断', ''],
  ['06-settings', '设置', ''],
];

const browser = await chromium.launch({
  executablePath: process.env.CHROME || '/usr/bin/chromium',
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--font-render-hinting=none'],
});

for (const theme of ['light', 'dark']) {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 1000 }, deviceScaleFactor: 1 });
  const page = await ctx.newPage();
  for (const [slug, label, hash] of PAGES) {
    // 同一文件多个视图时，用 hash 区分文件名
    const variant = hash ? '-' + hash.replace('#', '') : '';
    const shot = slug + variant + '-' + theme + '.png';
    await page.goto('file://' + path.join(HERE, slug + '.html') + hash, { waitUntil: 'load' });
    await page.evaluate(
      (t) => document.documentElement.setAttribute('data-theme', t),
      theme
    );
    await page.waitForTimeout(200); // 等二维码点阵与 spinner 渲染
    const file = path.join(OUT, shot);
    await page.screenshot({ path: file, fullPage: true });
    console.log(`  ${theme.padEnd(5)} ${label.padEnd(8)} -> ${path.basename(file)}`);
  }
  await ctx.close();
}

// 移动端：账号页（看余额/开关账号）+ 登录页（单列收拢）
const m = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true });
const mp = await m.newPage();
for (const [slug, hash, out] of [
  ['02-accounts', '', '02-accounts-mobile.png'],
  ['07-login', '#signin', '07-login-mobile.png'],
  ['07-login', '#oauth', '07-login-oauth-mobile.png'],
]) {
  await mp.goto('file://' + path.join(HERE, slug + '.html') + hash, { waitUntil: 'load' });
  await mp.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
  await mp.waitForTimeout(200);
  await mp.screenshot({ path: path.join(OUT, out), fullPage: true });
  console.log('  mobile ->', out);
}
await m.close();

await browser.close();
console.log('done →', OUT);
