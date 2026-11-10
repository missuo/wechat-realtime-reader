---
name: wechat-db-keys
description: Acquire, verify, and update the local database keys for macOS WeChat 4.x used by this project, using a temporary app copy and a pre-launch Frida PBKDF2 listener. For first-time setup of the owner's own account, or maintenance after an old key fails verification.
---

# WeChat database key maintenance

Read [references/capture.md](references/capture.md) first, then run the matching
steps. The project root is two levels above this skill; all paths are relative to
the actual working directory.

## Decisions and done criteria

1. Run `bin/wechat-reader doctor` first. If existing keys pass, reuse them; do not
   start a capture when the user only asked for documentation.
2. Capture targets the user's own logged-in local account. Confirm the app
   version, the account database path, and that the official WeChat has fully
   quit, then prepare a same-version temporary copy. Do not modify the official
   app's signature.
3. Follow `spawn suspended → attach → load hook → confirm ready → resume`.
   Attaching to an already-running process easily misses the derivation that
   happens at startup.
4. Use `scripts/keys.py capture`, 120 s maximum; Ctrl-C as soon as enough is
   captured. This makes the copy read the same account's real data, so it must
   not run alongside the official WeChat, and no HOME isolation should be claimed.
   Do not re-scan the QR code or loop on login attempts.
5. Use `scripts/keys.py pair` to pair offline. Save only the 32-byte values that
   pass the database first-page HMAC-SHA512 check. A matching capture count or
   salt is no substitute for authentication.
6. After updating the config, run `doctor`; run `sync` only when full-decryption
   verification is needed. At the end, confirm the temporary main process and its
   helpers have all exited, then bring the official WeChat back.

## Tools and limits

- `capture`: hooks only the system CommonCrypto C function, with no dependency on
  the Objective-C bridge; candidate values are written to a private JSONL and the
  terminal shows only counts. On a hook-load error it does not resume app launch.
- `pair`: reads the `.db` first page and computes the authentication code without
  opening a writable SQLite connection; it writes to a new, non-existent file by
  default to avoid overwriting existing keys.
- The full steps, parameter meanings, config format, error localization, cleanup,
  and verification status are in the reference file.
- Real keys, candidate logs, account mappings, and plaintext databases stay in
  private directories such as `~/.config/wechat-realtime-reader` (dir 0700, files
  0600). They must not be copied into the project or printed to chat.
- This project previously completed real first-page and realtime-message
  verification on macOS / WeChat 4.2.0.16; new scripts added by this skill must
  distinguish offline verification from on-device verification, and must not
  report prior results as the new scripts' measured results.
- Copy-signing keeps the official app's files but does not remove account
  risk-control or system-permission changes; this Mac's debugging history has
  seen login restrictions before. On a security prompt, restricted login, or
  crash, stop that run immediately.
