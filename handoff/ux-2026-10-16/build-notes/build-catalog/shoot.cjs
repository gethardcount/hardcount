// node shoot.cjs <baseURL> <outDir> <name=path> [name=path ...]
// Env MODES=light,dark,mobile (default all). Env STEPS=<file.cjs> exports async (page, name, shot) for interactive states.
const { chromium } = require('/opt/node22/lib/node_modules/playwright');
const [base, out, ...pages] = process.argv.slice(2);
const modes = (process.env.MODES || 'light,dark,mobile').split(',');
const fs = require('fs'); fs.mkdirSync(out, { recursive: true });
(async () => {
  const browser = await chromium.launch();
  for (const mode of modes) {
    const mobile = mode === 'mobile';
    const ctx = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 900 }, storageState: '/tmp/claude-0/ux/state.json' });
    const theme = mode === 'dark' ? 'dark' : 'light';
    await ctx.addInitScript((t) => { try { localStorage.setItem('velox-theme', t); localStorage.setItem('theme', t) } catch {} }, theme);
    const page = await ctx.newPage();
    for (const spec of pages) {
      const i = spec.indexOf('='); const name = spec.slice(0, i), path = spec.slice(i + 1);
      try {
        await page.goto(base + path, { waitUntil: 'networkidle', timeout: 30000 });
        await page.waitForTimeout(700);
        if (process.env.STEPS) await require(process.env.STEPS)(page, name, async (n) => page.screenshot({ path: `${out}/${name}-${n}--${mode}.png` }));
        const sw = await page.evaluate(() => { const m = document.querySelector('main'); return m ? m.scrollWidth - m.clientWidth : 0 });
        if (mobile && sw > 0) console.log(`OVERFLOW ${name} main overflows by ${sw}px`);
        await page.addStyleTag({ content: mobile
          ? '.h-screen{height:auto!important;min-height:100vh} main{overflow:auto!important;height:auto!important;max-height:none!important;max-width:100vw!important}'
          : '.h-screen{height:auto!important;min-height:100vh} main{overflow:visible!important;height:auto!important;max-height:none!important}' });
        await page.waitForTimeout(300);
        await page.screenshot({ path: `${out}/${name}--${mode}.png`, fullPage: true });
        console.log('ok', name, mode);
      } catch (e) { console.log('FAIL', name, mode, e.message.split('\n')[0]); }
    }
    await ctx.close();
  }
  await browser.close();
})();
