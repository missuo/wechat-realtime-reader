# Full key-acquisition procedure

## Principle and scope

This project reads the WCDB/SQLCipher-style databases of macOS WeChat 4.x. The
verified format is 4096-byte pages, a 16-byte salt, an 80-byte reserve (16-byte
IV + 64-byte HMAC), AES-256-CBC, and HMAC-SHA512.

Hook the entry and exit of the system C function `CCKeyDerivationPBKDF`: at entry,
record the arguments and the output pointer; at exit, copy the result only when
the return value is 0. Do not treat the output pointer itself as the key, and do
not read the not-yet-generated output at entry. The CommonCrypto argument order
is:

```text
algorithm, password, passwordLen, salt, saltLen, prf, rounds, derivedKey, derivedKeyLen
```

This tool handles only PBKDF2 (2) + HMAC-SHA512 (5), a 16-byte salt, a 32-byte
output, and calls with 256000 or 2 rounds. The 256000-round output is a candidate
AES key; 2 rounds is usually for the HMAC key, whose 32-byte input may also be the
AES key. The final identity is decided by the first-page HMAC. The `input` and
`output` in the capture file are both sensitive material.

Each database must be paired independently; a `message_0` key cannot be applied to
other shards. For the same account and database, a key is usually reusable while
the salt/derivation input is unchanged; after a database rebuild, migration,
reinstall, or upgrade it should be re-verified, with no guarantee of permanence.
WeChat may also switch to another derivation implementation; this hook is not on
every version's path.

## 1. First decide whether a capture is needed

From the project root:

```bash
go build -o bin/wechat-reader ./cmd/wechat-reader
bin/wechat-reader doctor
```

If the config exists and every needed database passes, use the existing keys. When
the config is missing, first determine the real account's `db_storage` path:

```text
~/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files/<account-dir>/db_storage
```

Bind the account explicitly; do not guess by directory age. This documentation
step does not require reading or displaying the real key.

## 2. Prepare a same-version temporary copy

Quit the official client normally from the WeChat menu and confirm that the app
and its helper processes have exited. Closing the window is not the same as
quitting. Avoid two clients accessing the same account directory at once.

Run the commands below from the project root, staying in the same terminal to
keep the `capture_work` variable. The example source is `/Applications/WeChat.app`;
adjust to the real location. The copy lives in a temporary directory; the keys are
saved in a long-lived private directory.

```bash
umask 077
capture_work="$(mktemp -d /private/tmp/wechat-db-keys.XXXXXX)"
ditto --noextattr --noqtn /Applications/WeChat.app "$capture_work/WeChat.app"
/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$capture_work/WeChat.app/Contents/Info.plist"
codesign --force --deep --sign - \
  --entitlements skills/wechat-db-keys/references/capture-entitlements.plist \
  "$capture_work/WeChat.app"
codesign --verify --deep --strict "$capture_work/WeChat.app"

python3 -m venv "$capture_work/venv"
"$capture_work/venv/bin/python" -m pip install 'frida==17.25.2'
mkdir -p ~/.config/wechat-realtime-reader/key-capture
chmod 700 ~/.config/wechat-realtime-reader ~/.config/wechat-realtime-reader/key-capture
```

This gives the copy minimal debug entitlements and does not keep the official
sandbox signature. The copy must read the same user's account data to derive the
matching key; **do not set a blank HOME/CFFIXED_USER_HOME and expect to obtain old
database keys**. System privacy permissions may still block the read; if so, stop
and inspect the error — do not disable SIP or modify the official app.

Copy-signing keeps the official app's files and signature, but the copy process
has a different signing identity and may still be flagged by risk-control. This
Mac's debugging history has seen login restrictions; do not make this flow a
day-to-day way of running.

## 3. Install the listener before launch

Do not double-click the copy, and do not launch first and attach later. Run:

```bash
"$capture_work/venv/bin/python" skills/wechat-db-keys/scripts/keys.py capture \
  --app "$capture_work/WeChat.app" \
  --seconds 30 \
  --output ~/.config/wechat-realtime-reader/key-capture/candidates-new.jsonl
```

The script's order is suspend launch, attach, load the hook, receive `ready`,
resume launch. It resumes the app only after the terminal shows `Hook ready`.
Captured values are written only to a 0600 file; the terminal shows only counts.
Default 30 s, 120 s maximum; on timeout or Ctrl-C it kills the main process it
launched.

With a saved login state, the database-open stage usually triggers derivation;
some databases are lazy-loaded and require the user to enter the relevant chat. If
a QR/confirmation page appears, the user must complete it; the script does not
confirm on their behalf. Ctrl-C as soon as there is enough; on a restricted login,
security prompt, or crash, end this maintenance and do not keep retrying.

Do not `tee` the app output or use a Frida console script that prints keys. This
tool suppresses the app console output via `stdio='pipe'`. Afterwards, also check
the copy's helper processes:

```bash
ps -axo pid=,command= | rg -F "$capture_work/WeChat.app/"
```

If any remain, verify each PID against the output and `kill -TERM <PID>`, then
check again. Only touch processes under this run's temporary directory.

## 4. Verify offline and pair

Replace the account-directory placeholder below; do not use this example's literal
value:

```bash
"$capture_work/venv/bin/python" skills/wechat-db-keys/scripts/keys.py pair \
  --capture ~/.config/wechat-realtime-reader/key-capture/candidates-new.jsonl \
  --db-root "$HOME/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files/<account-dir>/db_storage" \
  --output ~/.config/wechat-realtime-reader/keys-new.json
```

