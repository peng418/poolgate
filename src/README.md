# PoolGate 源码

Go 单二进制 + `go:embed`，交付形态沿用 wild-work 的 FPK 路线。

## 构建与校验

Go 工具链不在 PATH（本机在 `/tmp/go125/go/bin/go`），脚本已处理：

```sh
./build.sh 0.1.2        # → build/poolgate（版本号会自报在面板上）
./check.sh              # gofmt + vet + race test + build，提交前必跑
```

打 FPK：`cd ../fpk && cp ../src/build/poolgate payload/poolgate && ./build-fpk.sh <版本>`。
产物在 `fpk/dist/`；`build-fpk.sh <版本> <目录>` 的第二参数可让它再拷一份到指定目录。
注意 `wizard/*` 必须是**非空** JSON 数组（每个 step 要有 `stepTitle`）——空数组会让
飞牛应用中心报「应用包不符合系统要求」，构建脚本已加校验挡住这种情况。

### 前端资源的前缀铁律（踩过一次，别再犯）

飞牛应用网关把页面挂在 `/app/poolgate` 下，**转发时会剥掉这个前缀**，浏览器侧必须
自己带回来。两条硬约束，`build.sh` 已加构建期校验：

1. `index.html` 里所有资源引用用相对路径（`./vue.global.prod.js`），不能用 `/…`；
2. 接口路径写成**单引号**字面量 `'/api/…'`，再由 `base` 前缀拼接。
   网关会改写正文里双引号包裹的 `"/api/…"` 并补一次前缀，若再叠上自己的 `base`
   就变成 `/app/poolgate/app/poolgate/api/…`。前端所有接口都走同一个 `api()` 封装，
   前缀只在那里拼一次。

违反第 1 条的症状极具误导性：Vue 脚本 404 → 页面**不报错也不渲染**，整屏都是
没替换的 `{{ }}`。此外 `build.sh` 还会把内联 `<script>` 抽出来跑 `node --check`：
脚本一有语法错同样整页不挂载，症状是「白屏 + 遮罩挡住点击」，比 404 更难查。

手工调用需带环境：`GOFLAGS=-mod=mod GOPATH=/tmp/go GOMODCACHE=/tmp/gomodcache GOTOOLCHAIN=local`。

## 跑起来

```sh
# 首次运行：未设置密码时，打开面板会落到设置向导
./build/poolgate -addr 127.0.0.1:5014 -conf /tmp/pg/conf -data /tmp/pg/data

# 命令行设置/重置密码（忘记密码的唯一入口）
./build/poolgate -conf /tmp/pg/conf admin set-password '<密码>'
./build/poolgate -conf /tmp/pg/conf admin reset-password '<密码>'
```

## 目录结构

```
cmd/poolgate/        入口：flag、子命令、路由装配
internal/
  errs/              统一错误模型 —— 红线一（可见性）与 D4（归一）
  channel/           适配器契约与能力声明 —— 红线三（可插拔）
  registry/          渠道注册表 —— 摘除渠道 = 改 Status，核心零改动
  health/            健康判据 —— 红线二（收到有效内容才算通过）
  store/             管理员凭证（Argon2id）与会话（含失败锁定）、账号凭证读写、API Key
  console/           面板 HTTP 接口（鉴权、会话、渠道清单、可观测、渠道授权）
  boot/              渠道注册集中处
  adapter/qodercn/   QoderCN 适配器（COSY 签名 + QoderEncoding + 嵌套 SSE + 设备流授权）
  adapter/workbuddy/ WorkBuddyCN 适配器（OpenAI 兼容 SSE 直通 + state 轮询授权）
  adapter/traework/  TraeWork 适配器（SOLO 事件序列 + 本机回调授权）
  pool/              账号池（冷却/禁用/轮换）
  gateway/           兼容网关 /v1/chat/completions + /v1/messages + /v1/responses + /v1/models
  router/            路由层（粘性/轮转/重试/分档冷却）
  webui/             前端产物 go:embed
```

依赖关系是单向的：下层不知道上层。核心层零渠道专有代码。

## 当前进度

### M0（已完成）

- [x] 分层骨架 + 错误模型 + 健康判据 + 注册表
- [x] 登录 / 首次设置 / 会话 / 失败锁定（含 Argon2id、HttpOnly Cookie）
- [x] 渠道清单接口（3 启用 + 3 暂停，暂停不下发）
- [x] 前端挂载（总览 / 账号 / 渠道授权 / 模型 / 日志 / 设置，单文件 Vue3 + go:embed）

### M1（已完成）— 单渠道闭环

- [x] `channel.Channel` 契约补 `Chat`（`ChatRequest` / `Stream` / `ChatCompletionChunk`）
- [x] QoderCN 适配器（`internal/adapter/qodercn`）：COSY 签名 + QoderEncoding + 嵌套 SSE 剥壳 → 标准 OpenAI chunk，错误 `Classify` 归一（移植 wild-work 已实测协议）
- [x] 账号池 `internal/pool`：按渠道分组 + 冷却/禁用状态机 + 轮换；`UpstreamFault` 不计账号错误（D4）
- [x] 网关 `internal/gateway`：`/v1/chat/completions`（流式 + 非流式）+ `/v1/models`，API Key 鉴权、流式错误回传、暂停渠道拒绝
- [x] 凭证加载（`store/creds.go` 兼容 wild-work 嵌套/扁平格式）+ API Key 首次生成（`store/apikey.go`）
- [x] `check.sh` 全绿（gofmt + vet + race test + build）

