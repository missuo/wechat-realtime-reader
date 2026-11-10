"""Offline synthetic fixtures only; never starts WeChat or reads real keys."""
import argparse
import contextlib
import hashlib
import hmac
import io
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

import keys


class KeyTests(unittest.TestCase):
    def setUp(self):
        self.key = bytes(range(32))
        self.page = bytearray((i % 251 for i in range(4096)))
        salt = bytes(x ^ 0x3a for x in self.page[:16])
        mac_key = hashlib.pbkdf2_hmac('sha512', self.key, salt, 2, 32)
        self.page[4032:] = hmac.new(
            mac_key, self.page[16:4032] + struct.pack('<I', 1), 'sha512'
        ).digest()

    def test_authentication_rejects_tampering_and_wrong_key(self):
        self.assertTrue(keys.authenticates(self.key, self.page))
        self.assertFalse(keys.authenticates(bytes(32), self.page))
        for offset in (0, 16, 4016, 4032, 4095):
            altered = bytearray(self.page)
            altered[offset] ^= 1
            self.assertFalse(keys.authenticates(self.key, altered))
        self.assertFalse(keys.authenticates(self.key, self.page[:-1]))

    def test_pair_deduplicates_and_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            db_root = root / 'db_storage'
            (db_root / 'message').mkdir(parents=True)
            (db_root / 'message/message_0.db').write_bytes(self.page)
            (db_root / 'message/message_1.db').write_bytes(bytes(4096))
            log = root / 'candidates.jsonl'
            record = {'input': self.key.hex(), 'output': bytes(32).hex()}
            log.write_text((json.dumps(record) + '\n') * 2)
            output = root / 'result.json'
            args = argparse.Namespace(capture=log, db_root=db_root, output=output)
            with patch.object(keys, 'private_parent', side_effect=lambda p: p):
                stdout = io.StringIO()
                with contextlib.redirect_stdout(stdout):
                    keys.pair(args)
                self.assertNotIn(self.key.hex(), stdout.getvalue())
                self.assertEqual(json.loads(output.read_text()),
                                 {'message/message_0.db': self.key.hex()})
                self.assertEqual(output.stat().st_mode & 0o777, 0o600)
                with self.assertRaises(FileExistsError):
                    keys.pair(args)
                self.assertEqual(json.loads(output.read_text())['message/message_0.db'], self.key.hex())

    def test_secret_output_rejected_outside_private_config(self):
        with tempfile.TemporaryDirectory() as temp:
            target = Path(temp) / 'must-not-create' / 'result.json'
            with self.assertRaises(ValueError):
                keys.private_parent(target)
            self.assertFalse(target.parent.exists())


if __name__ == '__main__':
    unittest.main()
