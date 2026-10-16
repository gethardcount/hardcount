// PR1: the preview card on a calendar-stub sub and a mid-period-changed sub.
// main (5173 → API 8080, main code) vs mine (5182 → API 8182, this branch).
const scrollToPreview = async (page) => {
  const h = page.getByText(/Next Invoice Preview/i).first()
  await h.scrollIntoViewIfNeeded().catch(() => {})
}
module.exports = {
  variants: ['desktop-light', 'desktop-dark', 'mobile-light'],
  shots: [
    { name: 'globex-seats-preview', path: '/subscriptions/vlx_sub_db8vsnu64plc2fltr520', action: scrollToPreview, full: false },
    { name: 'ux-sub-main-preview', path: '/subscriptions/vlx_sub_db90agu64plc2fltuq4g', action: scrollToPreview, full: false },
  ],
}