### M2（已完成）— 多渠道 + 面板授权

- [x] WorkBuddyCN 适配器（OpenAI 兼容 SSE 直通，信封剥壳，额度类错误归 `HardCredit`）
- [x] TraeWork 适配器（SOLO 事件序列 `metadata/output/token_usage/done/error` → 标准 chunk）
- [x] 粘性路由 / 轮换重试 / 分档冷却（`internal/router`；粘性有次数上限与 LRU 上限）
- [x] 网关补 Anthropic `POST /v1/messages` 与 Codex `POST /v1/responses`
- [x] 控制台渠道授权：`POST /api/login/start` · `GET /api/login/poll` · `POST /api/login/cancel`
      （QoderCN 设备流 / WorkBuddyCN state 轮询 / TraeWork 本机回调，均带失败原因）
- [x] 授权成功 → 凭证落盘（0600 原子写）+ 立即入池，无需重启

### 真出字验证（2026-09-24，接现网凭证）

- QoderCN `qodercn/qwen3.8-flash`：流式与非流式均真出字，usage 正常。
- TraeWork `traework/glm-5.2`：非流式回「1+1等于2。」；流式拼出「一、二、三！」并带 usage + `[DONE]`。
- WorkBuddyCN：鉴权与请求构造可用（上游返回业务错误而非 401），但**该账号额度已用尽**
  （HTTP 429 + `code 14018`「额度已用尽」），因此无法真出字 —— 这次实测暴露出额度类错误
  被判成 `SoftRate`（60s 后继续重试），已改为 `HardCredit`（12h 冷却），并加了用例钉住。
- 模型目录：`/v1/models` 真实返回 52 个模型（qodercn 14 / traework 28 / workbuddy 10）。

### M4（已完成）— 七屏补全 + 可观测 + 交付修正

对照 `docs/01-需求说明书.md` 与 `prototype/` 七屏逐项核对后补齐（此前只有 6 个残缺视图，
F1.7/F1.8/F1.9、F4.x、F5.x、F6.x 大半未实现）：

- [x] **账号屏**：按渠道分组、余额/到期/冷却/签到/启停原因；签到、刷新余额、全部签到、
      启用/禁用、删除（F1.7/F1.8/F1.9）。`CreditsKnown` 为 false 时显示「未知」而不是 0。
- [x] **模型屏**：渠道×模型矩阵，能力位 + 来源标注（上游/本地/未知）、上下文窗口、健康标记、
      下发开关；被剔除的模型**在 `/v1/models` 与直连调用两侧都拒绝**（F4.2/F4.4）。
- [x] **测速屏**（原型有、实现此前完全缺失）：`internal/health/runner.go` 真实抽样体检 +
      全量测速，固定 3 并发，判据复用 `health.Judge`；结果含 TTFT、失败原因、上游原话；
      体检历史可回看（F5.1/F5.3/F5.5）。
- [x] **日志屏**：按渠道/状态/时间过滤、错误分类聚合、导出 CSV（含 BOM，Excel 不乱码）、
      一键诊断（本地问题 vs 上游问题 + 逐步证据链）（F6.1/F6.2/F6.3）。
- [x] **设置屏**：服务/鉴权/路由冷却/健康探测/签到保活/渠道开关/备份/危险区，改动即生效、
      落盘可回滚（F6.4/F6.6）。含凭证的导出**明确拒绝**而不是给个假包。
- [x] **总览屏**：四张指标卡（含今日失败率、TTFT 中位）、渠道健康（含模型通过数）、
      TTFT 条形图 + 表格视图、错误聚合、待处理告警。
- [x] 深浅主题跟随系统并持久化；390px 宽无横向溢出；危险操作全部二次确认并写明后果。
- [x] 密码强度**只提示不拦截**（自用单管理员系统，锁的是自己）。

**真上游验证（2026-09-24 晚，接现网 QoderCN 凭证）**：模型目录真实返回 14 个模型；
抽样体检 3/3 通过（真实 TTFT 1.1–5.2s）；全量测速 14/14 通过、耗时 5.3s；
一键诊断给出「正常：本地配置与上游都可用」并附逐步证据；对无账号的暂停渠道
正确判为「本地配置问题：没有可用账号」。用完即删凭证副本。

**本次实测揪出的两个真 bug**（都已加用例钉住）：

1. `store.decodeSettings` 把目标结构体序列化后再解回自己 —— 等于没读文件，
   表现为「面板显示已保存、重启后设置全丢」。改为按 JSON 键存在性覆盖。
2. 设置屏 `Object.assign(form, r.settings)` 触发深层 watcher 自激，
   「读→存→读→存」无限循环（实测静置 6 秒发 11 次写）。加 `silent` 标志挡住回读。

