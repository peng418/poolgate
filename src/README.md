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

### 0.6.4 — 面板只留「能用的那几个」（接入源 + 聊天模型都先屏蔽，代码不删）

用户裁定：接入源（API Key 式来源）他后面还要改；豆包 / ChatGPT / 元宝 / Kimi / 智谱清言 / 通义 /
DeepSeek 网页 / Gemini / iFlow / Antigravity 这些「聊天模型」他现在不用。做法沿用**最小改动、一行恢复**：

- `internal/webui/dist/index.html` 里两个开关：
  - `const SHOW_PROVIDERS=false;` —— 导航项被 `.filter()` 摘掉，视图写成
    `v-if="SHOW_PROVIDERS && view==='providers'"`；
  - `const VISIBLE_CHANNELS=['qodercn','qodercom','traework','workbuddy','workbuddyai','qwenwork'];`
    —— 白名单之外一律不显示。应用到：总览的渠道健康表与「可用渠道 N/6」计数、
    账号页的 `accountSections`、「添加账号」的 `pickChannels`、模型与费率页的 `filteredModels`、
    日志/诊断的渠道下拉（`visChannels`）、设置页的签到能力表。
- **两个标识符都必须加进 `setup()` 的返回列表**：生产 Vue 对「模板里用了、setup 没透出」的标识符
  是**静默**的（渲染成 undefined，v-if 永远不成立），由 `tools/audit-vue.mjs`（dev 构建）兜住。
- 刻意**没有**动的地方：后端 `/api/providers*`、`internal/adapter/openaiup`、17 个预设、
  各渠道适配器与凭证、渠道 `Status`。「设置 → 渠道开关」表仍列**全部**渠道 —— 那是管理面，
  以后暂停/恢复渠道仍在这里，也是把聊天渠道放回来的入口。
- **边界（写进 README 与面板注释）**：这次只改显示，**网关行为没变**，被隐藏渠道的模型仍会下发；
  要让客户端也选不到，得在「设置 → 渠道开关」把那些渠道暂停 —— 那是运行状态，与这里的显示独立。
  把这一点写清楚，是因为「面板看不见但接口还在发」正是本项目红线一要防的那种不一致。
- `tools/audit-ui.mjs` / `tools/audit-vue.mjs` 改成「导航里没有这个入口就跳过并打印」：
  审计跟着面板入口走，恢复入口后那一屏自动重新被覆盖。

验证：`check.sh` 全绿；双端审计 0 问题；Vue 告警审计 0 条 0 报错；截图核对总览渠道表只剩白名单 6 个
（豆包 / ChatGPT / 元宝 / Kimi / 智谱 都不出现在总览、账号、模型三页）。

### 0.6.3 — 三个网页渠道的真 bug（ChatGPT / DeepSeek / 豆包）

用户实测：三个新接入的渠道在 Studio 里「gpt 报错、豆包读不到上下文/记忆、deepseek 回一堆乱码」。
三种病根完全不同，逐条修掉并留了回归用例：

**一、ChatGPT 网页渠道（一律 502「第 3 道门」）—— 三个 bug 叠在一起**
- **turnstile 的密钥传错了**：`dx` 是「与本次请求发出去的 requirements token（`p`）逐码点异或后 base64」
  的 opcode 程序，旧代码传空串（注释还写着「参考实现如此」，但 gpt4free `process_turnstile_new` 与
  ChatGPT2API-GO `solveTurnstileToken` 传的都是 `p`）。空串解出 22K 乱码 → 程序永远跑不出结果，
  而错误文案把「密钥用错」谎报成「解释器覆盖不到上游指令集」。现网挑战核对：换 `p` 立刻得到合法的
  88 条指令程序，597 步跑出 token。
- **寄存器必须按 JS 属性名存**（`map[string]any`）：上游把 opcode 重绑到**随机小数槽位**做混淆
  （实测程序里有 `[84.18, 9.23, 7]`），旧的 `map[int]any` 把 `9.23` 截断成 `9` —— 正好砸中程序队列
  寄存器，队列被换成别的值、程序当场停摆。参考实现是 Python dict（原始键）与 Go 的 `map[string]any`
  （`ChatGPT2API-GO/internal/app/turnstile.go:15`），我们那一版移植成了 int 键。
- **上游换了 SSE 帧形态**：现在把消息**直接摊在顶层**（`{"message":{author:assistant,content:{parts:[…]}}}`，
  没有外层 `v`），旧 `walk` 见不到 `v` 就 `return` → 三道门都过、上游正常出字、客户端拿到**空回复**。
  定位手法：临时给适配器加 `POOLGATE_DEBUG_SSE=<file>` 的 tee 抓原始帧（用完即删）。
- 错误文案改成说真话：`solveTurnstileToken` 现在返回区分三种原因的错误（解不开 / 程序显式拒绝 /
  跑完没产出），不再用一句「解释器覆盖 1–35 号指令」把密钥错误盖过去。

**二、工具调用模拟层（toolshim）两处改进 —— 对所有 shim 渠道生效**
- **格式提醒挪到对话末尾**：把工具说明只写在系统提示词里时，豆包 4/4 次都用自然语言回答、一个工具
  都不调；同一份请求只在最后一条消息末尾补一句格式提醒 → doubao-pro 4/4、doubao 2/3 正确调用。
  系统提示词一长，那里的约定就被淹没，紧挨生成位置的那句话才起作用（两份都留）。
- **认模型自己的原生语法**：请求一大，DeepSeek 会改用它的 DSML（`｜｜DSML｜｜ invoke name="…"`），
  旧的解析层只认 `<tool_call>` → 整段标记当正文泄漏。现在 DSML 与 `<function_calls><invoke>` 两家族
  共用一套家族无关的抽取器，另认复数 `<tool_calls>` 外壳（含外壳里嵌多个单数调用）。
  抽不出来照样把原文（连标记）交出去 —— 红线一。

