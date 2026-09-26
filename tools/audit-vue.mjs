// Vue 告警审计：把生产的 vue.global.prod.js 换成开发构建跑一遍，收集控制台告警。
//
// 为什么需要这个：生产构建下 Vue 对「模板里用了、setup() 却没 return 的标识符」
// **完全静默** —— 不报错、不警告，渲染成 undefined。症状是「按钮文案不变」「v-if
// 永远不成立」这类看起来像样式问题的怪事。0.3.5 的 keyPlain 就是栽在这里：
// 值改了，按钮还写着「显示明文」。
//
// 开发构建会打印 `Property "keyPlain" was accessed during render but is not
// defined on instance`，正好补上这个盲区。所以本脚本用 dev 构建驱动全部视图，
// 把 Vue 的 warn/error 当作失败。
//
// 用法: node tools/audit-vue.mjs http://127.0.0.1:5114/ [密码]
//   首个参数是面板地址；密码默认 pg-test-1234（与 audit-ui.mjs 一致）。
//   dev 构建缓存在 tools/.cache/vue.global.dev.js，缺了就现下（unpkg）。
//   拿不到 dev 构建时**跳过**并给出提示（不当成失败：离线环境也要能跑）。
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// playwright 解析：优先 PW 环境变量（项目里没有 node_modules 时用），否则按常规包名加载。
// 例：PW=/path/to/node_modules/playwright/index.mjs node tools/audit-ui.mjs http://127.0.0.1:5014/
const PW = process.env.PW || "playwright";
const VUE_VERSION = "3.5.13";
const here = path.dirname(fileURLToPath(import.meta.url));
const cacheDir = path.join(here, ".cache");
const cacheFile = path.join(cacheDir, "vue.global.dev.js");

const url = process.argv[2];
const password = process.argv[3] || "pg-test-1234";
if (!url) {
  console.error("用法: node tools/audit-vue.mjs <面板地址> [密码]");
  process.exit(2);
}

async function devVue() {
  if (fs.existsSync(cacheFile)) return fs.readFileSync(cacheFile, "utf8");
  try {
    const r = await fetch(`https://unpkg.com/vue@${VUE_VERSION}/dist/vue.global.js`);
    if (!r.ok) throw new Error("HTTP " + r.status);
    const txt = await r.text();
    fs.mkdirSync(cacheDir, { recursive: true });
    fs.writeFileSync(cacheFile, txt);
    console.log(`已缓存 Vue dev 构建到 ${cacheFile}`);
    return txt;
  } catch (e) {
    console.log(`跳过 Vue 告警审计：拿不到 dev 构建（${e.message}）`);
    return null;
  }
}

const dev = await devVue();
if (!dev) process.exit(0);

const { chromium } = await import(PW);
const b = await chromium.launch({ executablePath: "/usr/bin/chromium", args: ["--no-sandbox"] });
const warnings = [];
const errs = [];

for (const [name, w, h] of [["桌面", 1400, 900], ["手机", 390, 844]]) {
  const ctx = await b.newContext({ viewport: { width: w, height: h } });
  const p = await ctx.newPage();
  // 用 dev 构建顶掉生产构建（路径里带 vue.global.prod.js 的都换掉）。
  await p.route("**/vue.global.prod.js", route => route.fulfill({ contentType: "application/javascript", body: dev }));
  p.on("console", m => {
    if (m.type() === "warning" || m.type() === "error") {
      const t = m.text();
      // Vue 的加载提示与无关噪音不算问题。
      if (t.includes("You are running a development build of Vue")) return;
      if (t.includes("Download the Vue Devtools")) return;
      // 未登录时面板会探一次 /api/session 并拿到 401，这是设计如此（前端据此显示
      // 登录页），浏览器仍会打一条 error —— 别把它当问题。
      const where = (m.location() && m.location().url) || "";
      if (t.includes("Failed to load resource") && where.includes("/api/session")) return;
      warnings.push(`[${name}] ${t}`);
    }
  });
  p.on("pageerror", e => errs.push(`[${name}] ${e.message}`));

  await p.goto(url, { waitUntil: "networkidle" });
  const ins = p.locator("input[type=password]");
  if (await ins.count() > 1) {
    await ins.nth(0).fill(password);
    await ins.nth(1).fill(password);
    await p.getByRole("button", { name: "创建并进入" }).click();
  } else {
    await ins.first().fill(password);
    await p.getByRole("button", { name: "登录" }).click();
  }
  await p.waitForTimeout(2500);

  for (const [k, label] of [["overview", "总览"], ["providers", "接入源"], ["accounts", "账号"], ["models", "模型与费率"],
    ["benchmark", "测速与体检"], ["logs", "日志"], ["settings", "设置"]]) {
    const nav = p.locator(".nav a", { hasText: label }).first();
    // 入口不在就跳过（例如「接入源」被 SHOW_PROVIDERS 隐藏）；恢复入口后自动重新覆盖。
    if (await nav.count() === 0) { console.log(`  跳过「${label}」：导航里没有这个入口`); continue; }
    await nav.click({ timeout: 5000 });
    await p.waitForTimeout(900);
    // 设置页多戳几下：Key 的两态（掩码/明文）文案不同，两态都要渲染一遍。
    if (k === "settings") {
      const reveal = p.locator("button", { hasText: "显示明文" });
      if (await reveal.count()) {
        await reveal.click();
        await p.waitForTimeout(500);
        const hide = p.locator("button", { hasText: "隐藏" });
        if (await hide.count()) await hide.click();
        await p.waitForTimeout(300);
      }
    }
  }
  // 加号流程（若该实例有可面板授权的渠道）：选渠道页与授权页也要渲染一遍。
  await p.locator(".nav a", { hasText: "账号" }).first().click();
  await p.waitForTimeout(500);
  const add = p.locator("button", { hasText: "添加账号" });
  if (await add.count()) {
    await add.click();
    await p.waitForTimeout(800);
    const cancel = p.locator("a", { hasText: "取消" });
    if (await cancel.count()) await cancel.first().click();
  }
  await ctx.close();
}
await b.close();

const uniq = [...new Set(warnings)];
console.log(`Vue 告警 ${uniq.length} 条，页面报错 ${errs.length} 条`);
for (const w of uniq) console.log("  " + w);
for (const e of [...new Set(errs)]) console.log("  !! " + e);
if (uniq.length || errs.length) process.exit(1);
console.log("通过：模板里的标识符都已在 setup() 中透出。");
