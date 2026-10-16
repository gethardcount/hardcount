// Usage: node shoot.cjs <port> <outdir> <prefix> <path> [<path> ...]
// Shoots each path at desktop light (1440), desktop dark, and mobile 390 light.
// Env FULL=0 → viewport-only shots. Env ACTION=<js file> → module exporting async (page, variant) run after load.
const { chromium } = require('/opt/node22/lib/node_modules/playwright');
const [port, out, prefix, ...paths] = process.argv.slice(2);
const fs = require('fs'); fs.mkdirSync(out, { recursive: true });
const action = process.env.ACTION ? require(process.env.ACTION) : null;
const variants = (process.env.VARIANTS || 'light,dark,mobile').split(',');
(async () => {
  const browser = await chromium.launch();
  for (const v of variants) {
    const mobile = v === 'mobile';
    const theme = v === 'dark' ? 'dark' : 'light';
    const ctx = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 900 }, storageState: '/tmp/claude-0/ux/state.json' });
    await ctx.addInitScript((t) => { try { localStorage.setItem('velox-theme', t) } catch {} }, theme);
    const page = await ctx.newPage();
    for (const p of paths) {
      const slug = p.replace(/[^a-z0-9]+/gi, '_').replace(/^_|_$/g, '') || 'root';
      await page.goto(`http://127.0.0.1:${port}${p}`, { waitUntil: 'networkidle' }).catch(() => {});
      await page.waitForTimeout(800);
      if (action) await action(page, v);
      if (process.env.FULL !== '0') {
        await page.addStyleTag({ content: '.h-screen{height:auto!important;min-height:100vh} main{overflow:visible!important;height:auto!important;max-height:none!important}' });
        await page.waitForTimeout(300);
      }
      await page.screenshot({ path: `${out}/${prefix}-${slug}-${v}.png`, fullPage: process.env.FULL !== '0' });
      console.log('shot', `${prefix}-${slug}-${v}.png`);
    }
    await ctx.close();
  }
  await browser.close();
})();