**三、网关：把「有帧、没内容」判成失败**
上游换了帧形态而适配器没认出来时，流里照样有帧、照样正常结束，以前会回一个 `content:""` + `stop`
的 200（客户端以为模型没话说）。现在流式与非流式都判失败并说明原因。顺带修掉 TTFT：原先只有
「第一个 chunk 带内容」才记首字，而上游普遍先发一个 role 空帧 → TTFT 一直记成 0。

**验证**：`check.sh` 全绿；新增回归用例（小数槽位、错密钥的错误文本、顶层消息帧、DSML 分片、
复数标记、空帧判失败、末尾提醒落位）。真上游实测：ChatGPT 流式/非流式真出字，豆包 30 工具请求
返回 `tool_calls`，deepseek 用抓包到的原始 DSML 验证解析。
**已知边界**：ChatGPT 网页模型**明确拒绝**文本协议的工具调用（原话大意「你列出的 Read/Bash/Grep
并没有挂载到我的可调用工具列表」），三种措辞实测 0/4 —— 该渠道可作聊天后端，作 coding agent 后端
需要它的原生工具通道（或 OAuth/Codex 路径）。

### 0.6.2 — 每个渠道 / 来源都带自己的品牌图标

22 个渠道 + 17 个接入源预设，各配一张**平台自己的图标**（favicon / 站点 logo），落在
`internal/webui/dist/brands/`；来源 URL 与商标归属逐条记在 `brands/SOURCES.md`。
实现上有几条刻意的选择：

- **有就显示、没有就退回**：模板里是 `v-if="icoOf(kind)"` + `v-else` 保留原来的身份色方块。
  删掉任何一个图标文件都不会让页面报错或留白 —— 这条对「某个平台方要求撤下图标」是必需的。
- **图标垫在一块固定浅色底板上**（`.ch .logo`，20×20 + 2px padding + 白底 + 细边）。
  原因有二：不少品牌标是深色透明底（Kimi、xAI、OpenRouter、Perplexity…），直接放暗色主题上等于看不见；
  各家留白差异极大，垫同一种底板，一列图标才像一套。
- **路径必须是 `./brands/…` 相对形式**：飞牛应用网关会剥掉 `/app/poolgate` 前缀，
  根绝对路径在网关下 404（同 `src/README` 顶部那条「前端资源前缀铁律」）。
- **同一家的多个来源共用一份文件**：QoderCOM 用 `qodercn.svg`、WorkBuddyCN 用 `codebuddy.svg`、
  DeepSeek 官方 API 用 `deepseek.png` —— 表在 JS 里（`BRANDS`），文件不重复放。

顺带按同一思路把「添加账号」的渠道选择页也分了模块：只列登录式渠道，API Key 式来源不再混在里面
（它们本来就是"点了没反应"的卡片，面板授权对 key 式来源不适用）。

**两个只有跑审计才能发现的坑**（都实测踩到）：
1. 类名撞车：先取的名字 `brand` 在样式表里**已被侧栏 logo 占用**（`.brand{display:flex;padding:4px 8px 18px}`），
   用在 `<img>` 上会把图标撑变形。改名 `.ch .logo`（作用域限定在 `.ch` 下）。
2. `:alt=""` 会被 Vue 当成**绑定表达式**去实例上找 `alt` 属性，`audit-vue` 报
   `Property "alt" was accessed during render but is not defined on instance`。装饰性图片用**静态** `alt=""`。

验证：`audit-ui`（16 视图 × 双端）0 问题、`audit-vue` 0 告警；playwright 实跑确认
总览 23 / 账号 22 / 设置 46 / 选择页 23 个图标**全部真实加载**（`naturalWidth>0`）、0 个 404。

### 0.6.1 — 「接入源」与「账号」两个模块彻底分开

用户 2026-09-26 的裁定：**接入源 = 只放「去官网注册拿 key 就能用」的来源；账号 = 我们本来就支持调用的那些渠道**
（QoderCN / 千问办公 / 豆包 …）。以前接入源页把两类**混排在同一张表**里（靠「类型」列 + 左侧色块区分），
「添加接入源」向导第一步还摆着「登录授权式」这张卡 —— 点它只是把你送去账号页，那一步的存在本身就在
暗示「接入源里也有登录式渠道」。现在：

- 接入源页的表格删掉登录式渠道行与「类型」列，只列 `providers`（API Key 式）；空态文案改成引导去加来源。
- 向导从三步变两步（去掉「选类型」），填参数 → 连通性测试后保存；`providerChannelRows` 这个 computed 连根删掉。
- 页面副标题、banner、「两类来源的区别」表全部改写：不再描述"混排"，改成两个模块的分工表 + 「去『账号』页」入口。

**顺带修掉向导里一个会静默改行为的 bug**（HEAD 里就在）：`resetProviderForm` 与 `editProvider` 组装的 `cfg`
都漏了 `tools_mode` —— ①新建时「工具调用能力」下拉是**空白**的（看不出当前是哪一档）；
②更糟的是编辑一个「工具调用靠网关模拟（shim）」的来源：它的 `supports_tools` 本就是 false，
保存时 `tools_mode` 空 → 后端按 `supports_tools` 推导 → **被静默降级成「不用工具（none）」**。
现在两处都回填 `tools_mode`，`applyPreset` 也让两档同步。
验证：playwright 真开一遍向导（新建显示 `native`、编辑 shim 显示 `shim`）+ 直接保存后读 `providers.json`
确认 `tools_mode` 仍是 `shim`；`audit-ui`（16 个视图 × 双端）0 问题、`audit-vue` 0 告警。