### M5（已完成）— 六渠道全接入 + 界面一比一还原原型

**界面**：直接照搬 `prototype/assets/poolgate.css` 的设计令牌与组件（分类色 3 槽、
状态 pill、单色阶条形、`.ch` 渠道身份点、`.cols` 柱状、`banner`、`kv`、`field` 等），
七屏模板按原型 DOM 结构逐屏重建。原型里的元素一个不少：近 7 日请求量柱状图、
P95 延迟、成功率列、待处理列表、错误分类条形、一键诊断证据链、表格视图折叠、
登录页左栏价值主张 + 五视图（登录/首次设置/授权/成功/失败）。

**渠道**：六家全部接入（此前只有三家，另三家是「保留位置不注册实现」的占位声明）。

| 渠道 | 协议 | 实测结果 |
|---|---|---|
| QoderCN | COSY 签名 + QoderEncoding + 嵌套 SSE | 真出字 ✓ |
| WorkBuddyCN | OpenAI 兼容 SSE + 信封剥壳 | 额度耗尽（429 code 14018） |
| TraeWork | SOLO 事件序列 | 真出字 ✓ |
| **千问办公** | **网页端 chat-ws（WebSocket + JSON-RPC）** | **真出字 ✓ 3/3 通过** |
| **QoderCOM** | COSY（与 QoderCN 同框架，仅域名不同） | 真出字 ✓ |
| **WorkBuddyAI** | OpenAI 兼容 SSE（国际版独立域名） | 额度耗尽（429 code 14018） |

**千问办公改走网页端（本次主要工作）**：桌面网关 `gateway.qwenwork.cn` 被上游加了闸门
（正确签名与档位也返回信封 503，wild-work Issue #31）。实测网页域 `qwenwork.cn` 同账号可用：

```
cookie token（与桌面端同源，直接可用）
  → POST /api/chat-sessions 建会话
  → GET /api/chat-ws（WebSocket，token 走 query 参数）join + new_prompt
  → 服务端推 session/update 帧：agent_thought_chunk→reasoning_content、
     agent_message_chunk→content
  → session_status 由 running 回 idle 即本轮结束
```

实测确认的三个坑（都已处理）：

1. **同账号并发被拒**：并发建会话返回 `CONCURRENT_OPERATION`「Already handling session
   start request for this user」→ 适配器按账号串行化对话（锁持有到流关闭）。
   修前抽样体检 2/3 通过，修后 3/3。
2. **超时不能报成 Parse**：`ctx.Done()` 原样返回会被归一成 `Parse`（「解析不了」），
   与「等超时了」是两件事 → 显式包成 `Transport`。
3. **目录失败要回退静态表**：否则面板会显示「一个模型都没有」；回退项标
   `Source=local`，不冒充上游值。

**WorkBuddyAI 的目录解析 bug（实测揪出）**：`doJSON` 已把外层 `{code,msg,data}`
剥掉，但 `extractCatalogModels` 仍按带信封的形态解析 → 永远解析不到，静默回退静态表
（13 个 vs 动态 19 个）。已修，并补了两种响应形态的用例。

**router 测试偶发失败（实测揪出）**：`newPool` 注释写「A 余额更高」但两个账号余额相同，
而 `pool.Pick` 遍历 map 顺序随机 → 「先试 A 再换到 B」的用例偶发失败。已把余额改成真的不等。

### 0.3.1 — 两个「看起来能用其实不能用」的修复

**① 全新安装（0 个账号）却显示 23 个模型。** `modelsFor` 在没有可用账号时仍回退
到适配器的静态兜底表，于是刚装完的面板与 `/v1/models` 都列出一堆模型，用户以为装完即用，
点下去全部失败。修法：**模型必须挂在账号上** —— 该渠道一个账号都没有就返回空，
静态兜底表也不例外（面板与网关两侧同改）。模型是「用某个账号向上游问出来的」，
没有账号就不存在可用模型。

**② 「＋ 添加账号」不能选渠道，且登录后点了没反应。** 两个原因叠在一起：

- 后端只有一个 `implemented` 字段（含义是「有适配器」），前端拿它当「能面板授权」用，
  于是直接挑了第一个渠道，用户没得选；点到没实现 `channel.Authorizer` 的渠道
  （千问办公 / QoderCOM / WorkBuddyAI）还会得到一个点了没反应的按钮。
  修法：新增 `panel_auth` 字段（真的查 `channel.Authorizer`）+ `panel_auth_note`
  说明替代做法 + `account_count`，与 `implemented` 分开。
- 授权相关的视图被关在 `v-if="!authed"` 的登录容器里，登录后在账号页点「添加账号」
  什么都不发生。修法：把「选渠道 / 授权 / 成功」提成独立覆盖层（`authFlowOpen`），
  登录前后都能用；取消/完成回到面板而不是登录页。

**③ 模型目录的加载态。** 目录要真实向上游拉取（实测 5–6 秒），期间页面显示
「0 个模型」，看起来像装坏了。加了加载提示，并把空态文案改成引导用户先去加号。

