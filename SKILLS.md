# Project Skills

## Acquiring the WeChat database key

Entry point: [skills/wechat-db-keys/SKILL.md](skills/wechat-db-keys/SKILL.md).

Covers signing a temporary copy, hooking `CCKeyDerivationPBKDF` before Frida
launch, pairing candidate keys against the database first-page HMAC, wiring the
result into this project, and the cleanup steps. The companion tooling is under
that skill's `scripts/`.

This is a one-off maintenance procedure to run when needed. Day-to-day reading
uses the saved key and the official WeChat; see [README.md](README.md) for the
exact commands.
