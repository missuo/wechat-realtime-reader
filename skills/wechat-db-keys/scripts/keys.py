#!/usr/bin/env python3
"""Capture PBKDF candidates privately, then pair them by page-one HMAC.

No dependency except frida for the capture subcommand. Does not decrypt/export chats.
"""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import signal
import struct
import sys
import threading
import time

HOOK = r"""
'use strict';
function hex(p, n) {
  return Array.from(new Uint8Array(p.readByteArray(n)),
    b => b.toString(16).padStart(2, '0')).join('');
}
const target = Module.getGlobalExportByName('CCKeyDerivationPBKDF');
Interceptor.attach(target, {
  onEnter(args) {
    this.record = null;
    // CommonCrypto: PBKDF2=2, HMAC-SHA512=5, salt length=16.
    if (args[0].toUInt32() !== 2 || args[5].toUInt32() !== 5 ||
        args[4].toUInt32() !== 16 || args[8].toUInt32() !== 32) return;
    const rounds = args[6].toUInt32();
    if (rounds !== 256000 && rounds !== 2) return;
    try {
      this.record = {type: 'candidate', rounds: rounds,
        salt: hex(args[3], 16), input: null};
      // The 2-round MAC derivation's input can itself be the AES key.
      if (args[2].toUInt32() === 32) this.record.input = hex(args[1], 32);
      this.output = args[7];
    } catch (_) { this.record = null; }
  },
  onLeave(retval) {
    if (this.record === null || retval.toInt32() !== 0) return;
    try {
      this.record.output = hex(this.output, 32);
      send(this.record);
    } catch (_) {}
  }
});
Interceptor.flush();
send({type: 'ready'});
"""


def private_parent(path):
    path = path.expanduser().absolute()
    # Secret artifacts may only be placed under the dedicated private config dir.
    base = (Path.home() / '.config/wechat-realtime-reader').resolve()
    parent = path.parent.resolve()
    if parent != base and base not in parent.parents:
        raise ValueError('output must be inside ~/.config/wechat-realtime-reader')
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    for directory in (base, parent):
        if directory.stat().st_mode & 0o077:
            raise ValueError('secret directory permissions must be 0700')
    return path


def capture(args):
    import frida
    app = args.app.expanduser().resolve()
    binary = app / 'Contents/MacOS/WeChat'
    if not binary.is_file() or app == Path('/Applications/WeChat.app'):
        raise ValueError('provide a temporary WeChat.app copy, not the official app')
    output = private_parent(args.output)
    device = frida.get_local_device()
    pid = None
    session = None
    ready, failed, stopped = threading.Event(), threading.Event(), threading.Event()
    count = 0
    old_handlers = {}
    for sig in (signal.SIGINT, signal.SIGTERM):
        old_handlers[sig] = signal.signal(sig, lambda *_: stopped.set())
    try:
        # Exclusive creation: never truncate a previous capture.
        fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as log:
            def on_message(message, _data):
                nonlocal count
                if message.get('type') == 'error':
                    failed.set()
                    print('Hook failed; stopping (details suppressed).', file=sys.stderr)
                    return
                payload = message.get('payload', {})
                if payload.get('type') == 'ready':
                    ready.set()
                elif payload.get('type') == 'candidate':
                    log.write(json.dumps(payload) + '\n')
                    log.flush()
                    count += 1
                    print(f'Captured candidates: {count}', flush=True)

            pid = device.spawn([str(binary)], stdio='pipe')
            session = device.attach(pid)
            session.on('detached', lambda *_: stopped.set())
            script = session.create_script(HOOK)
            script.on('message', on_message)
            script.load()
            if not ready.wait(5) or failed.is_set() or stopped.is_set():
                raise RuntimeError('hook was not ready; app was not resumed')
            print('Hook ready; resuming temporary WeChat. Ctrl-C stops capture and app.', flush=True)
            device.resume(pid)
            deadline = time.monotonic() + args.seconds
            while time.monotonic() < deadline and not stopped.wait(0.1):
                if failed.is_set():
                    break
            if failed.is_set():
                raise RuntimeError('capture hook failed')
    finally:
        # Kill before detach so the copy cannot keep running uninstrumented.
        if pid is not None:
            try:
                device.kill(pid)
            except frida.ProcessNotFoundError:
                pass
        if session is not None:
            try:
                session.detach()
            except frida.InvalidOperationError:
                pass
        for sig, handler in old_handlers.items():
            signal.signal(sig, handler)
    print(f'Capture ended: {count} candidates. Check for remaining helper processes.')


def authenticates(key, page):
    if len(page) != 4096 or len(key) != 32:
        return False
    mac_salt = bytes(b ^ 0x3a for b in page[:16])
    mac_key = hashlib.pbkdf2_hmac('sha512', key, mac_salt, 2, 32)
    actual = hmac.new(mac_key, page[16:4032] + struct.pack('<I', 1), 'sha512').digest()
    return hmac.compare_digest(actual, page[4032:4096])


def pair(args):
    candidates = set()
    with args.capture.expanduser().open() as log:
        for line in log:
            record = json.loads(line)
            for field in ('input', 'output'):
                value = record.get(field)
                if isinstance(value, str) and len(value) == 64:
                    candidates.add(bytes.fromhex(value))
    root = args.db_root.expanduser().resolve(strict=True)
    result, missing = {}, 0
    for db in sorted(root.rglob('*.db')):
        if db.is_symlink():
            continue
        with db.open('rb') as source:
            page = source.read(4096)
        if page.startswith(b'SQLite format 3\0'):
            continue
        hits = [key for key in candidates if authenticates(key, page)]
        name = db.relative_to(root).as_posix()
        if len(hits) == 1:
            result[name] = hits[0].hex()
        else:
            missing += 1
    if not result:
        raise ValueError('no authenticated matches; existing keys were not changed')
    output = private_parent(args.output)
    fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as dest:
        json.dump(result, dest, indent=2)
        dest.write('\n')
    print(f'Authenticated databases: {len(result)}; unmatched/ambiguous: {missing}.')
    print('New key file written privately; existing keys.json was not changed.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    cap = commands.add_parser('capture', help='run a temporary app with a startup hook')
    cap.add_argument('--app', type=Path, required=True)
    cap.add_argument('--output', type=Path, required=True)
    cap.add_argument('--seconds', type=int, default=30, choices=range(1, 121), metavar='1..120')
    cap.set_defaults(run=capture)
    match = commands.add_parser('pair', help='authenticate candidates against database first pages')
    match.add_argument('--capture', type=Path, required=True)
    match.add_argument('--db-root', type=Path, required=True)
    match.add_argument('--output', type=Path, required=True)
    match.set_defaults(run=pair)
    args = parser.parse_args()
    os.umask(0o077)
    args.run(args)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Never echo exception contents: a library may include secret input.
        print(f'Operation failed ({type(error).__name__}); see Skill troubleshooting.', file=sys.stderr)
        sys.exit(1)
