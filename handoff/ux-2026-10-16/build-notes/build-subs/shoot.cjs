// Usage: node shoot.cjs <outDir> <specModule.cjs>
// spec module exports: { shots: [{ name, path, base?: 'main'|'mine', variants?: [...], action?: async (page) => {}, full?: bool }] }
// Each shot is captured for each variant: desktop-light, desktop-dark, mobile-light (default).
const { chromium } = require('/opt/node22/lib/node_modules/playwright');
const fs = require('fs');
const OUT = process.argv[2];
const spec = require(require('path').resolve(process.argv[3]));
const BASES = { main: 'http://127.0.0.1:5173', mine: 'http://127.0.0.1:5182' };
const VARIANTS = {
  'desktop-light': { viewport: { width: 1440, height: 900 }, theme: 'light' },
  'desktop-dark': { viewport: { width: 1440, height: 900 }, theme: 'dark' },
  'mobile-light': { viewport: { width: 390, height: 844 }, theme: 'light' },
};
(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch();
  for (const v of spec.variants || Object.keys(VARIANTS)) {
    const cfg = VARIANTS[v];
    const ctx = await browser.newContext({ viewport: cfg.viewport, storageState: '/tmp/claude-0/ux/state.json', deviceScaleFactor: 1 });
    await ctx.addInitScript((t) => { try { localStorage.setItem('velox-theme', t) } catch {} }, cfg.theme);
    const page = await ctx.newPage();
    for (const s of spec.shots) {
      if (s.variants && !s.variants.includes(v)) continue;
      for (const base of s.bases || ['main', 'mine']) {
        const name = `${s.name}__${base === 'main' ? 'before' : 'after'}__${v}`;
        try {
          await page.goto(BASES[base] + s.path, { waitUntil: 'networkidle', timeout: 30000 });
          await page.waitForTimeout(s.wait ?? 800);
          if (s.action) await s.action(page, base, v);
          await page.waitForTimeout(400);
          if (s.full !== false) {
            await page.addStyleTag({ content: '.h-screen{height:auto!important;min-height:100vh} main{overflow:visible!important;height:auto!important;max-height:none!important}' });
            await page.waitForTimeout(250);
            await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: true });
          } else {
            await page.screenshot({ path: `${OUT}/${name}.png` });
          }
          const sw = await page.evaluate(() => { const m = document.querySelector('main'); return m ? [m.scrollWidth, m.clientWidth] : null });
          console.log('ok', name, sw ? `main ${sw[0]}/${sw[1]}` : '');
        } catch (e) {
          console.log('FAIL', name, e.message.split('\n')[0]);
        }
      }
    }
    await ctx.close();
  }
  await browser.close();
})();