三个问题都补了回归用例（`internal/console/catalog_test.go`）。

### 0.3.2 — 六渠道面板授权全接入 + 总览不再虚报

**① 总览「可用渠道 6 / 6」是假的。** 那张卡按「状态为启用」计数，与有没有账号无关 ——
全新安装 0 个账号也显示 6/6，用户以为六个渠道都能用。修法：改为按「真的有号可用」计数，
显示 `0 / 6 启用` + 「还没有可用账号」；有账号但部分渠道缺号时显示「N 个渠道没账号」。

**② 后三个渠道没有面板授权。** 之前千问办公 / QoderCOM / WorkBuddyAI 只有适配器、
没实现 `channel.Authorizer`，加号里只能看不能点。现已全部实现：

| 渠道 | 授权方式 | 实测 |
|---|---|---|
| WorkBuddyAI | `auth/state` → 浏览器登录 → `auth/token` 轮询 | 返回真实授权页 ✓ |
| QoderCOM | OAuth 设备流（PKCE + `deviceToken/poll`） | 返回真实授权页 ✓ |
| 千问办公 | **OAuth2 授权码 + 本机回调**（PKCE） | 授权页可达、回调链路跑通 ✓ |

**千问办公为什么不走扫码**：网页版的 QR 登录要阿里设备指纹（`bx-ua` / `bx-umidtoken` /
`bx_et`），那串由钉钉/支付宝 SDK 在浏览器端动态生成 —— 实测传空值、传假值都被拒
（`invalid QR login request`），服务端无法复现。OAuth 授权码这条路不需要它，
用的是桌面端同一个 public client（`qwenwork-desktop-app`，非密钥）。

**六个渠道现在都支持面板加号**，`panel_auth` 全部为 true。

### 0.3.3 — 双端（桌面 / 手机）布局修正

用户指出：**每次改动都要考虑双端**。这轮把七屏 + 登录页在 1400×900 与 390×844 各跑一遍，
揪出三处问题：

**① 登录页往下滑能看到面板。** 主面板用的是 `v-else-if="!authFlowOpen"` ——
未登录时 `authFlowOpen` 为 false，于是**登录页与面板同时渲染**，手机往下一滑就露出面板内容。
修法：改成 `v-else-if="authed"`，未登录时面板根本不渲染。

**② 选渠道是手机布局。** `auth-card` 默认 `max-width:420px`（为登录表单定的），
选渠道要展示六张卡，沿用窄栏就成了手机布局。修法：选渠道用 920px 两列栅格、
授权视图 620px（有二维码 + 长回调地址），手机端（≤820px）一律回到单列。

**③ 手机端总览与设置页横向溢出。** 栅格/弹性子项默认 `min-width:auto`，
宽内容（表格、长路径）把整页撑宽。修法：`.grid > *`、`.card`、`.nav` 补 `min-width:0`，
窄屏导航条自身横向滚动。宽表格仍按原型要求**在卡片内**横向滚动，页面本身不滚。

**可复现的审计脚本**：`tools/audit-ui.mjs`（七屏 × 双端，报告横向溢出与 JS 报错）、
`tools/audit-picker.mjs`（加号流程双端）。`node tools/audit-ui.mjs <url>`，当前 0 问题。

### 0.3.4 — 渠道身份色补齐（六个渠道的点）

**问题**：模型/渠道列表里渠道名前的那个小方块是**渠道身份色**（原型设计令牌
`--s1/--s2/--s3`，色觉障碍下靠它区分渠道）。原型只定义了 3 个槽（当初只有 3 家渠道），
后接入的三家没有对应色槽，`channelClass` 返回空串 → `.k` 渲染成一个没有背景色的空方块，
看起来就是「一部分渠道有点、一部分没有」。

**修法**：补槽 4–6，取值来自原型用的同一套参考调色板（黄 `#eda100` / 洋红 `#e87ba4` /
绿 `#008300`），明暗两套都给。

用 dataviz 校验器实测选色（不靠眼睛判断）：

| 模式 | 相邻对 CVD ΔE | 常视 ΔE | 结果 |
|---|---|---|---|
| 亮色 | 9.1 | 19.6 | 全通过 |
| 暗色 | 8.4 | 19.3 | 全通过 |

试过用「红」补槽：红与橙（槽 2）在 CVD 下 ΔE 只有 5.6，**实测定过不过**，故改用绿。

顺带修掉同一处的一个隐藏问题：**暂停渠道原本也返回空串**（同样渲染成空方块）。
原型里暂停渠道是**灰点**，现在统一给 `k-off`（`--ink-3`），与原型一致。

### 0.3.5 — 拿地址、拿 Key、看余额都不用再绕路

用户实测反馈两条（「新增账号后默认没有刷新余额」「API 地址应该能直接复制、Key 应该能直接生成」），
加上追查过程中发现的同类问题：

