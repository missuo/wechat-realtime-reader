# WeChat Realtime Reader

一个只读的 macOS 微信 4.x 本地聊天记录近实时读取系统设计草案。

> 目标是读取已经同步到本机并提交到 SQLite WAL 的消息。不控制微信 UI，不发消息，不修改微信数据库，不默认导出聊天记录。

## 已验证的事实

- 环境：macOS，WeChat 4.2.0.16。
- 数据库位于官方容器的 `xwechat_files/<account>/db_storage/`。
- 聊天核心库为 `contact/contact.db`、`session/session.db`、`message/message_0.db` 到 `message/message_5.db`。
- 消息库使用 4096 字节页、80 字节 reserve、AES-256-CBC 和 HMAC-SHA512。
- 本机已保存 26 个通过真实数据库首页校验的 key；聊天所需库全部命中。
- 已验证主库 + WAL 合并、逐页 HMAC、AES 解密和 SQLite 完整性检查。
- 现场测试中，新消息同时修改 `session.db` 和对应的 `message_N.db`，约 1.5 秒内完成刷新和正文回读。

## 密钥边界

真实 key 不得放入本项目、Git、日志或聊天输出。当前私密存储位置：

```text
~/.config/wechat-realtime-reader/
├── keys.json                 # 配对后的数据库 key，0600
├── image-key.json            # 图片 .dat 的 AES/XOR key，首次运行自动推导，0600
├── source-config.json        # 数据源映射，0600
└── key-capture/
    └── wechat_frida_keys.log # 原始捕获记录，0600
```

运行时只加载已配对 key。不应在日常流程中启动 Frida 或自签名微信。仅在数据库 salt/key 轮换且旧 key 校验失败时，才将“重新获取 key”作为独立的人工维护流程。

获取 Key 的操作说明与配套工具见 [项目 Skills](SKILLS.md)，详细入口为 [wechat-db-keys](skills/wechat-db-keys/SKILL.md)。

## 整体架构

```mermaid
flowchart LR
    A[官方 WeChat] -->|SQLite 主库 + WAL| B[文件变化监视器]
    B --> C[稳定快照读取器]
    C --> D[WAL 校验与已提交帧重放]
    D --> E[逐页 HMAC 验证]
    E --> F[AES-256-CBC 解密]
    F --> G[私有明文快照]
    G --> H[只读查询层]
    H --> I[本地 API / Agent Tool]
```

### 1. 数据源发现

- 账号目录必须由显式配置绑定，不根据目录排序或最新修改时间猜测。
- 默认只监视 `contact`、`session`、`message_0..N`、必要的 `biz_message_*`，以及存放语音音频的 `media_0..N` 和头像库 `head_image`。
- 朋友圈、收藏和搜索索引不属于实时聊天 MVP。

### 2. 变化监视

优先使用 FSEvents/kqueue 监视 `.db`、`-wal` 和 `-journal`，不做高频全目录扫描。

- 事件到达后 debounce 200–500 ms。
- 同一数据库最多同时运行一个刷新任务。
- 后续事件在当前刷新结束后合并成一次补刷新。
- 不以文件 mtime 作为“消息已提交”的证据，必须检查 WAL commit frame。

### 3. 稳定快照

每个数据库独立建立快照：

1. 记录主库和 WAL 的 inode、大小、mtime/ctime。
2. 读取主库和 WAL。
3. 再次读取元数据；前后不一致则重试。
4. 拒绝同时存在的 rollback journal，不发布可能处于事务中的快照。
5. 验证 WAL magic、版本、page size、generation salt、header checksum 和 frame rolling checksum。
6. 只重放最后一个已提交事务；忽略未提交帧。

这里的一致性是“单数据库提交级”，不声称多库之间具有原子性。

### 4. 认证解密

对每一页执行：

1. 从首页前 16 字节取 salt。
2. 用数据库 key 和 `salt XOR 0x3A` 派生 HMAC key。
3. 对加密 payload 和 little-endian page number 验证 HMAC-SHA512。
4. HMAC 通过后才用页尾 IV 执行 AES-256-CBC 解密。
5. 重建 SQLite 首页头，保留 reserve byte 语义。

任意页认证失败都必须使整个快照失败，不能跳过损坏页或回退到旧数据后伪装成最新状态。

### 5. 快照发布

