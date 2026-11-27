#!/bin/sh
set -eu

# Available through both core-base and thick-prep's existing lib/ COPY.
exec node - "$@" <<'JS'
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const [version, url, sha256, integrity, ...flags] = process.argv.slice(2);
let directory;
try {
  if (!/^\d+(?:\.\d+){1,3}$/.test(version || '') || /\s/.test(version) ||
      (sha256 || '').length !== 64 || !/^[a-fA-F0-9]{64}$/.test(sha256 || '')) {
    throw new Error('npm pins require a concrete version and SHA256');
  }
  const parsed = new URL(url);
  if (parsed.protocol !== 'https:' || parsed.username || parsed.password || parsed.hash) {
    throw new Error('npm pin URL must be HTTPS without credentials or fragments');
  }
  const entries = (integrity || '').split(/\s+/);
  const algorithm = ['sha512', 'sha384', 'sha256'].find(a =>
    entries.some(e => e.startsWith(a + '-')));
  const encoded = algorithm && entries.find(e => e.startsWith(algorithm + '-')).slice(algorithm.length + 1);
  const expected = encoded && Buffer.from(encoded, 'base64');
  if (!expected || expected.toString('base64') !== encoded ||
      expected.length !== Number(algorithm.slice(3)) / 8) {
    throw new Error('npm pins require valid SHA2 integrity');
  }
  directory = fs.mkdtempSync(path.join(os.tmpdir(), 'scion-npm-pin-'));
  const archive = path.join(directory, 'package.tgz');
  const download = spawnSync('curl', ['--proto', '=https', '--proto-redir', '=https',
    '-fsSL', url, '-o', archive], {stdio: 'inherit'});
  if (download.error || download.status !== 0) throw new Error('npm archive download failed');
  const bytes = fs.readFileSync(archive);
  if (crypto.createHash('sha256').update(bytes).digest('hex') !== sha256.toLowerCase() ||
      !crypto.createHash(algorithm).update(bytes).digest().equals(expected)) {
    throw new Error('npm archive SHA256/integrity mismatch');
  }
  const install = spawnSync('npm', ['install', '-g', ...flags, archive], {stdio: 'inherit'});
  if (install.error || install.status !== 0) throw new Error('verified npm archive installation failed');
} catch (error) {
  console.error('Pinned npm install refused: ' + error.message);
  process.exitCode = 1;
} finally {
  if (directory) fs.rmSync(directory, {recursive: true});
}
JS