1. **新增账号后余额是「未知」**：授权成功只做了落盘 + 入池，没人去问上游要余额，用户看到
   「刚加完号，余额一片未知」以为没接上。现改为授权成功时**就地问一次** `Channel.Balance`
   （`console/ops.go: refreshOneBalance`），把结果写进池子并在授权完成页显示；取不到就带回
   原因（`balance_error`），**不把授权成功改成失败**。导入 wild-work 账号同样顺手刷一遍，
   且导入后会重建账号池（以前导完不重建，得重启才看见）。
2. **账号页没有单行刷新**：`refreshOne` 早就写好了却没人调用（死代码），每行的「刷新余额」
   按钮补上。
3. **API 地址要能直接复制**：设置页「对外基址」加一键复制 + 「用探测值」；探测值由
   `detectClientBase` 按「当前访问用的主机名 + 进程实际监听的端口」推出。
4. **网关 API Key 能在面板里拿**：新增 `GET /api/apikey`（掩码）、`POST /api/apikey/reveal`
   （显式回显明文）、`POST /api/apikey/rotate`（换新，旧 Key 立即失效，带二次确认）。
   以前只能登服务器 `cat config.json`。明文默认不出现、离开设置页自动收起、不落日志。

**顺带修掉三个「平时看不出来」的问题**：

- **设置里的监听地址/端口从来没生效过**：进程只听启动参数 `-addr`，安装包的启动脚本
  又总是显式给 `-addr`，于是用户改了端口、看到「已保存」，实际什么都没发生。现在
  `-addr` 未被显式指定时设置才生效；面板显示「当前实际监听」并明说「以启动参数为准」。
  设置页副标题也从「改动即时生效」改成「监听地址需重启生效」。
- **模板里 `<` 会把插值切碎**：`{{ paths.creds_dir || '<conf>/creds/' }}` 里的 `<conf>`
  被 HTML 解析器当成标签，Vue 报 `Invalid end tag`，那段渲染不出来（只有 creds_dir 为空时
  才显形）。已改写并在 `build.sh` 加构建期校验。
- **「从 wild-work 导入账号」失败只说「导入失败」**：飞牛跨应用目录默认 0600，PoolGate 的
  进程读不了 wild-work 的凭证，原因只在日志里。现在错误消息带上具体文件与权限原因，
  并说明该怎么绕（以管理员身份复制 json 到 `conf/creds/`）。

**新增可复用审计脚本** `tools/audit-vue.mjs`：把 `vue.global.prod.js` 换成开发构建跑一遍
全部视图，收集 Vue 告警 —— 生产构建对「模板用了、setup() 没 return 的标识符」是**完全静默**
的（0.3.5 的 `keyPlain` 就栽在这里：值变了、按钮文案不变），开发构建会明说
`Property "keyPlain" was accessed during render but is not defined on instance`。
已实测：注入该 bug 能抓到，修复后 0 告警。

### 0.3.6 — 设置里加「只下发可用模型」开关

需求：设置页加一个按钮，开启后接口只返回可用的模型。

**语义（这条是刻意的，别顺手改成白名单）**：开关只隐藏**体检明确失败**的模型；
**未体检的照常下发** —— 「不知道」不等于「不可用」，把未体检的一起藏掉，
刚装完、还没跑过体检的机器会向客户端返回一个**空模型列表**，那看起来像服务坏了。
开关也只影响 `/v1/models` **列表**：直连调用不拦（探测失败可能是一次性的，
拦下来会把「偶发失败」变成必须去面板操作才能恢复的故障；调用失败本就带原因回传）。

**实现**：

- `store.Settings.OnlyHealthyModels`（默认 false —— 默认开等于升级即静默改行为）。
- `health.Snapshot`（`internal/health/snapshot.go`）：`<渠道>/<模型>` → 最近一次结论的索引，
  带缓存、`Add` 后失效。**每个模型各自最近**，不是「最近一个批次」——
  抽样体检只覆盖少数模型，按批次取会把上一轮全量体检的结论一起丢掉。
  `Snapshot.Hide()` 对未体检的返回 false，上面那条语义就落在这里。
- `health.Health(history, enabled)` 把「体检结论」与「设置开关」合成一个查询器，
  装配层（`cmd/poolgate`）构造**一次**，面板与网关拿的是同一个函数 →
  面板标着「已按健康度隐藏」的模型，客户端那边一定也拿不到。
- 网关 `Options.Health` 在 `/v1/models` 里过滤（请求内取一次索引）；
  面板 `handleModelCatalog` 不改列表，只给每行加 `hidden_by_health` 标记 ——
  模型页必须看得见被隐藏的模型，否则用户只会觉得「模型凭空少了」。

**面板的两处「前后不一致」是这次实测揪出来的**：

1. 开关保存后模型页还显示旧结论（两个页面各说一套）。修法：**在 `saveSettings` 确认保存之后**
   比对开关是否真的变了，变了才重拉一次目录。第一版挂在字段的 `change` 上，
   早于保存触发 → 服务端那时读到的还是旧值，拉回来的仍是过期数据。
2. 跑完体检后模型页的「健康」列与隐藏标注都是旧结论 → `probe()` 之后一并 `loadModels()`。

