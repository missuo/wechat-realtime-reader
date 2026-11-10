---
name: wechat-db-keys
description: 获取、验证和更新本项目使用的 macOS 微信 4.x 本地数据库密钥，使用临时应用副本与启动前 Frida PBKDF2 监听。适用于本人账号的首次配置或旧 Key 校验失败后的维护。
---

# 微信数据库 Key 维护

先读 [references/capture.md](references/capture.md)，再执行对应步骤。项目根目录是本 Skill 上两级；所有路径以实际工作目录为准。

## 决策与完成标准

1. 先运行 `bin/wechat-reader doctor`。已有 Key 通过校验时直接复用；用户只要求写文档时不启动抓取。
2. 抓取针对用户本人已登录的本机账号。确认应用版本、账号数据库路径、正式微信已正常退出，再准备同版本临时副本。不得修改正式应用签名。
3. 必须按 `spawn suspended → attach → load hook → 确认 ready → resume` 执行。运行中 attach 很容易错过启动时的派生。
4. 使用 `scripts/keys.py capture`，最长 120 秒；捕获够用后立即 Ctrl-C。该操作让副本读取同一账号的正式数据，因此不允许与正式微信并行运行，也不能宣称 HOME 隔离。不要重复扫码或循环尝试登录。
5. 使用 `scripts/keys.py pair` 离线配对。只保存通过数据库首页 HMAC-SHA512 校验的 32 字节值。捕获次数和 salt 相同都不能替代认证。
6. 更新配置后运行 `doctor`；需要验证完整解密时才运行 `sync`。结束时确认临时主进程和辅助进程均已退出，再恢复官方微信。

## 工具与限制

- `capture`：只 hook 系统 CommonCrypto C 函数，不依赖 Objective-C bridge；候选值写入私密 JSONL，终端只显示计数。hook 加载错误时不恢复应用启动。
- `pair`：读取 `.db` 首页，计算认证码，不打开可写 SQLite 连接；默认输出到不存在的新文件，避免覆盖现有 Key。
- 完整步骤、参数含义、配置格式、错误定位、清理和验证状态见参考文件。
- 实际 Key、候选日志、账号映射、明文库保持在 `~/.config/wechat-realtime-reader` 等私密目录，目录 0700、文件 0600。不得复制到项目或输出到聊天。
- 本项目曾在 macOS / WeChat 4.2.0.16 上完成真实首页和实时消息验证；本 Skill 新增脚本需区分离线验证与现场验证，不把以往结果称作新脚本的实测结果。
- 副本签名保留正式应用文件，但不消除账号风控或系统权限变化；此前本机调试历史有过登录限制。遇到安全提示、限制登录或崩溃立即停止该次流程。
