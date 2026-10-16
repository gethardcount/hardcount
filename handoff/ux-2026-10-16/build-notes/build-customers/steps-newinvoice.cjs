module.exports = async (page) => { await page.getByRole('button', { name: /new invoice/i }).first().click(); await page.waitForTimeout(800); return 'viewport' }
