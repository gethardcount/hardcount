// Template for interactive-state screenshots. Copy, edit the STEPS, run with node.
// Signed-in session is in state.json (test mode). App: http://127.0.0.1:5173
const { chromium } = require('/opt/node22/lib/node_modules/playwright');
const OUT = process.argv[2] || '/tmp/claude-0/ux/states';
(async () => {
  const fs = require('fs'); fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, storageState: '/tmp/claude-0/ux/state.json' });
  await ctx.addInitScript(() => { try { localStorage.setItem('velox-theme', 'light') } catch {} });
  const page = await ctx.newPage();
  const full = async (name) => {
    await page.addStyleTag({ content: '.h-screen{height:auto!important;min-height:100vh} main{overflow:visible!important;height:auto!important;max-height:none!important}' });
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: true });
  };
  // STEPS — example: open a dialog and capture it (viewport shot keeps dialogs in frame)
  await page.goto('http://127.0.0.1:5173/customers', { waitUntil: 'networkidle' });
  await page.getByRole('button', { name: /new customer|create customer|add customer/i }).first().click().catch(() => {});
  await page.waitForTimeout(600);
  await page.screenshot({ path: `${OUT}/example-dialog.png` });
  await browser.close();
})();