### 0.6.0 — 逐渠道与开源参考实现核对（真上游实测驱动）

**做法**：把 22 个渠道**逐个**与公开开源参考实现（以及上一代 wild-work 实现）逐字段核对 ——
端点 / URL query / 请求头（含伪装版本号）/ 请求体每个字段的名字与形态 / 流帧解析 / 结束条件 /
错误归一 / 模型表。每处改动都在代码注释里写明依据（参考实现文件名 + 行号）。
核对前先确认参考实现的时效性（逐个与上游 HEAD 比对），并从 GitHub 扫到更近的实现做交叉验证
（`AIClient2API` 8.8k★、新版 `doubao2api`、`codebuddy2api`、`Qoder-2API-Go`、`Orchids-2api`、
`workbuddy-openai-proxy`、`BYOKEY` 等）；其中几家独立实现互相印证时，才把结论写进代码。

**真实凭证实测（现网账号，2026-09-26）**：`doubao` / `qodercn` / `workbuddy` / `qwenwork`
四家真出字（含原生工具调用），`traework`（`code=4008` 额度耗尽）与 `workbuddyai`
（`14018 Credits exhausted`）是账号额度问题，错误如实带上游原话。

**核对出来的真问题（按影响排序）**：

1. **请求体字段形态不对 → 整条渠道不可用**
   - 豆包：`local_conversation_id` / `local_message_id` / `block_id` 是空串、缺
     `is_finish` / `patch_type` / `icon_url` → 上游 `710020202 common invalid param`。
     补齐后同一条 Cookie **真出字**（流式 / 深度思考 / 工具调用三条路径都验过）。
   - Gemini（Code Assist）：外层信封多发 `user_prompt_id`、内层多发 `session_id` ——
     三份参考实现都没有这两个字段，Google 对未知字段直接 400（`Cannot find field`）。
2. **「移植丢失」：wild-work 里有、重做时整块没搬过来的三道防线**
   - **`internal/sanitize`（新补）**：上游对请求体里 Claude Code / Codex CLI 的模板句做
     **逐字精确黑名单匹配**，命中回 `HTTP 400 code=11128 "Illegal API invocation from an
     unapproved channel"` —— 即**从 Claude Code / Studio 调 CodeBuddy / WorkBuddy 会被挡掉**。
     **真上游 A/B**：脱敏前 `ContentBlocked / 11128`，脱敏后正常出字「你好」。
     接到 `workbuddy` / `workbuddyai` / `codebuddy` 三处请求体组装。
   - **孤儿 `tool_call` ↔ `tool` 结果配对清理**：工具执行失败时客户端常把 tool_calls 存进历史
     却写不回结果，坏历史每次重放都 400 → **整条会话报废**。网关发请求前剔除无法配对的条目。
   - **工具调用残缺参数检测**：流被截断时 `arguments` 只剩半截 JSON，原样给客户端会卡死会话。
3. **静默丢字**：ChatGPT 网页版的 patch 流里有「只有 `v`、没有 `p`/`o`」的省略路径增量帧，
   三份参考实现都专门处理，我们原来直接忽略 → 正文缺词断句且日志无痕。
4. **上游原话被吞**：适配器流内错误抛的是普通 error，被网关换成通用的「上游请求失败」
   （TraeWork 额度耗尽就是这么变成「解析失败」的）。现在流内错误一律归一成
   `errs.Error`（Kind + 上游原话），网关对非结构化错误也会把原话带出去。
5. **端点 / 域名打错**：QoderCOM 的模型目录打到了推理网关（COM 是双域名：模型表 api2 / 推理 api1）；
   Gemini 的开通轮询走了一个没有任何参考实现用过的 `GET /v1/internal/{name}`（三份参考都是重发 `onboardUser`）。
6. **等待上游期间连接静默**：思考型档位出字前可能静默几十秒，会被中间的 nginx / 飞牛网关按空闲
   超时掐断 → 网关每 15 秒发一次 SSE 保活注释帧（`: keep-alive`，按 SSE 规范客户端会忽略）。
7. **Windsurf 改走原生工具调用**：参考实现已用**付费实弹**标定 ToolDef 的内部 tag
   （外层 #10、`name=1 / description=2 / parameters=3`，2026-07-04 opus-4-8 实弹确认）。

**仍未真上游验证**：13 家登录式渠道里只有豆包在本机有可用凭证并完成了真上游验证，其余仍需
用户粘一次凭证确认；第一次失败时错误信息会带上游原话。

### 0.5.0 — 「全部走登录式」：Kimi / 智谱清言 / 豆包 / 腾讯元宝

按用户裁定：**不要 API Key 式接入，所有来源都用登录实现**（0.4.3 的「接入源」保持可用，
但不再往里加适配器）。这一版把国内几个主流「网页版」平台接成渠道。它们的共同前提是：
凭证来自**你自己浏览器里的登录态**，粘一次即可；服务端不与登录接口打交道 ——
**不存密码、不在 NAS 上跑无头浏览器**。

