# 品牌图标的来源

这些文件是**各平台自己站点上的图标**（favicon / apple-touch-icon / 站点 logo），
用来在面板里标明「这条模型来自哪家」。**商标归各自所有者**，此处仅作识别用途；
若某个平台方要求移除，删掉对应文件即可（前端会自动退回「色块 + 名称」的形态，不会报错）。

| 文件 | 名称 | 取自 |
|---|---|---|
| `doubao.png` | 豆包 | `www.doubao.com` 站点图标 |
| `qwen.png` / `bailian.png` | 通义 / 阿里百炼 | `qwen.ai` 站点图标（阿里 CDN） |
| `qwenwork.png` | 千问办公 | `qwenwork.cn` 站点图标（阿里 CDN） |
| `kimi.png` / `moonshot.png` | Kimi / Moonshot | `www.kimi.com` 站点图标 |
| `chatglm.png` / `glm.png` | 智谱清言 / 智谱 GLM | `chatglm.cn` 站点图标 |
| `yuanbao.png` | 腾讯元宝 | `yuanbao.tencent.com` 站点图标 |
| `deepseek.png` | DeepSeek / DeepSeek 官方 API | `www.deepseek.com` 站点图标 |
| `qodercn.svg`（QoderCOM 共用） | QoderCN / QoderCOM | `qoder.com` 站点图标 |
| `traework.*` | TraeWork | `www.trae.ai` 站点图标 |
| `codebuddy.svg`（WorkBuddyCN 共用） | CodeBuddy / WorkBuddyCN | `www.codebuddy.cn` 站点 logo |
| `workbuddyai.*` | WorkBuddyAI | `www.workbuddy.ai` 站点 logo |
| `lingma.png` | 通义灵码 | `lingma.aliyun.com` 站点图标 |
| `iflow.png` | iFlow | `iflow.cn` 站点图标 |
| `antigravity.*` | Antigravity | `antigravity.google` 站点 logo |
| `windsurf.*` | Windsurf | `windsurf.com` 站点图标 |
| `kiro.*` | AWS Kiro | `kiro.dev` 站点图标 |
| `gemini.png` | Gemini | `gemini.google.com` 站点图标（gstatic） |
| `google.png` | Google AI Studio | `www.google.com` 站点图标 |
| `anthropic.png` | Anthropic（Claude 订阅） | `www.anthropic.com` 站点图标 |
| `openrouter.png` | OpenRouter | `openrouter.ai` 站点图标 |
| `volc.png` | 火山方舟 | `www.volcengine.com` 站点图标 |
| `siliconflow.png` | 硅基流动 | `siliconflow.cn` 站点图标 |
| `modelscope.png` | 魔搭 ModelScope | `modelscope.cn` 站点图标 |
| `minimax.png` | MiniMax | `www.minimaxi.com` 站点图标 |
| `groq.png` | Groq | `groq.com` 站点图标 |
| `together.png` | Together AI | `www.together.ai` 站点图标 |
| `mistral.png` | Mistral | `mistral.ai` 站点图标 |
| `xai.png` | xAI Grok | `x.ai` 站点图标 |
| `nvidia.png` | NVIDIA NIM | `build.nvidia.com` 站点图标 |
| `perplexity.png` | Perplexity | `www.perplexity.ai` 站点图标 |
| `chatgpt.svg` | ChatGPT | simple-icons（CC0）的 OpenAI 标识 |
| `copilot.svg` | GitHub Copilot | simple-icons（CC0）的 Copilot 标识 |
| `custom.svg` | 自定义来源 | 本仓自绘（原创，无第三方权利） |

SVG 是矢量（明暗主题下都清晰），其余是从站点图标缩放成的 64×64 PNG。
同一家的多个渠道/来源共用一份图标（例如 QoderCOM 用 `qodercn.svg`、WorkBuddyCN 用 `codebuddy.svg`、
DeepSeek 官方 API 用 `deepseek.png`）—— 前端按名字查表，表在多处复用同一个文件。
