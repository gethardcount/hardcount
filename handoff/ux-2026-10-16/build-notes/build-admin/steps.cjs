module.exports = [
  { name: 'home', path: '/' },
  { name: 'home-90d', path: '/', variants: ['light'], actions: async (p) => { await p.getByRole('tab', { name: /90/ }).first().click().catch(() => p.getByText('90d').first().click()); await p.waitForTimeout(1200) } },
]