| 渠道 | 用户粘什么 / 怎么授权 | 上游协议 | 工具调用 |
|---|---|---|---|
| `kimi` | refresh token（推荐）或 access token | **Connect（gRPC-Web）**：5 字节信封 + JSON 帧流 | toolshim 模拟 |
| `chatglm` | cookie `chatglm_refresh_token` | SSE；**每请求带自算签名**（时间戳变换 + md5） | toolshim 模拟 |
| `doubao` | 整行 Cookie（含 `sessionid`） | 带事件名的 SSE；思考靠 `block_type=10040` 开关块 | toolshim 模拟 |
| `yuanbao` | 请求头里的 `x-uskey`（整段头也行） | SSE；按 `type=think` / `type=text` 分流 | toolshim 模拟 |
| `chatgpt` | `accessToken`（浏览器里搜 `"accessToken":"…"`） | sentinel 挑战 + 自算 PoW → `/backend-api/conversation` 的 patch 流 | toolshim 模拟 |
| `anthropic` | **OAuth 授权码**（Claude 订阅登录，浏览器授权后粘回 code） | Messages API（原生协议） | 原生 |
| `codebuddy` | 浏览器授权（与 WorkBuddy 同一套 state 轮询） | OpenAI 兼容 SSE（后端只接受流式） | 原生 |
| `copilot` | **GitHub 设备码**（浏览器打开验证页输码） | 标准 OpenAI 协议（需伪装 VS Code 插件头） | 原生 |
| `kiro` | 刷新令牌（桌面版；企业版三件套） | **AWS Event Stream 二进制帧**（不是 SSE） | 原生 |
| `iflow` | `~/.iflow/settings.json` 里的 apiKey | OpenAI 兼容 SSE + **每请求 HMAC-SHA256 签名** | 原生 |
| `lingma` | IDE 登录缓存（`cache/user` + `cache/id`） | 双层嵌套 SSE | 原生 |
| `antigravity` | **OAuth 授权码**（Google 账号） | `v1internal:streamGenerateContent`（外层包 `response`） | 原生 |
| `windsurf` | Windsurf/Devin 客户端的 session token | protobuf / Connect 流式帧（只走直连云那条路） | toolshim 模拟 |

四条实现原则（每条都对应一个真会踩到的坑）：

1. **凭证能当场验就当场验**。Kimi（换令牌）、智谱（换令牌）有轻量接口，登录时就验一次，
   把「已失效的凭证」挡在池子外面；豆包与元宝**没有**这类接口，只做格式检查 ——
   这个区别写在面板的粘贴引导语里，用户不会以为是登录流程坏了。
2. **按上游给的字段分流思考**。四家四种形态（Kimi 的 `block.think`、智谱的 item `type=think`、
   豆包的 `10040` 开关块、元宝的 `type=think`）。取错字段**不会报错**，只会静默地把思考混进正文 ——
   所以每个渠道都有针对分流的测试。
3. **二进制帧必须按帧切、并容忍半帧**。Kimi 的 Connect 帧按行读会得到乱码；而且切帧时
   **必须先处理已切出的帧再压缩缓冲区**（帧的载荷是缓冲区的子切片，顺序反了会被覆盖）——
   两者都有测试钉住（逐字节喂入）。
4. **不伪造看不懂的签名**。豆包的 `a_bogus` 是它自家 fetch hook 在浏览器里现算的，
   纯 HTTP 路径不带它 —— 我们**不去猜**这个算法，而是在 `Spec.Docs` 与错误信息里
   说清楚「可能被风控要求人工验证码」，并把风控码（710022002 / 710022004）归一成
   「该去做什么」而不是「上游故障」。

顺带修掉一个静默缺陷：**「接入源」的名字可以与内置渠道重名，挂载时会静默顶掉内置渠道**
（来源名同时就是 channel.Kind，而同 Kind 是覆盖语义）。现在内置渠道名一律登记为保留名
（校验时拒绝保存、挂载时跳过并记日志、面板上标出冲突），并有回归测试钉住。
`deepseek` / `glm` 两个预设也相应改名为 `deepseek-api` / 保持 `glm`，
避免用户新建来源时撞上内置渠道。

**验证**：每个渠道都有 mock 上游端到端测试（请求形态、增量解析、思考分流、错误归一）；
其中三种**二进制帧**协议（Kimi 的 Connect、Kiro 的 AWS Event Stream、Windsurf 的 protobuf）
额外测了逐字节喂入的半帧切分，会自算签名的两个（智谱、iFlow）在测试里按同一算法复算比对，
ChatGPT 的 PoW 同样复算比对。`internal/boot` 新增回归测试钉住「能力位说实话」
与「同名来源不能顶掉内置渠道」；面板实测 22 个渠道全部注册、粘贴引导语按渠道正确下发
（`/api/login/start` 返回的 `paste_hint` 就是面板上显示的那段）；`check.sh`（gofmt + vet + race + build）
与 `audit-ui`（桌面 + 手机）/ `audit-vue` 全绿。

修掉的两个面板缺陷（都是 UI 审计抓出来的）：① 粘贴登录的说明文案曾**硬编码成 DeepSeek 的
「userToken」说法**，服务端下发的渠道专属引导语只被当作布尔开关用、没渲染出来 —— 现在文案跟着渠道走
（Kimi 说 refresh token、豆包说整行 Cookie、通义灵码说 IDE 缓存文件）；② 授权地址很长时
（Anthropic 的授权 URL 有 700+ 字符）会把授权覆盖层撑破，现在长串可断行。

**没有验证的部分**：本机网络到不了这些上游、也没有可用的真实账号，所以**协议是按公开参考实现
移植 + mock 验证的**（哪一份参考实现见 `docs/03` §7 的表）；真上游的首次验收需要你用自己的账号走一次
（第一次对话若失败，错误信息里会带上上游原话）。

### 0.4.9 — DeepSeek（网页版）渠道：粘贴 userToken 登录 + 服务端解 PoW（工具调用走 toolshim）

网页协议没有原生 tools，所以这是**第一个靠 toolshim 模拟工具调用**的新渠道（0.4.4 的模拟层第一次接真渠道）。

