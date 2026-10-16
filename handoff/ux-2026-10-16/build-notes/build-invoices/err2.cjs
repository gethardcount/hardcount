const { chromium } = require('/opt/node22/lib/node_modules/playwright');
(async () => {
  const b = await chromium.launch(); const ctx = await b.newContext({ storageState: '/tmp/claude-0/ux/state.json' });
  const p = await ctx.newPage();
  await p.goto(process.argv[2], { waitUntil: 'networkidle' }); await p.waitForTimeout(1000);
  await p.getByText('Details').click().catch(()=>{}); await p.waitForTimeout(300);
  console.log((await p.locator('body').innerText()).slice(0,1500)); await b.close();
})();
