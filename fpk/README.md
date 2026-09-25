# FPK 打包（飞牛 fnOS 原生包）

`build-fpk.sh <版本号> [输出目录]` → 产出 `dist/poolgate-<版本号>.fpk`（给了第二个参数就再拷一份过去）。

正式发布包在 [Releases](../../releases) 里下载，不必自己打。

## 打之前需要准备三样（都不入库）

| 文件 | 怎么来 |
|---|---|
| `payload/poolgate` | 在 `../src` 跑 `./build.sh <版本号>`，把 `src/build/poolgate` 拷过来（**版本号要一致**，它会写进 `-ldflags`） |
| `tpl/wwbridge` | 飞牛应用网关桥（fnOS 应用包通用件），来自 `peng418/wildwork-fpk` 的 `tpl/` |
| `~/.local/bin/fnpack` | 飞牛的打包工具（fnpack），本机已装 |

## 铁律（踩过一次，别再犯）

`wizard/install|upgrade|config` 必须是**非空** JSON 数组，且每步有非空 `stepTitle`。
空数组会让应用中心解析向导时空指针 panic，界面只给一句笼统的「应用包不符合系统要求」
（真实原因在 `/var/log/trim_app_center/error.log`：`Code:10111 ... (*vo.WizardData)(nil)`）。
`build-fpk.sh` 已内置这条校验，构建期就会挡下。

另外注意：用 `find -type f -exec chmod 644` 收敛权限时会把不带扩展名的可执行文件
（`tpl/wwbridge`、`payload/poolgate`、`tpl/ui/index.cgi`）一起降权，下一轮打包会直接
报「缺网关桥」——收敛后记得把它们恢复 755。