| 环节 | 做法 |
|---|---|
| 登录 | **粘贴 userToken**：用户在**自己浏览器**登录 DeepSeek，控制台执行 `JSON.parse(localStorage.getItem("userToken")).value` 把结果整段粘回面板（不存密码、不碰登录接口）。不能做扫码的原因：登录接口的 `device_id` 必须来自真实浏览器里的数美 SDK 指纹，服务端复现不了（空值/随机值被判 `RISK_DEVICE_DETECTED`） |
| 设备 id | 由 userToken **派生**（FNV-1a 双哈希 → RFC4122 v4 UUID）：同账号重登稳定不变、不同账号天然不同（共用会被判异常） |
| PoW | 每次 completion 前现场解挑战：跑**官方那份 WASM**（wazero 纯 Go，不注册任何 import；分配器按签名探测，不写死导出名），`X-Ds-Pow-Response` = base64(6 字段 JSON) |
| 对话 | `chat.deepseek.com/api/v0` 的 agent 协议：建会话 → 解 PoW → SSE **p/o/v patch 流**（路径与操作跨帧持久，文本在 `response/fragments[-1].content` 上 APPEND 累积；fragments[].type 分 THINK/RESPONSE） |
| 工具调用 | **toolshim 模拟**：`Spec.Tools=false + ToolsShim=true`，网关把 tools 翻成提示词、把输出标记解析回结构化 `tool_calls`（对客户端合同不变，面板如实标注「工具调用靠网关模拟」） |
| 错误 | 上游大量错误是 **HTTP 200 + 信封码**：`biz_code` 5/10/11（禁言/封禁/设备指纹）与信封级 40003（invalid token）都归一成对应 Kind，不走「解析失败」 |

**验证**：真 WASM ABI 自测（`DEEPSEEK_POW_WASM` 指向下载的官方 wasm，解低难度挑战 + 六字段头）；mock 上游端到端（建会话 → 挑战 → 真求解 → patch 流解析成 THINK/正文/finished → 删会话）；面板粘贴流（发起 → 粘 token → 轮询入池，popup 数 = 0）；`check.sh`/`audit-ui`/`audit-vue` 全绿。
**注意**：账户凭证（userToken）落盘在凭证目录（0600），与其它渠道同样处理。

### 0.4.8 — Gemini 渠道：官方 OAuth 的登录式接入（原生工具调用）

**这批「登录式」里最优的一条**：不是逆向网页，而是走 **Gemini Code Assist CLI 的官方 OAuth**。

| 环节 | 做法 |
|---|---|
| 授权 | **user-code 流 + PKCE**：面板给 Google 授权地址（可二维码），用户在**自己浏览器**里点完，页面给一串 code → 粘回面板即完成（NAS 不需要公网回调、不需要桌面浏览器） |
| 令牌 | access + refresh；过期自动用 `refresh_token` 续期（Google 可能轮换 refresh，新值落盘） |
| **开通** | ⚠️ 只换令牌不够：还要 `loadCodeAssist` → 必要时 `onboardUser`（**free-tier 不能带 project**，带了 Precondition Failed）拿 project；**不开通直接调会 412/403**。这段结果缓存进凭证 |
| 对话 | `POST cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse`，请求外层包 `{model, project, request:{…}}`，模型名**裸名** |
| 工具调用 | **原生**：请求 `tools[].functionDeclarations`、响应 `parts[].functionCall{name,args}`（整包到达，不是分片）→ 我们合成标准 `tool_calls` |
| 指纹 | **不需要任何浏览器指纹**：OAuth 令牌即全部鉴权（本机实测四个 Google 域名都可达，不用代理） |
| 额度 | Google 账号登入 Code Assist Individual = **1000 请求/用户/天**（官方 quota 文档） |

**验证**：授权地址实测带对全部参数（client_id/redirect_uri/PKCE/state/access_type=offline/scope 含 cloud-platform）；
面板「添加账号 → Gemini → 授权」实测拿到真实 Google 授权地址且带粘贴入口；单测覆盖
（授权流、开通两种分支、账号不可用要透出上游枚举原因、原生工具调用与思考分片、请求体转换、
finishReason/usage 映射、错误归一）；`check.sh`、`audit-ui`、`audit-vue` 全绿。

### 0.4.7 — 账号分类隔离 + 全局出口代理（通义登录报错的解）

用户实测反馈两件事：**① 通义授权报错；② 新老渠道该分成两个分类**。

**① 报错根因（实测，不是猜）**：通义登录分两步 —— 拿设备码、换令牌。这台机器上：

| 请求 | 结果 |
|---|---|
| `/api/v1/oauth2/device/code`（拿设备码） | ✅ 两个边缘节点都 200 |
| `/api/v1/oauth2/token`（**换令牌**） | ❌ **两个节点、所有方法/头/带不带 cookie/尾斜杠，全部 405** —— 阿里边缘**在路径级别**挡掉，请求根本没进应用 |
| `portal.qwen.ai/v1/chat/completions`（对话） | ✅ 通（无 token 正常 401） |

出口走的是海外边缘（响应头 `x-ap: na-vancouver-pop`），所以换令牌这一步需要**能被上游接受的出口**。
做法：新增**全局出口代理**设置（`egress_proxy`）—— 登录（那时还没有账号凭证）与对话都走它；
优先级：**账号绑定代理 > 全局出口 > 环境变量**。改完新请求立即生效，不用重启。

**② 分类隔离**：`channel.Spec` 加 `Category`：

