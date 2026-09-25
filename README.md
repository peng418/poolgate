# PoolGate · AI 账号池网关

> **立项日期**：2026-09-24 ｜ **状态**：需求 / 设计方案已确认；实现到 **0.4.3**（安装包见 [Releases](https://github.com/peng418/poolgate/releases)）
> **仓库**：https://github.com/peng418/poolgate ｜ **安装包**：https://github.com/peng418/poolgate/releases
> **前身/借鉴**：`wild-work`（已定版 v2.4.7，代码冻结不再加功能）
> **交付形态**：Go 单二进制 + `go:embed` + 飞牛 fnOS 的 FPK

---

## 一句话

把多个 AI 编程助手的**订阅账号**汇成一个池子，对外暴露 **OpenAI / Anthropic 兼容接口**，
并提供一个**能看懂、不会骗人**的控制台。

wild-work 证明了这条路走得通；本项目的任务是把**真正能用的那部分**重做干净——
换掉堆砌式架构、砍掉已废渠道、把「静默失败」从代码里根除。

## 实现进度

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M0 | 分层骨架、错误模型、健康判据、注册表、控制台登录/首次设置/会话与失败锁定 | ✅ 完成 |
| M1 | QoderCN 适配器、账号池、兼容网关、凭证加载与 API Key | ✅ 完成并真出字 |
| M2 | WorkBuddyCN / TraeWork 适配器、粘性路由与轮换重试、Anthropic/Codex 兼容端点、**面板渠道授权** | ✅ 完成并实测 |
| M4 | 七屏补全（账号/模型/测速/日志/设置/总览）、一键诊断、真实抽样体检 | ✅ 完成 |
| M5 | 六渠道全接入（含千问办公网页端协议）、界面一比一还原原型 | ✅ 完成 |

**当前版本 0.4.3**（2026-09-25）：**接入源** —— 加一个「通用 OpenAI 兼容上游适配器」，官方 API
（Google AI Studio / 阿里百炼 / OpenRouter / DeepSeek / 智谱 / 火山方舟 …）**填 base_url + key 就能接**，
与登录授权式渠道共用同一个网关入口；面板新增「接入源」页（两类来源混排 + 三步向导 + **连通性测试**：
真发一条带工具定义的请求，确认拿得到结构化 `tool_calls` 才算能用）。
0.4.2：**工具调用（tools / tool_calls）打通** —— 之前网关把客户端的 `tools`
**静默丢掉**，上游只能把「要调用工具」写成文本，Studio / Claude Code 这类 agent 拿到的就是「不回复 / 一堆乱码」；
现在 OpenAI 与 Anthropic 两个入口都透传工具定义、输出标准 `tool_calls` / `tool_use`（含流式分片），
不支持工具调用的渠道（千问办公）改成**明确拒绝并给原因**，不再静默丢弃。
0.4.1：**授权不跳转** —— 面板内渲染能扫的**真二维码**（手机扫码完成登录，桌面零跳转）、
TraeWork 回调改用面板自己的局域网地址（去掉「必须与 PoolGate 同机」的限制）、
千问办公给「粘贴回调地址」兜底；点「授权」后**能出码就不弹浏览器**，只有出不了码的渠道才自动打开授权页。
更早几版：0.3.9 千问办公换成上游认的 device_token（治「重新登录也一样 401」）、0.3.7 面板内改密 + 凭证自动续期 +
401 不再把人踢回登录页、0.3.6「只下发可用模型」开关、0.3.5 对外基址一键复制 / 面板内显示与轮换网关 API Key / 新增账号自动拉余额。
逐版本细节见 [`src/README.md`](src/README.md) 的 0.3.x / 0.4.x 小节。

- 源码：[`src/`](src/)（Go module `poolgate`，`src/check.sh` 一键 gofmt + vet + race test + build，全绿）。
- 打包：[`fpk/`](fpk/)（`build-fpk.sh <版本>` → `fpk/dist/poolgate-<版本>.fpk`）；正式安装包见 [Releases](https://github.com/peng418/poolgate/releases)。
  **打包铁律**：`wizard/install|upgrade|config` 必须是非空 JSON 数组且每步有 `stepTitle` ——
  飞牛应用中心解析到空向导会直接报「应用包不符合系统要求」（实测 Code 10111 + nil pointer dereference）。
  `build-fpk.sh` 已内置这条校验，构建时会挡住。
- 实测记录见 [`src/README.md`](src/README.md)「真出字验证」一节（含 WorkBuddyCN 账号额度耗尽这条环境限制）。

## 导航

| 文件 | 内容 |
|---|---|
| [`docs/01-需求说明书.md`](docs/01-需求说明书.md) | 要解决什么问题、给谁用、功能清单与优先级、验收标准、明确不做的事 |
| [`docs/02-项目设计方案.md`](docs/02-项目设计方案.md) | 架构分层、渠道适配器契约、统一错误模型、数据模型、API 契约、前端栈、交付与里程碑。**§11 为评审结论** |
| [`docs/03-渠道能力矩阵.md`](docs/03-渠道能力矩阵.md) | **实测**数据：哪些渠道能用、哪些为什么不能用、模型级健康度 |
| [`prototype/index.html`](prototype/index.html) | UI 原型导航（7 屏，纯 HTML，双击即开） |
| [`prototype/07-login.html`](prototype/07-login.html) | 登录 / 首次设置 / 渠道授权（含授权失败态） |
| [`prototype/shots/`](prototype/shots/) | 原型截图（PNG，明暗双主题 + 移动端） |
| [`tools/audit-ui.mjs`](tools/audit-ui.mjs) | 双端布局审计：7 屏 × 桌面/手机，报横向溢出与 JS 报错（**每轮改 UI 都该跑**） |
| [`tools/audit-vue.mjs`](tools/audit-vue.mjs) | Vue 告警审计：换成开发构建跑一遍全部视图，抓「模板用了、setup() 没透出」这类生产构建下完全静默的问题 |
| [`tools/audit-picker.mjs`](tools/audit-picker.mjs) | 「添加账号」选渠道页的双端截图与布局检查 |

## 已确认的三件事（2026-09-24）

| 议题 | 结论 |
|---|---|
| 前端栈 | **Vue 3 + Vite + TypeScript** |
| 渠道范围 | 首发 **QoderCN / WorkBuddyCN / TraeWork**；其余 `Paused`（保留适配器位置） |
| 迁移方式 | **与 wild-work 并存**，稳定后再摘，全程可回退 |

详见 [设计方案 §11](docs/02-项目设计方案.md#11-评审结论2026-09-24)。

## 范围裁定：只留能用的

依据 `docs/03-渠道能力矩阵.md` 的当日实测（2026-09-24）：

| 渠道 | 裁定 | 依据 |
|---|---|---|
| **QoderCN** `qodercn/*` | ✅ **保留** | 14 个模型全通，0.6–5.6s，日签到可领 |
| **WorkBuddyCN** `workbuddy/*` | ✅ **保留** | 16 个模型全通，2.1–9.5s |
| **TraeWork** `traework/*` | ⚠️ **保留（带模型健康过滤）** | 27 个模型，部分（如 `deepseek-v4.1-flash`）稳定挂死 >230s |
| 千问办公 `qwenwork/*` | ⛔ **暂停** | 上游网关闸门，`503 Model catalog unavailable`；上游作者 Issue #31 确认是改版，客户端无可修 |
| QoderCOM `qodercom/*` | ⛔ **暂停** | 账号 0 积分 + 上游 `code 112` |
| WorkBuddyAI `workbuddyai/*` | ⛔ **暂停** | 额度耗尽，`429 Credits exhausted` |
| Qoder（旧）`qoder/*` | ⛔ **移除** | 已被 QoderCN / QoderCOM 取代 |

> 暂停 ≠ 删除：适配器仍可实现，但默认不下发给客户端、不在面板显示为可用，
> 以免再出现「面板全绿、实际不可用」的误导。

## 三条设计红线（来自 wild-work 的实测教训）

1. **零静默失败** —— 任何上游异常都必须在**客户端**可见，并落一条能定位的日志。
   wild-work v2.4.7 修的就是这个：流式分支把上游错误丢掉，客户端拿到 `HTTP 200 + 0 字节`，
   既不回复也不报错，排查方向被带偏好几天。
2. **健康判据必须是「收到了有效内容」**，不是 `HTTP 200`。上游普遍把错误包在 200 的 SSE 信封里，
   看状态码等于没看。
3. **渠道可插拔** —— 新增一个渠道 = 写一个适配器 + 一份能力声明，核心代码零改动。
   上游改版是常态（千问办公就是例子），架构必须让「摘掉一个渠道」是分钟级操作。

## 当前状态

- [x] wild-work 定版 v2.4.7（tag `v2.4.7`；安装包见 [wildwork-fpk Releases](https://github.com/peng418/wildwork-fpk/releases)）
- [x] 需求整理
- [x] 项目设计方案
- [x] **评审确认三项**（前端栈 / 渠道范围 / 迁移方式）——已写进文档
- [x] UI 原型（7 屏 + 截图，含登录与渠道授权）
- [x] 实现 M0–M5：六渠道、兼容网关、控制台七屏、面板渠道授权（见 [`src/README.md`](src/README.md)）
- [x] 装机与迭代到 **0.4.1**（飞牛 FPK，已装 0.1.3 → 0.4.1，含用户实测反馈修复）
- [ ] 与 wild-work 并存观察 → 稳定后摘除旧服务（迁移方式见设计方案 §11）
