// Used only by disposable package-install test users. Never point seed at a
// real profile: exclusive creation refuses to overwrite existing user files.
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { isMainModule } from './is-main-module.mjs';

const entries = [
  '.super-cage/config.json',
  '.super-cage/environment.json',
  '.super-cage/account.json',
  '.super-cage/password-vault.json',
  '.super-cage/favorites.json',
  '.super-cage/projects.json',
  '.super-cage/workbench.json',
  '.super-cage/appearance.json',
  '.super-cage/webview2/Default/Local Storage/leveldb/000001.log',
  '.super-cage/webkit/data/WebsiteData/LocalStorage/storage.sqlite3',
  '.super-cage/state/lima/persistent/disk',
  '.super-cage-workspace/project.txt',
];
const digest = (data) => createHash('sha256').update(data).digest('hex');

export async function seed(home, manifest) {
  const records = {};
  for (const name of entries) {
    const path = join(home, name);
    await mkdir(dirname(path), { recursive: true, mode: 0o700 });
    const bytes = Buffer.from(`SuperCage package test: preserve ${name}\n历史、收藏与登录数据\n\0\xff`, 'utf8');
    await writeFile(path, bytes, { flag: 'wx', mode: 0o600 });
    records[name] = digest(bytes);
  }
  await writeFile(manifest, JSON.stringify(records), { flag: 'wx', mode: 0o600 });
}

export async function verify(home, manifest) {
  const records = JSON.parse(await readFile(manifest, 'utf8'));
  for (const [name, expected] of Object.entries(records)) {
    if (digest(await readFile(join(home, name))) !== expected) {
      throw new Error(`Installation changed personal data: ${name}`);
    }
  }
  return Object.keys(records).length;
}

if (isMainModule(import.meta.url)) {
  const [operation, home, manifest] = process.argv.slice(2);
  if (!home || !manifest || !['seed', 'verify'].includes(operation)) {
    throw new Error('Usage: personal-data-fixture.mjs seed|verify TEST_HOME MANIFEST');
  }
  if (operation === 'seed') await seed(home, manifest);
  else console.log(`PASS: ${await verify(home, manifest)} personal data files preserved byte for byte.`);
}
