# 项目 Skills

## 获取微信数据库 Key

入口：[skills/wechat-db-keys/SKILL.md](skills/wechat-db-keys/SKILL.md)。

包含临时副本签名、Frida 启动前监听 `CCKeyDerivationPBKDF`、候选 Key 与数据库首页 HMAC 配对、接入本项目以及清理步骤。配套工具在该 Skill 的 `scripts/` 下。

这是需要时执行的一次维护流程。日常读取使用已保存的 Key 和官方微信，具体命令见 [README.md](README.md)。