| 分类 | 渠道 | 说明 |
|---|---|---|
| `coding` 编程助手类 · 订阅额度池 | QoderCN / QoderCOM / TraeWork / WorkBuddyCN / WorkBuddyAI | 扫码或设备码授权，按订阅额度用；模型原生支持工具调用 |
| `chat` 聊天平台类 · 网页/CLI 登录 | **通义（Qwen）** / 千问办公 | 网页聊天产品的登录式接入；额度与风控模型不同，**两者不互相顶替** |
| `api` API Key 式来源 | Google AI Studio / 百炼 / OpenRouter… | 面板「接入源」页单独列 |

账号页按大类分成两段（同一类里先可用后暂停），接入源页的渠道行也标出大类 ——
分开的理由不是好看：两类**额度模型、风控强度、能不能调工具**都不同，混在一起会让人拿「余额」去理解「限速」。

**验证**：面板实测账号页出现两个分类标题；设置页出现「全局出口代理」字段；`audit-ui` 双端 7 屏 0 问题；
`audit-vue` 0 告警 0 报错；`check.sh` 全绿（含 `egress_proxy` 落盘往返用例 —— 这次同时加了 `decodeSettings` 白名单项）。

### 0.4.6 — 通义（Qwen）渠道：扫码登录 + 原生工具调用

**第一个「网页/CLI 登录式」新渠道**（不再是官方 API Key 那一类）。协议取自公开参考实现
（`Rfym21/Qwen2API`，819★，2026-09 仍在更新）的实测结论，**没有猜**：

| 环节 | 做法 |
|---|---|
| 登录 | **设备码（RFC 8628）+ PKCE** → 面板给出 `https://chat.qwen.ai/authorize?user_code=…&client=qwen-code`，用户用手机/浏览器点一下即完成；**我们不要邮箱密码**（参考实现存密码，我们刻意不走那条） |
| 续期 | `grant_type=refresh_token`，上游轮换 refresh_token → **落盘新值**（只换内存不落盘 = 重启后拿作废旧值） |
| 对话 | `POST portal.qwen.ai/v1/chat/completions`，**纯 OpenAI 格式**，工具调用是**原生**（含流式 arguments 分片、tool_choice 四态） |
| 请求体 | 两个硬要求：`qwen3.5-plus → coder-model` 重定向；首条 system 的 content 必须以「空 text + `cache_control: ephemeral`」开头（上游用它表达可缓存前缀） |
| 指纹 | **不需要**：CLI 端无签名、无 `bx-ua`、无 ssxmod —— 参考实现里的网页端才要伪造指纹，我们刻意不走网页端（网页端的原生工具调用还会被服务端 agent loop 拦掉） |
| 防封号 | 该渠道 `DefaultMinIntervalSec = 2`（出厂每账号最小间隔 2 秒），配合已就绪的闸门与「一账号一出口」 |

**实测（本机直连，未用任何账号）**：

| 检查点 | 结果 |
|---|---|
| 设备码端点（CLI 风格头） | ✅ `HTTP 200`，返回 `user_code` + `verification_uri_complete`（`expires_in: 900`） |
| **坑**：不带客户端标识 | ❌ 用 Go 默认 UA 会被**阿里云 WAF** 拦成 HTML 页 → 认证请求也带 `QwenCode/…` 与 Origin/Referer |
| CLI 对话端点 | ✅ 无 token 正确返回 `401 invalid_api_key`（端点存在、鉴权生效） |
| 面板授权入口 | ✅ 点「添加账号 → 通义 → 授权」拿到真实授权地址 + 二维码（playwright 实测，截图 `prototype/shots/app-qwen-auth-light.png`） |
| 单测 | ✅ 设备码流程（pending → token）、refresh 轮换、CLI 头与请求体形状、SSE 工具调用分片、不透明 token 也要有 UID、错误归一 |
| `check.sh` | ✅ 全绿 |

**已知未做**：CLI 端每账号**每日 2000 次**的额度没有本地计数（参考实现有）；余额未知（上游无接口，如实显示「未知」）；
面板授权窗口是全局 5 分钟，而设备码实际有效期 15 分钟（想更宽松可以重发一次授权）。

### 0.4.5 — 防封号基础设施：一账号一出口 + 每账号串行与最小间隔

**动机**：接网页渠道（扫码登录那类）最大的风险不是写不出来，是**被封号与下架**
（见 `docs/06`）。封号从来不是单一原因，而是「同一账号在多处露出破绽」。本版把两件最要紧的事
做成**框架能力**，之后每家网页渠道自动受益，不用各自想办法：

- **一账号一出口**：`channel.NewHTTPClient(cred, timeout)` 按凭证建 HTTP 客户端，
  凭证 `Extra["proxy"]` 绑哪个出口就走哪个（没绑则回落环境变量，整机代理的部署方式不变）。
  「多个账号同一个 IP」是平台关联封号的第一条线索，所以出口绑到**账号**上而不是进程上。
- **每账号串行 + 最小请求间隔**：`pool.Gate` —— 取号之后、打上游之前卡住；
  同账号的请求排队（不是并发冲上去），距上次不足间隔则等待。间隔按渠道可配
  （设置里的 `account_min_interval_sec`，网页渠道建议 1–3 秒）。
- **释放时机**：闸门的释放挂在**流关闭或读尽**上（`router.releaseOnClose`）——
  漏了会让账号被永久占住，表现成「这个号以后一直排队超时」，比封号还难查。

**顺手揪出的一个真 bug**：`store.decodeSettings` 是**白名单式**的，新字段没加进白名单就会被
**静默丢掉** —— 新加的 `account_min_interval_sec` 正好踩中（面板保存了、重启后失效）。
已修 + 补了往返用例，并在代码里写明「加字段必须同时加白名单 + 用例」。

**验证**：