The authentication algorithm matches `internal/crypto/crypto.go`:

```text
salt = page1[0:16]
macSalt = salt XOR 0x3A
macKey = PBKDF2-HMAC-SHA512(candidateAESKey, macSalt, rounds=2, len=32)
expected = HMAC-SHA512(macKey, page1[16:4032] || LE32(1))
compare expected with page1[4032:4096] using constant-time comparison
```

Only a database with exactly one candidate passing authentication is written to
the result. Candidates are de-duplicated by value, not guessed by capture order;
the output key is a path relative to `db_storage`, e.g. `message/message_0.db`. A
missing database is never faked into a success; check whether the chat-essential
`contact/contact.db`, `session/session.db`, and every actually-present
`message/message_N.db` are paired. `pair` reads only the main-db first page and
does no WAL snapshot merge; keeping the client quit after the capture helps stable
verification.

The script prints only the match count, never a key. The file format is the flat
JSON the project accepts directly:

```json
{
  "contact/contact.db": "<64-hex-key>",
  "session/session.db": "<64-hex-key>",
  "message/message_0.db": "<64-hex-key>"
}
```

## 5. Wire into the reader

If `keys.json` already exists, back it up privately first, then merge the
authenticated entries from `keys-new.json` into it. Do not overwrite a complete
old mapping with an incomplete new result; if the old file already has an entry of
the same name, choose after authenticating against the current database. Keep the
backup at 0600 and out of the project.

For a first-time setup where `keys.json` does not yet exist, use
`cp -n ~/.config/wechat-realtime-reader/keys-new.json ~/.config/wechat-realtime-reader/keys.json`,
then check that it was actually created. The `source-config.json` (0600) format
is:

```json
{
  "wxid": "<account-dir-name>",
  "db_base_path": "~/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files/<account-dir>/db_storage",
  "vault_dir": "~/.local/share/wechat-realtime-reader"
}
```

When `decrypted_dir` is unspecified, `vault_dir/decrypted/current` is used. Do not
place the plaintext directory in Projects, Desktop, Downloads, or a sync drive.
When done:

```bash
chmod 600 ~/.config/wechat-realtime-reader/keys.json ~/.config/wechat-realtime-reader/source-config.json
go build -o bin/wechat-reader ./cmd/wechat-reader
bin/wechat-reader doctor
```

`doctor` tries derived/raw mode per database and verifies the first page. The new
tool pairs and saves the derived AES key, so the 256000-round derivation is not
needed again. To verify all databases, run `bin/wechat-reader sync`, which
produces a private plaintext snapshot. When done, quit the temporary copy and
reopen the official WeChat; day-to-day `serve`/`mcp` does not load Frida.

## 6. Cleanup and retention

After confirming all processes under the temporary directory have exited, move
**the exact directory** shown by the terminal variable `capture_work` to the
Trash; what is deleted is the temporary app and venv. Do not bulk-clean with vague
wildcards, and do not delete the official app or account data.

This project's user has asked to keep all key-related material: keep `keys.json`,
old backups, `keys-new.json`, `source-config.json`, and `key-capture/`; the raw
candidate file also contains keys, so keep it at 0600. Do not delete them unless
the user explicitly asks.

## Common failures

| Symptom | Check and handling |
| --- | --- |
| `Operation failed (FileExistsError)` | The tool refuses to overwrite an existing output; use a new filename and keep the old file. |
| No `Hook ready` | The hook did not install; the tool does not resume launch and cleans up the main process. Check the Frida version, exported symbols, signature, and debug entitlements. |
| `ObjC is not defined` | Frida 17 split the bridge out of GumJS. This script calls only the C API and needs no ObjC; check whether a different old script was used by mistake. |
| 0 captures | Confirm it was not launch-then-attach; check the login state, the real account directory, whether the process exited, and whether the database is lazy-loaded. If the version changed its derivation implementation, stop and analyze the new one — do not conclude the key was destroyed. |
| Captures but 0 pairings | Possibly only MAC outputs were captured, the wrong account was read, the format changed, or the key rotated. Re-check the first-page layout; do not skip the HMAC check. |
| Some databases missing | A capture covers only the opened databases; decide whether the realtime-chat-essential databases are complete before deciding on another maintenance pass. |
| `PermissionError` / cannot attach | Check the copy's signature, the Python/Frida architecture, and privacy permissions. Do not re-sign the official app, auto-use sudo, or turn off system protection. |
| Risk-control, security prompt, restricted login | End this run immediately; restore the official client and do not loop retries. |

## Verification status and basis

The existing project saved first-page-verified keys on WeChat 4.2.0.16 and
verified new-message read-back. New scripts in this skill should be reported
separately against the current commit's offline tests and the actual maintenance
record; do not claim "on-device capture succeeded" without a real launch
verification.

2026-11-04: the new tool passes 3 offline synthetic tests covering the correct
key, a wrong key, salt/ciphertext/IV/HMAC tampering, candidate de-duplication,
file permissions, refusing to overwrite, and refusing output outside a private
directory; this run did not launch WeChat to verify the new capture script. Re-run
from the project root:

```bash
python3 -m unittest discover -s skills/wechat-db-keys/scripts -p 'test_*.py' -v
```

- [Frida JavaScript API](https://frida.re/docs/javascript-api/): Module,
  Interceptor, send, NativePointer APIs.
- [Frida 17 release notes](https://frida.re/news/2025/05/17/frida-17-0-0-released/):
  runtime-bridge split and API changes.
- Project crypto implementation: `internal/crypto/crypto.go`; config definitions:
  `internal/config/config.go`.