- 解密到同目录临时文件。
- 运行 `PRAGMA quick_check`；周期性运行完整 `integrity_check`。
- 检查通过后用原子 rename 替换 `current` 快照。
- 失败时保留上一份快照，但必须标记 `stale` 和源数据最新时间，不得当作实时结果。
- 明文快照只存放在权限 700 的本机私密目录，文件权限 600。

### 6. 查询层

查询层只打开已发布的静态明文快照，使用 SQLite `immutable=1`/read-only 模式。

基本能力：

- 最近会话、未读数和最后消息。
- 按会话 ID + 时间范围查询消息。
- 只读取上次消费位点之后的新消息。
- 将一条已解析消息发布为本机事件，供 Agent 或本地面板消费。

默认返回结构化 JSON，不自动写 Markdown/文本导出。

## 近实时语义

“实时”指消息已被微信写入本机 WAL 并完成 commit 后，系统在数秒内可读。它不包括：

- 尚未同步到电脑的手机消息。
- 尚未提交的 SQLite 事务。
- 微信本地并未保存的服务端数据。
- 图片/语音/视频文件本体尚未下载的情况。

每个结果应附带：

- `source_commit_time`：快照所覆盖的源提交时间。
- `snapshot_time`：明文快照完成时间。
- `lag_ms`：两者之差。
- `fresh/stale/gap`：新鲜度状态。

## 使用

需要 Go 1.25+ 和 macOS。密钥与数据源映射放在 `~/.config/wechat-realtime-reader/`（见上文）。

```bash
go build -o bin/wechat-reader ./cmd/wechat-reader

bin/wechat-reader doctor        # 逐库校验 key（不输出任何密钥）
bin/wechat-reader sync          # 构建/刷新明文快照
bin/wechat-reader serve         # Web 查看 + HTTP MCP + 实时同步
bin/wechat-reader mcp           # stdio MCP（如果没有其他进程在同步，它自己同步）
bin/wechat-reader status        # 每个库的新鲜度
bin/wechat-reader token         # 查看 Web / HTTP MCP 访问令牌
```

### Web 查看

`serve` 默认监听 `127.0.0.1:5678`，启动时打印带 `?token=` 的链接；第一次打开会写入 HttpOnly cookie。

- 左侧：会话列表，支持按名称/备注/微信号/拼音搜索，按私聊/群聊/公众号过滤。
- 会话内筛选：发送者（群成员下拉，或直接点消息上的名字）、关键词、日期范围；筛选条件写在 URL hash 里，可以收藏。
- 跨会话搜索：选择一个联系人，查看 TA 在所有私聊和群里的消息；也可以叠加关键词和日期。
- 新消息通过 SSE 实时推送到页面。
- 图片消息显示缩略图，点击后看原图。只能显示微信已经下载到本机的图片：缩略图基本都有，原图只有在微信里点开过才有。

### 图片

图片在 `msg/attach/<md5(会话)>/<年-月>/Img/<id>[_t|_h].dat`，`<id>` 来自消息的 `packed_info_data`。4.x 的 V2 格式：文件头之后，前 1024 字节是 AES-128-ECB，末尾一段是单字节 XOR，中间是明文。

- AES key = `hex(md5(uin + wxid))[:16]`，XOR key = `uin & 0xff`。首次运行时从一张缩略图暴力推导（2^24 个候选，几秒），结果写入 `image-key.json`，不需要接触微信进程。
- 多数原图是微信自有的 `wxgf`（HEVC）格式，浏览器无法显示。装了 `ffmpeg` 的话会转成 JPEG，并缓存到 vault 的 `cache/img/`（私密目录）；没装就退回显示缩略图。

### MCP

stdio（Claude Code 等本地客户端）：

```bash
claude mcp add wechat -- /path/to/bin/wechat-reader mcp
```

HTTP：`serve` 同时在 `http://127.0.0.1:5678/mcp` 提供 Streamable HTTP，需要 `Authorization: Bearer <token>`。

| 工具 | 作用 |
| --- | --- |
| `list_sessions` | 最近会话、未读数、最后一条消息 |
| `search_contacts` | 按名称/备注/微信号/拼音找联系人、群、公众号 |
| `get_messages` | 读取单个会话，可按发送者、关键词、时间筛选，`before_seq` 翻页 |
| `search_messages` | 跨会话按关键词和/或发送者搜索 |
| `list_chat_members` | 会话里发过言的人及条数 |
| `get_new_messages` | 游标之后的新消息，用于持续跟踪 |
| `get_image` | 返回图片消息的图片（缩略图或原图），Agent 可以直接看图；原图未下载时标注 `THUMBNAIL ONLY` |
| `get_video` | 返回视频消息的封面图和本地 mp4 路径 |
| `get_voice` | 返回语音消息的微信转写、时长和解码后的本地 WAV 路径；`audio=true` 时附带音频 |
| `sync_status` | 每个库的快照新鲜度 |

