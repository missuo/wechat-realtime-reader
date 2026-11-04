# 获取 Key 的完整操作

## 原理与适用范围

本项目读取 macOS 微信 4.x 的 WCDB/SQLCipher 风格数据库。已验证格式是 4096 字节一页、16 字节 salt、80 字节 reserve（16 字节 IV + 64 字节 HMAC），AES-256-CBC，HMAC-SHA512。

监听系统 C 函数 `CCKeyDerivationPBKDF` 的入口和出口：入口记录参数与输出指针，出口在返回值为 0 时复制结果。不要把输出指针本身当 Key，也不要在入口读取尚未生成的输出。CommonCrypto 参数顺序为：

```text
algorithm, password, passwordLen, salt, saltLen, prf, rounds, derivedKey, derivedKeyLen
```

本工具只处理 PBKDF2（2）+ HMAC-SHA512（5）、16 字节 salt、32 字节输出，以及 256000 或 2 轮的调用。256000 轮输出是候选 AES Key；2 轮通常用于 HMAC Key，其 32 字节输入也可能是 AES Key。最终身份由首页 HMAC 判定。捕获文件中的 `input`、`output` 都是敏感材料。

每个数据库要独立配对，不能把某个 `message_0` 的 Key 套给其他分片。同一账号和数据库在 salt/派生输入未变化时通常可复用；重建数据库、迁移、重装或升级后应重新校验，不能保证永远不变。微信也可能改用其他派生实现；此 hook 并非所有版本必经路径。

## 1. 先判断是否需要抓取

在项目根目录运行：

```bash
go build -o bin/wechat-reader ./cmd/wechat-reader
bin/wechat-reader doctor
```

已有配置且需要的库都通过，直接使用现有 Key。缺少配置时，先确定真实账号的 `db_storage` 路径：

```text
~/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files/<账号目录>/db_storage
```

明确绑定账号，不能按目录新旧猜测。此次文档操作不需要读取或显示真实 Key。

## 2. 准备同版本临时副本

先在微信菜单正常退出官方客户端，确认该应用及其辅助进程已退出。只关闭窗口不等于退出。避免两个客户端同时访问同一个账号目录。

以下命令从项目根目录执行，保持在同一个终端中，以保留 `capture_work` 变量。示例源为 `/Applications/WeChat.app`，按实际位置调整。副本位于临时目录；Key 则保存在长期私密目录。

```bash
umask 077
capture_work="$(mktemp -d /private/tmp/wechat-db-keys.XXXXXX)"
ditto --noextattr --noqtn /Applications/WeChat.app "$capture_work/WeChat.app"
/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$capture_work/WeChat.app/Contents/Info.plist"
codesign --force --deep --sign - \
  --entitlements SKILLS/wechat-db-keys/references/capture-entitlements.plist \
  "$capture_work/WeChat.app"
codesign --verify --deep --strict "$capture_work/WeChat.app"

python3 -m venv "$capture_work/venv"
"$capture_work/venv/bin/python" -m pip install 'frida==17.25.2'
mkdir -p ~/.config/wechat-realtime-reader/key-capture
chmod 700 ~/.config/wechat-realtime-reader ~/.config/wechat-realtime-reader/key-capture
```

此处对副本使用最小调试 entitlements，未保留正式沙盒签名。副本需读取同一个用户的账号数据才能派生对应 Key；**不能设置空白 HOME/CFFIXED_USER_HOME 后期待获得旧数据库 Key**。系统隐私权限仍可能阻止读取，此时先停止并检查报错，不关闭 SIP 或修改正式应用。

复制签名保留了正式应用的文件与签名，但副本进程具有不同签名身份，仍可能被风控识别。本机历史调试有过登录限制，不要将这种流程作为日常运行方式。

## 3. 启动前安装监听器

不要双击副本，也不要先启动再 attach。执行：

```bash
"$capture_work/venv/bin/python" SKILLS/wechat-db-keys/scripts/keys.py capture \
  --app "$capture_work/WeChat.app" \
  --seconds 30 \
  --output ~/.config/wechat-realtime-reader/key-capture/candidates-new.jsonl
```

脚本顺序是暂停启动、attach、加载 hook、收到 `ready`、恢复启动。只在终端显示 `Hook ready` 后才会恢复应用。捕获值仅写入 0600 文件，终端只显示计数。默认 30 秒、最多 120 秒，到期或 Ctrl-C 会杀掉本次启动的主进程。

有已保存登录态时，数据库打开阶段通常就会触发派生；有些库懒加载，需要用户进入相应会话。若进入扫码/确认页面，需要用户本人完成，脚本不代替确认。够用后立即 Ctrl-C；若出现限制登录、安全提示或崩溃，结束这次维护，不反复重试。

不要直接 `tee` 应用输出或使用会打印 Key 的 Frida console 脚本。本工具通过 `stdio='pipe'` 抑制应用控制台输出。结束后还要检查副本的辅助进程：

```bash
ps -axo pid=,command= | rg -F "$capture_work/WeChat.app/"
```

若有残留，按输出逐个核实 PID 后 `kill -TERM <PID>`，再次检查。只操作本次临时目录下的进程。

## 4. 离线验证并配对

替换下面的账号目录占位符；不要使用本示例字面值：

```bash
"$capture_work/venv/bin/python" SKILLS/wechat-db-keys/scripts/keys.py pair \
  --capture ~/.config/wechat-realtime-reader/key-capture/candidates-new.jsonl \
  --db-root "$HOME/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files/<账号目录>/db_storage" \
  --output ~/.config/wechat-realtime-reader/keys-new.json
```