| 场景 | 结果 |
|---|---|
| 同账号并发（间隔 5 秒） | ✅ 2 个并发请求总耗时 **5008 ms**（串行 + 间隔生效） |
| 不同账号之间 | ✅ 互不等待（单测） |
| 排队超时 | ✅ 按 ctx 返回可读错误，不无限等 |
| 闸门释放 | ✅ 客户端提前关流与读尽两条路径都释放（单测钉住） |
| 取闸门失败 | ✅ 不打上游、不冷却账号 |
| 凭证绑代理 | ✅ transport 走该代理；非法地址回落而不是让渠道崩掉 |
| `check.sh` | ✅ 全绿 |

### 0.4.4 — 工具调用模拟层：让「只会聊天」的来源也能给 coding agent 用

**动机**：扫码登录来的网页聊天平台（豆包/DeepSeek/Kimi 这类）**没有工具调用协议** —— 接口只有
「发消息 → 回文本」，没有放 `tools` 的位置。硬要它给 Studio / Claude Code 当后端，模型只能把
「我要调用工具」写成文本，客户端拿不到 `tool_calls`。

**做法**（业界叫 prompt-based tool calling，`internal/toolshim`）：

```
客户端发来 tools
  ↓ 工具定义 + 输出格式写进系统提示词（并要求「只输出 <tool_call>{…}</tool_call>」）
上游回文本
  ↓ 把标记解析回**结构化 tool_calls**（含流式分片、代码围栏、arguments 是字符串等变体）
客户端执行工具、把结果回传
  ↓ role=tool 改写成「[工具执行结果] …」的普通消息（聊天上游不认识 tool 角色）
循环
```

**能力分三档**（`channel.Spec`：`Tools` / `ToolsShim`，面板「接入源」里选）：

| 档位 | 行为 |
|---|---|
| `native` | 上游原生支持，`tools` 原样透传 |
| `shim` | 上游只会聊天，**由网关代做模拟** |
| `none` | 纯聊天来源；带 tools 的请求**明确拒绝**（不静默丢弃，F3.7） |

**实现要点**：

- `toolshim.ToolPrompt` 把工具 schema 原样写进提示词（简化 schema = 模型编参数）；
- `toolshim.BuildRequest` 改写请求：系统提示词合并、清掉 tools/tool_choice、工具消息改纯文本；
- `toolshim.Parser` 增量解析：**标记被 SSE 切成两片也要拼出来**（留可能是前缀的尾巴），
  宽容处理代码围栏 / `arguments` 是字符串 / 未闭合标记；
- **解析不出来绝不丢内容** —— 一律当正文交出去（红线一），宁可客户端看到原文；
- `toolshim.Wrap` 包装 chunk 流：正文照旧，标记变成 `tool_calls` 增量；`finish_reason` **推迟到末尾**
  再发（解析器可能到流结束才吐出最后一个调用），有调用时归一成 `tool_calls`；
- OpenAI 与 Anthropic 两个入口都接了这条路（Anthropic 侧先把 `tools` 转成 OpenAI 形态）。

**可靠性如实说**：模拟档靠模型守格式，稳定性比原生低一档。所以「接入源」页的连通性测试会**真发一条
带工具定义的请求**，拿不到结构化 tool_calls 就标「未确认」；面板里的档位选择也把这段说明写在旁边。

**验证**：

| 场景 | 结果 |
|---|---|
| 分片标记 / 纯文本 / 代码围栏 / arguments 是字符串 / 未闭合 | ✅ 单测逐条钉住 |
| 解析失败不丢内容 | ✅ 原样当正文返回 |
| 网关层（模拟渠道） | ✅ 客户端发 tools → 网关改写成提示词 → 上游回标记 → 客户端收到标准 `tool_calls` |
| 真机（模拟的「只会聊天」上游） | ✅ 上游收到 `has_tools:false` + 带工具说明的 system；客户端拿到 `tool_calls` + `finish_reason: tool_calls`，正文保留 |
| 工具往返 | ✅ 上游收到 `roles: [system, user, assistant, user]`，无 `tool_call_id` 字段（聊天上游不会报错） |

### 0.4.3 — 接入源：一个适配器覆盖所有官方 API，面板里三类东西一次管完

**做了什么**（原型见 `prototype/08-providers.html`、`prototype/09-provider-add.html`，裁定见 `docs/02` §12）：

- **通用 OpenAI 兼容上游适配器**（`internal/adapter/openaiup`）：官方 API 那边（Google AI Studio / 阿里百炼 /
  OpenRouter / DeepSeek / 智谱 / 火山方舟 …）线上格式本来就是 OpenAI 兼容，所以**一个适配器覆盖一整类**，
  不用一家写一个。填 `base_url` + `key` + 模型名即可。
- **配置存储**（`internal/store/providers.go`）：`providers.json`（0600，明文 key 等于密码）；名称校验，
  不允许与内置渠道重名（否则会覆盖注册表里的实现）。
- **统一入口落地**：key 式来源与登录式渠道**共用同一个网关入口与账号池** —— 客户端仍只连一个地址、一个 key，
  模型名带来源前缀（`google/gemini-3.8-flash`）。key 式来源合成一条「账号」进池，否则路由层选不到号。
- **面板「接入源」页**（`/api/providers*` 四个接口 + Vue 视图）：两类来源混排一张表，靠「类型」列与
  最左侧色块区分（登录式=渠道身份色，key 式=中性方块）；**「添加接入源」三步向导**：选类型 → 填参数
  （17 个主流平台预设，选中自动带出 base_url，并写明免费档能拿到什么 / 要不要实名）→ **连通性测试**后保存。
