// Usage: node shoot.cjs <outDir> <name> <url> [mode=light|dark|mobile|all] [stepsModule]
// stepsModule (optional) exports async (page) => {} run after load; if it returns 'viewport', shoot viewport only.
const { chromium } = require('/opt/node22/lib/node_modules/playwright');
const [, , OUT, NAME, URL, MODE = 'all', STEPS] = process.argv;
(async () => {
  const fs = require('fs'); fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch();
  const modes = MODE === 'all' ? ['light', 'dark', 'mobile'] : MODE.split(',');
  for (const mode of modes) {
    const mobile = mode === 'mobile';
    const ctx = await browser.newContext({
      viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 900 },
      deviceScaleFactor: 1,
      storageState: '/tmp/claude-0/ux/state.json',
    });
    const theme = mode === 'dark' ? 'dark' : 'light';
    await ctx.addInitScript((t) => { try { localStorage.setItem('velox-theme', t) } catch {} }, theme);
    const page = await ctx.newPage();
    await page.goto(URL, { waitUntil: 'networkidle' });
    await page.waitForTimeout(1200);
    let how = 'full';
    if (STEPS) { const r = await require(STEPS)(page, mode); if (r) how = r; await page.waitForTimeout(500); }
    if (how === 'full') {
      await page.addStyleTag({ content: '.h-screen{height:auto!important;min-height:100vh} main{overflow:visible!important;height:auto!important;max-height:none!important}' });
      await page.waitForTimeout(300);
    }
    await page.screenshot({ path: `${OUT}/${NAME}__${mode}.png`, fullPage: how === 'full' });
    await ctx.close();
  }
  await browser.close();
})().catch(e => { console.error(e); process.exit(1); });
