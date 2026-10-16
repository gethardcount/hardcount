module.exports = async (page) => { const b = page.getByRole('button', { name: /^Pay / }); if (await b.count()) { await b.first().click(); await page.waitForTimeout(1500); } };
