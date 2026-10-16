const { chromium } = require('/opt/node22/lib/node_modules/playwright');
(async () => {
  const b = await chromium.launch(); const ctx = await b.newContext({ storageState: '/tmp/claude-0/ux/state.json' });
  const p = await ctx.newPage(); p.on('console', m => { if (m.type()==='error') console.log('ERR', m.text().slice(0,500)) }); p.on('pageerror', e => console.log('PAGEERR', e.message));
  await p.goto(process.argv[2], { waitUntil: 'networkidle' }); await p.waitForTimeout(1000); await b.close();
})();