另外把模型页副标题里那句含糊的「N 个因健康度剔除」拆成
「N 个人工剔除 · M 个按健康度隐藏」（两者来源与恢复方法都不同，混着说就是把人工操作说成探测结论），
列名「下发」改成「人工下发」，避免与「已按健康度隐藏」自相矛盾。

### 0.3.7 — 改密码 / 凭证续期 / 401 不再把人踢出去

用户实测反馈三条（缺改密码、千问登录后取不到积分且被踢回登录页、千问对话报 HTTP 401），
查下来是**一个问题链**加两个独立缺口：

**① 上游 401 被当成了「管理员会话过期」（这是「被踢回登录页」的真凶）**

`statusForKind` 把 `SessionDead`/`AuthFailed` 映射成 **HTTP 401**，而前端 `api()` 把所有 401
当成「我被登出了」→ 立刻清状态跳登录页。于是用户点一下「刷新余额」（上游恰好 401）
就被踢出面板 —— 而管理员会话其实好好的。修法：**401 只属于 `withAuth`**，
上游凭证问题统一 502；错误消息补一句「该渠道账号的凭证已失效：到「账号」页点它的
「重新登录」重新授权」。网关侧同理（Studio / Claude Code 看到 401 会以为自己的 API Key 无效，
而坏的是池里的账号）。

**② 凭证从不续期（千问办公「全是 401」的根因）**

`qwenwork.Refresh` 原来写的是「网页域没有公开的刷新端点，如实返回原凭证」——
**实测是错的**：`.well-known/openid-configuration` 的 `grant_types_supported` 明确带
`refresh_token`，垃圾 refresh token 会得到标准 `invalid_grant`。而池子里存的永远是
签发那一刻的凭证，没有任何地方会去换新的，于是 token 一到期这个渠道就彻底废掉
（每个请求 401 → 账号被判「会话失效」禁用 → 客户端看到「无可用账号」或「上游返回 HTTP 401」）。
本次补上：

- `qwenwork.Refresh`：`grant_type=refresh_token` 兑换，**轮换后的 refresh token 必须落盘**
  （上游会轮换，只换内存 = 重启后拿到已作废的旧值）；被拒时报 `SessionDead` +
  「refresh_token 已失效，需要重新授权」+ 上游原话。
- `pool.SetRefresher` + `Fresh`/`RefreshNow`（`internal/pool/refresh.go`）：临期（15 分钟内）
  自动续期；401 时强制续期一次再重试；同账号串行化、失败后 30 秒冷却（否则一个坏号的
  每个请求都会去敲上游 token 端点）。
- `Pick`/`PickPreferred` 改成带 `ctx`：**编译器强制**每个选号点都考虑续期，
  不靠「记得调用」。
- 装配层（`cmd/poolgate`）负责「换 token → 落盘 → 回写池子 → 若原因是会话失效则自动恢复启用」
  四步；人工「停用」不会被自动续期推翻。

**③ 面板内改密码（原本只有命令行一条路）**

`POST /api/admin/password`：验当前密码 → 换新（Argon2id）→ **注销其它设备的会话**
（当前浏览器不断），失败计入与登录共用的失败计数（否则这里就是一条不限速的爆破入口）。
忘记密码仍然只能走 `poolgate admin reset-password` —— 面板里做「忘记也能改」
等于把唯一的门钥匙挂在门上。

顺带：账号行的禁用原因从 `SessionDead` 这种枚举值改成「会话失效 —— 点『重新登录』重新授权」。

产物 `poolgate-0.3.7.fpk`；`check.sh` 全绿（新增 pool 续期 8 例、
路由续期/换号 3 例、改密码 5 例、401 语义 2 例、千问续期 3 例）。

### 0.3.8 — 登录失败要说人话

用户反馈里那句「你怕不是改错了，改成登陆的了」逼出来的一个真问题：登录失败时面板显示的是
**「未登录」**，还有一行 `POST /api/session → 401 Unauthorized · kind=AuthFailed`。

两处都是我们自己的锅：

1. `api()` 把所有 401 一律当成「会话过期」，先抛出写死的「未登录」，
   **把服务端真正的原因吞掉了** —— 密码错的真实提示是「密码错误，还可尝试 4 次」。
   现在只有**非登录接口**的 401 才代表被登出；`/api/session`(POST) 与 `/api/setup`
   的 401 属于业务失败，原样把 message 交给登录页。
2. 那行请求明细是**写死的假字符串**（原型里的示例），任何失败都显示同一个
   `kind=AuthFailed`。现在从真实响应里取 method/path/status/kind，没有就不显示。

顺带把改密码卡片的强度提示与首次设置收敛到同一份判据（`strengthOf`），
免得两个页面各说各话。

另外两个「点千问没反应」的真问题（同一轮实测揪出）：

3. **关掉授权对话框，服务端那次授权还在跑**。授权会话有 5 分钟有效期、全局只允许一次；
   面板的「取消」只关覆盖层、不调 `/api/login/cancel`，于是用户再点「授权」得到的是
   「已有渠道授权在进行中，请先在浏览器完成或点取消」—— 而那个「取消授权」按钮
   当时挂在 `v-if="auth.url"` 下，失败过（或刷新过页面）时根本没有这个按钮，
   人就被卡满 5 分钟。现在关覆盖层会顺手取消服务端会话，取消按钮也**不设条件**显示。
