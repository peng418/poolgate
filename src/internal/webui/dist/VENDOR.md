# 第三方前端资源（vendored）

这里的东西是**直接拷进来的第三方文件**，不是我们写的。改之前先看授权与来源。

| 文件 | 版本 | 授权 | 来源 | 用途 |
|---|---|---|---|---|
| `vue.global.prod.js` | 3.x 生产构建 | MIT | https://unpkg.com/vue@3/dist/vue.global.prod.js | 面板框架（无构建步骤，直接挂载） |
| `qrcode.js` | 1.4.4 | MIT | https://unpkg.com/qrcode-generator@1.4.4/qrcode.js | 把渠道授权地址画成**真**二维码（手机扫码完成授权） |
| `brands/` | — | 各家商标，见该目录 `SOURCES.md` | 各平台官网的 favicon / logo | 面板里标明「这条模型来自哪家」 |
| `poolgate.css` / `poolgate.js` | — | 本项目 | `prototype/assets/` | 原型的设计令牌与脚本，界面一比一还原时直接沿用 |

`qrcode.js` 只用了它的两个能力：`qrcode(typeNumber, ecc)` + `createSvgTag({cellSize, margin, scalable})`，
产出的是 `fill="white"` 底 + `fill="black"` 模块的 SVG —— 明暗两种主题下都能被扫码器识别
（这正是不跟随主题色的原因）。
