import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { MARKET_ARCHIVE_PREFIX, MARKET_ICON_PREFIX, MARKET_INDEX_URL, normalizeMarketIndex } from './plugins-market.ts';

test('forked index resolves inherited assets without changing integrity metadata', () => {
  const entry = {
    name: 'custom-tool', version: '1.0.0', bytes: 123, sha256: 'a'.repeat(64),
    archive: 'https://raw.githubusercontent.com/Lee-xeo/cage-plugins/main/dist/custom-tool-1.0.0.tar.gz',
    icon: 'https://raw.githubusercontent.com/Lee-xeo/cage-plugins/main/plugins/custom-tool/icon.svg',
  };
  const [plugin] = normalizeMarketIndex({ version: 1, plugins: [entry] });
  assert.equal(plugin.packages[0].archive, MARKET_ARCHIVE_PREFIX + 'custom-tool-1.0.0.tar.gz');
  assert.equal(plugin.icon, MARKET_ICON_PREFIX + 'custom-tool/icon.svg');
  assert.equal(plugin.packages[0].sha256, entry.sha256);
  assert.equal(plugin.packages[0].bytes, entry.bytes);
  assert.equal(normalizeMarketIndex({ version: 1, plugins: [{ ...entry, archive: 'https://example.com/tool.tar.gz' }] }).length, 0);
  assert.ok(entry.archive.includes('Lee-xeo/'), 'must not mutate the source index');
});

test('bundled snapshot and market endpoints use the fork', () => {
  assert.equal(MARKET_INDEX_URL, 'https://raw.githubusercontent.com/larito001/cage-plugins/main/index.json');
  const raw = JSON.parse(readFileSync(new URL('../data/plugins-market.json', import.meta.url), 'utf8'));
  const plugins = normalizeMarketIndex(raw);
  assert.equal(plugins.length, new Set(raw.plugins.map(entry => entry.name)).size);
  for (const plugin of plugins) {
    assert.ok(plugin.repoUrl.startsWith('https://github.com/larito001/cage-plugins/'));
    for (const pkg of plugin.packages) assert.ok(pkg.archive.startsWith(MARKET_ARCHIVE_PREFIX));
  }
});