4. **本机回调在远端浏览器下不可能完成**（TraeWork / 千问办公）。授权码送回
   `127.0.0.1:<随机端口>`，只有浏览器与 PoolGate 同机时才收得到；从 PC 打开 NAS 上的面板时，
   授权页跳回的是**那台 PC**，面板一直等到超时（表现就是「点了授权没反应」）。
   面板现在会在这种情况下给出明确警告（并说明「请在 NAS 上的浏览器里打开授权链接」）。
   `authMethod` 也补全了六个渠道 —— 原先 qwenwork 落到兜底文案，正好把
   「本机回调」这个关键事实藏掉了。

### 0.3.9 — 千问办公：换成 device_token（用户实测「重新登录了还是一样」的正解）

用户第三次反馈千问，附上了确切报文：

```
✗ SessionDead: 上游返回 HTTP 401 (upstream: {"code":"invalid-credential","msg":"Invalid JWT token"})
我重新登录了千问，还是一样报错，你需要看看之前的代码是怎么写的，抄过来
```

**查清了**：`/vol6/@appconf/poolgate/creds/qwenwork-*.json` 的 mtime 说明他们的
「重新登录」**确实写进了新凭证**（所以不是回调没回来），问题是**换出来的 token 网页域不认**。

wild-work 的代码里早就写着这件事（`internal/app/app.go`）：

> 若 Poll 兑换的首个 token 无效（实测 OAuth 兑换 token 调 /user/info 会 401 invalid-credential），
> refreshIfSessionDead 会自动换新 token

它的 `qwenwork.RefreshToken` 也不是去 `/oauth2/token`，而是：

```
POST https://gateway.qwenwork.cn/api/v1/deviceToken/refresh
     {"refresh_token": "<OAuth 的 refresh_token>", "target": "c"}
  → {token, device_token, refresh_token, expires_in(ms)}
  → a.AccessToken = device_token（或 token），a.RefreshToken = 轮换后的
```

**只有 device_token 才是网页域 JWT 校验认的凭证。** 我们之前把 OAuth 的 access token
直接当 cookie 用，于是每次授权拿到的都是那个不认的 token —— 这也是 0.3.7 里我那版
「用 /oauth2/token 刷新」治不好的原因（换回来的还是 OAuth token）。

改动（把 wild-work 的做法抄过来）：

- `Adapter.fetchDeviceToken`：打桌面网关的 deviceToken 端点，校验「一对 token 必须完整」
  （只换回 access 不换 refresh 就直接报错，半份 token 比没有更糟）；
  401/403 → `SessionDead` + 「refresh_token 已失效，需要重新授权」+ 上游原话。
- `Adapter.Refresh` 改走它（`ExpiresAt` 按 `expires_in` 毫秒换算，缺省保守 24h）。
- **登录路径也走它**：授权码兑换出 access token 后立刻换 device_token，
  换不到就**让授权失败**（宁可报错，也不把「网页域不认的凭证」塞进池子 ——
  那正是「面板显示授权成功、之后每次都 401」的成因）。
- device_token 里才带 `username`（OAuth 那个没有）→ 昵称空着就用它补，面板不再显示 hex uid。

实测：deviceToken 端点仍可用（垃圾 refresh_token → `401 {"errorCode":"INVALID_REFRESH_TOKEN",...}`），
503 闸门只影响推理端点。协议说明同步写进了 `docs/03-渠道能力矩阵.md` 与适配器包头注释。

### 0.4.1 — 能扫码就别抢着跳浏览器

用户实测反馈：「如果能正常生成二维码，则不要自动跳转登录页面，像千问这种不能自动生成的，再跳转页面。」

原来 `startAuth` 拿到授权地址后**一律** `window.open` —— 于是即使用户准备扫码，桌面也会被弹一个标签页。
现在改成：**先把地址写进面板、等 `authQr` 算出来**，出了码就**不**打开浏览器（用户拿手机扫），
只有「这个渠道出不了码」（千问的 `redirect_uri` 被上游锁死在本机）或「码没生成出来」（库没加载/地址过长）
才自动打开授权页。副标题与「打开授权页」那句兜底说明也按有没有码分成两套文案。

实测（playwright 数弹窗）：QoderCN / TraeWork → 有码、**不弹**；千问办公 → 无码、**弹**。

### 0.4.0 — 授权不跳转：面板内真二维码 + TraeWork 局域网回调 + 手工回填

用户诉求：「能不能不跳转登录，直接嵌进我们的软件里，能扫码扫码、能验证码验证码」。
调研结论见 [`docs/04-登录方式调研.md`](../docs/04-登录方式调研.md)：**iframe 嵌上游登录页这条路堵死**
（QoderCN 实测 `x-frame-options: SAMEORIGIN` + CSP `frame-ancestors 'none'`；其余三家虽没禁止头，
但第三方 Cookie/Storage 分区 + 风控 SDK 会让登录态拿不回来），**「服务端复现扫码/验证码接口」也不做**
（腾讯 OneID / 字节 sdk-glue / 阿里 cloudauth 都在页面上，逆风控不划算）。能做的是把授权流搬进面板：

