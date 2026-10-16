// Screenshot crawl of every dashboard route. Usage: node crawl.cjs <outdir>
const { chromium } = require('/opt/node22/lib/node_modules/playwright');
const fs = require('fs');
const ids = JSON.parse(fs.readFileSync(__dirname + '/ids.json', 'utf8'));
const OUT = process.argv[2] || __dirname + '/shots';
const BASE = 'http://127.0.0.1:5173';
const pick = (arr, n) => arr.find(x => x.name.includes(n)) || arr[0];
const routes = [
  ['home', '/'], ['customers', '/customers'], ['subscriptions', '/subscriptions'], ['invoices', '/invoices'],
  ['usage', '/usage'], ['pricing', '/pricing'], ['recipes', '/recipes'], ['credits', '/credits'],
  ['credit-notes', '/credit-notes'], ['dunning', '/dunning'], ['dunning-policies', '/dunning-policies'],
  ['webhooks', '/webhooks'], ['webhook-events', '/webhooks/events'], ['api-keys', '/api-keys'],
  ['test-clocks', '/test-clocks'], ['provider-costs', '/provider-costs'], ['audit-log', '/audit-log'],
  ['settings', '/settings'], ['not-found', '/nope'],
  ...ids.customers.map(c => [`customer-${c.name.replace(/_\d+$/, '')}`, `/customers/${c.id}`]),
  ...ids.invoices.map(c => [`invoice-${c.name}`, `/invoices/${c.id}`]),
  ...ids.subscriptions.map(c => [`subscription-${c.name.replace(/-\d+$/, '')}`, `/subscriptions/${c.id}`]),
  ...ids.plans.map(c => [`plan-${c.name}`, `/plans/${c.id}`]),
  ...ids.meters.map(c => [`meter-${c.name}`, `/meters/${c.id}`]),
  ...ids.clocks.map(c => [`clock-${c.name}`, `/test-clocks/${c.id}`]),
];
const publicRoutes = [['login', '/login'], ['forgot-password', '/forgot-password'], ...(ids.public || [])];
const variants = [
  { tag: 'desktop-light', viewport: { width: 1440, height: 900 }, theme: 'light' },
  { tag: 'desktop-dark', viewport: { width: 1440, height: 900 }, theme: 'dark' },
  { tag: 'mobile-light', viewport: { width: 390, height: 844 }, theme: 'light' },
];
(async () => {
  const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' in process.env ? undefined : undefined });
  fs.mkdirSync(OUT, { recursive: true });
  // Sign in once through the real login form.
  const ctx0 = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const p0 = await ctx0.newPage();
  for (const [name, path] of publicRoutes) {
    await p0.goto(BASE + path, { waitUntil: 'networkidle' });
    await p0.waitForTimeout(800);
    await p0.screenshot({ path: `${OUT}/${name}__desktop-light.png`, fullPage: true });
  }
  await p0.goto(BASE + '/login', { waitUntil: 'networkidle' });
  await p0.fill('input[type=email]', 'ops@acme.test');
  await p0.fill('input[type=password]', 'Velox-walk-2026!');
  await p0.click('button[type=submit]');
  await p0.waitForURL(u => !u.pathname.startsWith('/login'), { timeout: 15000 });
  const state = await ctx0.storageState();
  await ctx0.close();
  const errors = [];
  for (const v of variants) {
    const ctx = await browser.newContext({ viewport: v.viewport, storageState: state, colorScheme: v.theme });
    await ctx.addInitScript(t => { try { localStorage.setItem('velox-theme', t) } catch {} }, v.theme);
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') errors.push(`${v.tag} ${page.url()} ${m.text().slice(0, 200)}`) });
    for (const [name, path] of routes) {
      // Mobile and dark: list pages + one detail of each kind, to keep the set reviewable.
      if (v.tag !== 'desktop-light' && /^(invoice|subscription|plan|meter|clock)-/.test(name) && !/(VLX-000001|VLX-000006|globex-pro|initech|pro$|api_calls|globex-history)/.test(name)) continue;
      try {
        await page.goto(BASE + path, { waitUntil: name === 'webhook-events' ? 'load' : 'networkidle', timeout: 30000 });
        await page.waitForTimeout(name === 'webhook-events' ? 4000 : 1200);
        // The app scrolls inside <main>; let it grow so fullPage captures everything.
        await page.addStyleTag({ content: '.h-screen{height:auto!important;min-height:100vh} main{overflow:visible!important;height:auto!important;max-height:none!important}' });
        await page.waitForTimeout(300);
        await page.screenshot({ path: `${OUT}/${name}__${v.tag}.png`, fullPage: true });
      } catch (e) { errors.push(`${v.tag} ${name} ${e.message.slice(0, 200)}`); }
    }
    await ctx.close();
  }
  fs.writeFileSync(`${OUT}/_errors.txt`, errors.join('\n'));
  fs.writeFileSync(__dirname + '/state.json', JSON.stringify(state));
  await browser.close();
  console.log('done', errors.length, 'errors');
})();
