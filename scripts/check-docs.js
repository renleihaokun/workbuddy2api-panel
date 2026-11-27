const fs = require('fs');
const path = require('path');
const root = path.resolve(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');
const exists = (rel) => fs.existsSync(path.join(root, rel));
const plain = (s) => s.replace(/\r\n/g, '\n');

const failures = [];
const oks = [];
const ok = (msg) => oks.push(msg);
const fail = (msg) => failures.push(msg);
const check = (cond, msg) => (cond ? ok(msg) : fail(msg));

const readme = plain(read('README.md'));
let config;
try { config = JSON.parse(read('config.example.json')); }
catch (err) { fail('config.example.json 不是合法 JSON：' + err.message); }
if (config) {
  const main = read('cmd/server/main.go');
  const mobileCss = read('internal/panel/mobile.css');
  const mobileJs = read('internal/panel/mobile.js');
  const version = (main.match(/const appVersion = "([^"]+)"/) || [])[1];
  check(Boolean(version), 'cmd/server/main.go 存在 appVersion');
  if (version) ok(`appVersion = ${version}`);

  check(!readme.includes('当前版本 `1.24.3-panel`'), 'README 不写死旧版本 1.24.3');
  check(!readme.includes('共 19 个提交'), 'README 不维护已过期的提交总数');
  check(!readme.includes('workbuddy2api-panel:1.17.6'), 'README 不以旧镜像 1.17.6 作示例');
  check(!readme.includes('git clone https://github.com/linguo2625469/workbuddy2api-panel.git'), 'README Compose 不克隆冻结上游');
  check(readme.includes('git clone https://github.com/renleihaokun/workbuddy2api-panel.git'), 'README Compose 指向本 fork');
  check(!readme.includes('无网关侧请求体上限') && !readme.includes('网关不再设请求体上限'), 'README 不再声称请求体无上限');
  check(readme.includes('64 MiB') && readme.includes('413 request_too_large'), 'README 明确 64 MiB 安全网与 413');
  check(readme.includes('### 手机端（≤1000px）'), 'README 手机断点为 1000px');
  check(!readme.includes('≤760px'), 'README 不再残留 760px 断点');
  check(readme.includes('deploy-wb2api.sh'), 'README 引用已提交的部署脚本');
  check(exists('deploy-wb2api.sh'), 'deploy-wb2api.sh 已存在于仓库');
  check(/max-width:\s*1000px/.test(mobileCss), 'mobile.css 使用 1000px 断点');
  check(mobileJs.includes("matchMedia('(max-width: 1000px)')"), 'mobile.js 使用 1000px 断点');

  const has = (obj, key) => obj && Object.prototype.hasOwnProperty.call(obj, key);
  check(has(config.upstream, 'identity_mode'), 'config.example.json 含 upstream.identity_mode');
  check(has(config.pool, 'verify_session_on_401'), 'config.example.json 含 pool.verify_session_on_401');
  check(has(config.pool, 'session_verify_interval'), 'config.example.json 含 pool.session_verify_interval');
  check(has(config.schedule, 'jitter_minutes'), 'config.example.json 含 schedule.jitter_minutes');
  check(config.pool && config.pool.batch_concurrency === 1, 'config.example.json 默认 pool.batch_concurrency 为 1');
  check(has(config.pool, 'batch_delay_min_ms') && has(config.pool, 'batch_delay_max_ms'), 'config.example.json 含 pool.batch_delay_min_ms/max_ms');
  check(has(config.server, 'read_timeout'), 'config.example.json 含 server.read_timeout');
  check(config.api_key === '', 'config.example.json 的 api_key 保持空串占位（不是真实密钥）');
}

for (const rel of [
  'AGENTS.md',
  'docs/README.md',
  'docs/getting-started.md',
  'docs/configuration.md',
  'docs/deployment.md',
  'docs/operations.md',
  'docs/api.md',
  'docs/development.md',
  'docs/releases.md',
  'scripts/check-docs.js',
  'deploy-wb2api.sh',
]) check(exists(rel), `文档 / 脚本入口存在：${rel}`);

const textFiles = [];
function walk(relDir, filter) {
  for (const ent of fs.readdirSync(path.join(root, relDir), { withFileTypes: true })) {
    if (ent.name === '.git' || ent.name === 'node_modules' || ent.name === '.tmp') continue;
    const rel = path.posix.join(relDir, ent.name);
    if (ent.isDirectory()) walk(rel, filter);
    else if (!filter || filter(rel)) textFiles.push(rel);
  }
}
walk('.', (rel) => /\.(md|ya?ml|json|go|sh|py|js)$/.test(rel));
for (const rel of textFiles) {
  const s = read(rel);
  if (/48\.218\.187\.173/.test(s)) fail(`${rel} 含真实服务器地址`);
  if (/C:\\Users\\chaog|A:\\文件（A）/.test(s)) fail(`${rel} 含本机绝对路径`);
  if (/BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY/.test(s)) fail(`${rel} 含私钥块`);
  if (/sk-[A-Za-z0-9]{24,}/.test(s)) fail(`${rel} 含疑似真实密钥`);
}
ok('敏感值扫描完成');

const mdFiles = ['README.md', 'AGENTS.md'];
function collectMd(relDir) {
  for (const ent of fs.readdirSync(path.join(root, relDir), { withFileTypes: true })) {
    const rel = path.posix.join(relDir, ent.name);
    if (ent.isDirectory()) collectMd(rel);
    else if (ent.name.endsWith('.md')) mdFiles.push(rel);
  }
}
collectMd('docs');
for (const rel of mdFiles) {
  if (!exists(rel)) continue;
  const s = read(rel);
  const links = [...s.matchAll(/\[[^\]]+\]\(([^)]+)\)/g)].map((m) => m[1]);
  for (const href of links) {
    if (/^(https?:\/\/|#|mailto:)/.test(href)) continue;
    const filePart = decodeURIComponent(href.split('#')[0]);
    if (!filePart) continue;
    const target = path.resolve(path.dirname(path.join(root, rel)), filePart);
    if (!fs.existsSync(target)) fail(`${rel} 的本地链接不存在：${href}`);
  }
}
ok('Markdown 本地链接检查完成');

for (const msg of oks) console.log(`ok - ${msg}`);
if (failures.length) {
  console.error('');
  for (const msg of failures) console.error(`FAIL - ${msg}`);
  console.error(`\n${failures.length} 项检查失败`);
  process.exit(1);
}
console.log(`\n全部 ${oks.length} 项检查通过`);