认证算法与 `internal/crypto/crypto.go` 一致：

```text
salt = page1[0:16]
macSalt = salt XOR 0x3A
macKey = PBKDF2-HMAC-SHA512(candidateAESKey, macSalt, rounds=2, len=32)
expected = HMAC-SHA512(macKey, page1[16:4032] || LE32(1))
compare expected with page1[4032:4096] using constant-time comparison
```

只有恰好一个候选通过认证的库会写入结果。候选按值去重，不按捕获顺序猜库；输出键名是相对于 `db_storage` 的路径，如 `message/message_0.db`。缺库不会自动伪造成功；需检查聊天必需的 `contact/contact.db`、`session/session.db` 和所有实际存在的 `message/message_N.db` 是否已配对。`pair` 只读主库首页，不做 WAL 快照合并；抓取结束后保持客户端退出有助于稳定验证。

脚本只输出匹配数量，不显示 Key。文件格式是项目直接接受的扁平 JSON：

```json
{
  "contact/contact.db": "<64位十六进制Key>",
  "session/session.db": "<64位十六进制Key>",
  "message/message_0.db": "<64位十六进制Key>"
}
```

## 5. 接入读取器

已有 `keys.json` 时先私密备份，再将 `keys-new.json` 中通过认证的条目合并进去。不要直接用不完整结果覆盖完整旧映射；如果旧文件已有同名条目，也要用当前数据库认证后选择。备份同样保持 0600，不写入项目。

首次配置且 `keys.json` 尚不存在时，可用 `cp -n ~/.config/wechat-realtime-reader/keys-new.json ~/.config/wechat-realtime-reader/keys.json`，随后检查是否确实创建成功。`source-config.json`（0600）格式为：

```json
{
  "wxid": "<账号目录名>",
  "db_base_path": "~/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files/<账号目录>/db_storage",
  "vault_dir": "~/.local/share/wechat-realtime-reader"
}
```

`decrypted_dir` 未指定时使用 `vault_dir/decrypted/current`。明文目录不放 Projects、Desktop、Downloads 或同步盘。完成后：

```bash
chmod 600 ~/.config/wechat-realtime-reader/keys.json ~/.config/wechat-realtime-reader/source-config.json
go build -o bin/wechat-reader ./cmd/wechat-reader
bin/wechat-reader doctor
```

`doctor` 逐库尝试 derived/raw 模式并验证首页。新工具配对保存的是 derived AES Key，因此不需要再执行 256000 轮派生。要验证全库时再执行 `bin/wechat-reader sync`，该步骤会生成私密明文快照。完成后退出临时副本，重新打开官方微信；日常 `serve`/`mcp` 不加载 Frida。

## 6. 清理与保留

确认临时目录下所有进程均已退出后，把终端变量 `capture_work` 显示出的**本次精确目录**移到废纸篓；删除的是临时应用和 venv。不要用模糊通配符批量清理，也不要删除正式应用或账号数据。

本项目用户已要求保留所有 Key 相关材料：保留 `keys.json`、旧备份、`keys-new.json`、`source-config.json` 和 `key-capture/`；原始候选文件也包含 Key，继续保持 0600。除非用户另行明确要求，不删除它们。

## 常见失败

| 现象 | 检查与处理 |
| --- | --- |
| `Operation failed (FileExistsError)` | 工具拒绝覆盖现有输出；换一个新文件名，保留旧文件。 |
| 没有 `Hook ready` | hook 未装好；工具不恢复启动并清理主进程。检查 Frida 版本、导出符号、签名和调试权限。 |
| `ObjC is not defined` | Frida 17 把 bridge 拆出 GumJS。本脚本只调用 C API，不需要 ObjC；检查是否误用了其他旧脚本。 |
| 0 次捕获 | 确认不是先启动再 attach；检查登录态、实际账号目录、进程是否退出、库是否被懒加载。若版本改了派生实现，停止并分析新实现，不能直接认定 Key 被销毁。 |
| 有捕获但 0 配对 | 可能只抓到了 MAC 输出、读错账号、格式改变或 Key 已轮换。先核对首页布局；不要跳过 HMAC 校验。 |
| 部分库缺失 | 捕获仅覆盖已打开的库；先判断实时聊天必需库是否齐全，再决定是否需要另一次维护。 |
| `PermissionError` / 无法 attach | 检查副本签名、Python/Frida 架构、隐私权限。不要给正式应用重签名，不自动使用 sudo 或关闭系统保护。 |
| 风控、安全提示、限制登录 | 立即结束本次操作；恢复官方客户端，不循环重试。 |

## 验证状态与依据

既有项目在 WeChat 4.2.0.16 上保存了通过首页验证的 Key，并验证过新消息回读。此 Skill 中的新脚本应以当前提交的离线测试和实际维护记录分别报告；没有真实启动验证时不能声称“现场抓取成功”。

2026-11-04：新增工具通过 3 个离线合成测试，覆盖正确 Key、错误 Key、salt/密文/IV/HMAC 篡改、候选去重、文件权限、拒绝覆盖与拒绝私密目录之外的输出；本次未启动微信验证新抓取脚本。可在项目根目录复跑：

```bash
python3 -m unittest discover -s SKILLS/wechat-db-keys/scripts -p 'test_*.py' -v
```

- [Frida JavaScript API](https://frida.re/docs/javascript-api/)：Module、Interceptor、send、NativePointer API。
- [Frida 17 发布说明](https://frida.re/news/2025/05/17/frida-17-0-0-released/)：runtime bridges 拆分及 API 变化。
- 项目密码学实现：`internal/crypto/crypto.go`；配置定义：`internal/config/config.go`。
