// Usage: node shoot.cjs <steps.cjs> <outdir> <baseURL> [prefix]
// steps.cjs exports an array of { name, path, actions?: async (page) => {}, full?: bool, variants?: ['light','dark','mobile'] }
const { chromium } = require('/opt/node22/lib/node_modules/playwright');
const fs = require('fs');
const [stepsFile, OUT, BASE, PREFIX = ''] = process.argv.slice(2);
const steps = require(require('path').resolve(stepsFile));
(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch();
  const VARIANTS = {
    light: { viewport: { width: 1440, height: 900 }, theme: 'light' },
    dark: { viewport: { width: 1440, height: 900 }, theme: 'dark' },
    mobile: { viewport: { width: 390, height: 844 }, theme: 'light' },
  };
  for (const step of steps) {
    for (const v of step.variants || ['light', 'dark', 'mobile']) {
      const cfg = VARIANTS[v];
      const ctx = await browser.newContext({ viewport: cfg.viewport, storageState: '/tmp/claude-0/ux/state.json' });
      await ctx.addInitScript((t) => { try { localStorage.setItem('velox-theme', t) } catch {} }, cfg.theme);
      const page = await ctx.newPage();
      try {
        await page.goto(BASE + step.path, { waitUntil: 'networkidle' });
        await page.waitForTimeout(700);
        if (step.actions) await step.actions(page, v);
        await page.waitForTimeout(400);
        if (step.full !== false) {
          await page.addStyleTag({ content: '.h-screen{height:auto!important;min-height:100vh} main{overflow:visible!important;height:auto!important;max-height:none!important}' });
          await page.waitForTimeout(300);
        }
        const file = `${OUT}/${PREFIX}${step.name}-${v}.png`;
        await page.screenshot({ path: file, fullPage: step.full !== false });
        const sw = await page.evaluate(() => document.documentElement.scrollWidth);
        console.log('shot', file, 'scrollWidth', sw);
      } catch (e) {
        console.log('FAIL', step.name, v, e.message.split('\n')[0]);
      }
      await ctx.close();
    }
  }
  await browser.close();
})();