1. **面板内真二维码**（`internal/webui/dist/qrcode.js`，MIT vendored；`VITE` 无关，直接 `<script>` 引入）。
   授权页现在渲染的是**能扫的真码**（内容就是这次授权的地址），手机扫完在手机上完成登录，
   桌面零跳转。四家（QoderCN/QoderCOM/WorkBuddyCN/WorkBuddyAI）+ TraeWork 都适用。
   之前的「二维码」是原型里的**装饰性假码**，删了。
   细节：QR 的 SVG 加 `shape-rendering:crispEdges`（抗锯齿会把模块边缘弄灰，扫码器解不出来）；
   底色固定白（不跟随暗色主题）——那是扫码的功能要求，不是漏改主题。
2. **TraeWork 回调改用面板地址**：`channel.LoginOptions.CallbackBase` 由控制台按面板自己的
   对外地址（`panelBase`）算出来，TraeWork 用它替换写死的 `127.0.0.1` 并改绑 `0.0.0.0`
   （一次性回调、5 分钟自关、凭证仍要过 PKCE 校验）。**「必须浏览器与 PoolGate 同机」的限制没了** ——
   实测扫码内容里的 `auth_callback_url` 就是 `http://192.0.2.10:<port>/authorize`。
3. **手工回填回调**（`channel.CallbackAcceptor` + `POST /api/login/callback`）：千问办公的
   `redirect_uri` 被上游锁死在预注册的 `127.0.0.1:<port>`（实测传局域网地址直接 `invalid_request`），
   扫码对它无效 —— 于是给它（以及 TraeWork）一条兜底：浏览器跳回本机打不开时，把地址栏那串
   粘回面板，服务端拿里面的 code 走原来的 PKCE 兑换。**不改回调地址、不额外开放端口**。

顺带：`tools/audit-ui.mjs` 现在也过一遍**授权覆盖层**（桌面 + 手机）——那是用户真会看到的一屏，
而且塞了长 URL + 二维码，最容易撑破窄屏（这次实测偶发过 scrollWidth 1500+，已用
`min-width:0`/`max-width:100%`/`overflow-wrap:anywhere` 逐层钉死）。

## 面板渠道授权（M2）

面板「渠道授权」页 → 点渠道的「授权」→ 浏览器打开授权页 → 面板自动轮询到成功。
成功即落盘 `<conf>/creds/<渠道>-<uid>.json` 并立即入池。

```sh
# 也可以用接口驱动（需先登录拿会话 Cookie）
curl -b cookie.txt -X POST 127.0.0.1:5014/api/login/start -d '{"channel":"qodercn"}'
curl -b cookie.txt 127.0.0.1:5014/api/login/poll     # pending → ok / 结构化错误
curl -b cookie.txt -X POST 127.0.0.1:5014/api/login/cancel
```

同一时间只允许一次授权；5 分钟未完成会明确报「授权超时」并释放本机回调端口。
不支持面板授权的渠道（已暂停那三家）会直说「请手动导入凭证」，不给点了没反应的按钮。

## 网关用法

```sh
./build/poolgate -addr 0.0.0.0:5014 -conf /tmp/pg/conf -data /tmp/pg/data
# 未显式指定 -addr 时，监听地址/端口取设置里的值（面板「服务」可改，重启生效）；
# 显式指定则以启动参数为准（安装包的启动脚本就是这样），面板会写明这一点。
# API Key 首次运行自动生成在 <data>/config.json（0600）；
# 面板「设置 → 鉴权」可直接显示明文/复制/重新生成（GET /api/apikey、POST /api/apikey/{reveal,rotate}）
curl -H "Authorization: Bearer <key>" 127.0.0.1:5014/v1/models
curl -H "Authorization: Bearer <key>" -X POST 127.0.0.1:5014/v1/chat/completions \
  -d '{"model":"qodercn/auto","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

模型名带渠道前缀 `<渠道>/<模型>`（`qodercn/` `workbuddy/` `traework/`）；无前缀默认 QoderCN。账号凭证放
`<conf>/creds/qodercn-<uid>.json`（嵌套/扁平格式，可迁移自 wild-work 旧 auth 目录）。

## 测试

```sh
./check.sh    # 含 -race
```

测试用例直接钉住三条红线，不是普通单测：

- 空流/信封错误必须判失败：`internal/gateway/gateway_test.go`、三个适配器的 stream 测试；
- `UpstreamFault` 不得计入账号错误、失败必带原因：`internal/pool`、`internal/router`、`internal/errs`；
- 暂停渠道不得下发模型：`internal/registry`、`internal/console`；
- 授权失败必须有原因、不落盘半份凭证：`internal/console/login_test.go`、各适配器 `login_test.go`。

改判据前先看 `internal/health/health_test.go` 与 `internal/errs/errs_test.go`。