- **连通性测试是这一步的重点**：真发一条**带工具定义**的请求，四项分开报 —— 鉴权 / 模型目录 /
  **工具调用** / 首字延迟。工具调用那项直接决定这个来源能不能给 Studio / Claude Code 当后端
  （0.4.2 之前我们静默丢掉 tools，客户端拿到的是一堆乱码，就是缺了这一步验证）。
- **开发工具**：`tools/mock-openai-upstream.py` —— 没有真实 key 时也能端到端验收（分片 tool_calls、
  非流式整包、错误信封都模拟了）。

**验证**（模拟上游 + 真机面板，全链路）：

| 场景 | 结果 |
|---|---|
| 加一个来源 → 模型目录 | ✅ 立刻多出 `google/mock-strong-1` …（新来源自动进目录与路由） |
| 带工具调用（非流式/流式） | ✅ 真出 `tool_calls`，分片能拼回；上游确认收到 `tools` |
| 工具往返 | ✅ 上游收到 `roles: [user, assistant, tool]` 且带 `tool_call_id` |
| 不支持工具的来源 | ✅ 400 + 可读原因（「已明确拒绝而不是静默忽略」） |
| 面板向导全过程 | ✅ 选预设自动带出 base_url → 测试（鉴权 ✓ / 目录 ✓ 2 个 / 工具调用 ✓ 可用）→ 保存后列表与目录同步更新 |
| 删除来源 | ✅ 从列表、注册表、池子、模型目录里一起消失 |
| 非法配置 | ✅ 明确拒绝（如「名称 qodercn 与内置渠道重名」） |
| `check.sh` / `audit-ui.mjs` / `audit-vue.mjs` | ✅ 全绿：双端 7 屏无溢出、Vue 0 告警、0 页面报错 |

### 0.4.2 — 工具调用（tools / tool_calls）打通：Studio 里选的模型终于能干活了

**现象**：在 Ekko Studio 里选 PoolGate 下发的模型，别的 provider 都正常，只有它「不干活 / 回一堆乱码」。

**根因（本机实测，不是猜）**：网关**静默丢弃了客户端的 `tools`** —— `gateway.go` 的入参结构里没有
`tools/tool_choice`，内部契约 `channel.ChatRequest` 也没有，消息体只有 `role` + `content(string)`。
上游收不到工具定义，只能把「我要调用工具」写成文本。拿现网凭证打**旧版**实例，模型回的是：

```
我来调用 get_weather 工具查询北京现在的天气。
  <function_calls><invoke name="get_weather"><parameter name="city">北京</parameter></invoke></function_calls>
```

客户端（Claude Code / Studio）解析不到 `tool_calls` → 表现成「模型不回复 / 回一堆看不懂的乱码」。
顺带这也违反 F3.7「不支持的参数按能力位降级或明确拒绝，不静默丢弃」。

**修法（移植 wild-work 已验证的做法，并补上它没有的诚实声明）**：

- **契约**：`channel.ChatRequest` 加 `Tools`/`ToolChoice`；`channel.Message` 加 `ToolCalls`/`ToolCallID`/`Name`；
  `channel.ToolCall` 加 `Index`（流式分片序号，**不 omitempty**：OpenAI 的 `index=0` 也是显式出现的，
  缺了它客户端可能把分片当成多个调用）。
- **网关 · OpenAI 入口**：解析 `tools`/`tool_choice` 并透传；`content` 支持 字符串 / parts 数组 / null；
  保留 assistant 的 `tool_calls` 与 `role=tool` 消息的 `tool_call_id`/`name`；非流式聚合按 index 拼回工具调用，
  并把 `finish_reason` 纠正成 `tool_calls`。
- **网关 · Anthropic 入口（`/v1/messages`）**：`tools[].input_schema → function.parameters`；
  assistant 的 `tool_use` → `tool_calls`，user 的 `tool_result` → 独立的 `role=tool` 消息（带 id/name）；
  响应侧输出 `tool_use` 块 + `stop_reason=tool_use`，流式侧输出 `input_json_delta`。
- **适配器**：QoderCN / QoderCOM（COSY）与 WorkBuddy 两家的请求体带上 `tools`；TraeWork 按 SOLO 形态改写
  （`function → function_call`、`parameters` 序列化成字符串、`tool_choice` 归一成函数名）；
  四家的 SSE 解析统一走 `channel.ParseOpenAIToolCalls`（顺带补上 `index`）。
- **能力位说实话**：千问办公（chat-ws 的 `new_prompt` 根本放不下工具定义）改成 `Tools: false`，
  网关对它的工具请求**明确拒绝并给原因**，而不是静默丢掉。

**真上游验证（现网 QoderCN 凭证，五个场景全过）**：

| 场景 | 结果 |
|---|---|
| 旧版 + tools（复现） | `finish_reason: stop`、无 `tool_calls`，content 是 `<function_calls>…` 文本标记 |
| 非流式 + tools | `finish_reason: tool_calls`，`tool_calls[0] = {id, get_weather, {"city":"北京"}}` |
| 流式 + tools | 5 个分片（首片带 id/name，后续只带 arguments 片段，`index: 0`），按 index 能拼回完整调用 |
| `/v1/messages` + tools | `stop_reason: tool_use` + `tool_use` 块（`input.city = 上海`） |
| 工具往返（第二轮把结果喂回） | 模型拿到 `{"temp":22,"desc":"多云"}` 后正常作答 |

`check.sh` 全绿（新增：网关 6 例 —— tools 透传、parts content、不支持时明确拒绝、非流式工具调用聚合、
mergeToolCalls 的两种上游形态、Anthropic tools 往返；适配器 4 例 —— COSY body 带 tools 与工具消息、
SOLO 形态改写、SOLO 工具帧解析、千问办公能力位）。

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