会话和发送者参数可以传 username，也可以传显示名，会做模糊匹配。时间参数支持 `2006-01-02`、`2006-01-02 15:04`、RFC3339、unix 秒，以及 `24h`、`7d` 这类相对时间。每个结果都带 `freshness`（`fresh`/`stale`、`lag_ms`）。

媒体说明：

- 图片缩略图和视频封面在收到消息时就会存下来；原图和 mp4 是否存在取决于微信是否已下载（私聊基本自动下载，群聊很多要在微信里看过才下载）。
- 语音的微信转写（自动或手动转文字）存在消息的 `packed_info_data` 里，会直接拼进消息正文，可以被关键词搜索到。
- 语音音频存在 `message/media_N.db` 的 `VoiceInfo` 表里，格式为 SILK v3，按需解码成 WAV，缓存在 vault 的 `cache/voice/`（0600）。

同一时刻只有一个进程持有 `state/sync.lock` 负责写快照；其他进程（比如第二个 `mcp`）只读该进程的快照和 `manifest.json`。持锁进程退出后，其他进程会自动接管。

## 项目结构

```text
cmd/wechat-reader/        CLI：serve / mcp / sync / status / doctor / token
internal/config/          私密配置加载、权限检查、key 名称映射
internal/crypto/          逐页 HMAC-SHA512 认证 + AES-256-CBC 解密
internal/wal/             WAL 解析：校验和、generation salt、只取已提交事务
internal/snapshot/        稳定读取、按页 tag 增量、clonefile + rename 原子发布、监视与 manifest
internal/media/           图片 .dat 解码（V1/V2/旧版 XOR）、key 推导、wxgf→JPEG
internal/store/           只读查询：联系人、会话、消息解析（zstd、XML 卡片、引用、系统消息）
internal/mcpserver/       MCP 工具
internal/web/             Web UI（embed）、JSON API、SSE、token 鉴权
```

### 增量刷新

每页尾部 80 字节（IV + HMAC）会原样保留在明文快照的 reserve 区，作为该页的 tag：

1. 主库元数据没变时，只看 WAL 里的页和上次来自 WAL 的页；主库变了（checkpoint）才全量比对。
2. tag 不同的页才解密；tag 相同的页仍然校验 HMAC，篡改照样能发现。
3. 用 APFS `clonefile` 复制当前快照，只写入变化的页，然后 `rename` 原子替换。
4. 重启时直接从已发布快照读回 tag，不需要重新解密。

实测：首次全量 1.3 GB 约 2.7 秒（含 `quick_check`）；单条新消息刷新只涉及几页，约 20–40 ms。

## 必须覆盖的测试

- WAL 大小端、checksum、generation 切换、未提交帧、truncate 和增长。
- 主库/WAL 在读取中变化时不得发布混合快照。
- key/salt 错误、页密文篡改、HMAC 篡改和错误 page number。
- 快照失败时原有可读副本不得被覆盖。
- 多消息分片、空表、压缩正文、撤回/系统消息和 schema 版本变化。
- 任何日志、异常和 manifest 都不得包含 key、salt 全值、wxid 或原始聊天正文。

已实现的测试：`go test ./...` 覆盖 WAL 大小端/校验和/generation/未提交帧/截断、页面篡改/IV/HMAC/错误页号/错误 key，以及快照的全量→无变化→WAL 增量→checkpoint→篡改失败不覆盖→重启复用 tag 全流程（fixture 全部为合成数据）。

## 安全规则

- 源微信目录永远只读，不执行 checkpoint、vacuum、attach 或 pragma 写操作。
- 不对正式 `WeChat.app` 重签名、注入或修改文件。
- 不常驻保存原始 PBKDF2 参数日志；若人工保留，必须位于私密目录且权限 600。
- 不将明文库放在 Projects、Desktop、Downloads、iCloud/Dropbox 或任何 Git 目录。
- 对外 API 只绑定 loopback（非 loopback 地址拒绝启动），强制 token 认证，并校验 Host 头防 DNS rebinding。

