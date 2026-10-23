import { test, expect, type Locator, type Page } from '@playwright/test'
import { api, usd, usdBalance, uniq } from './fixtures/api'
import { billedCustomer, createCustomer, invoicesFor, type Invoice } from './fixtures/seed'

// Invoices through the screen: issue one, settle it, correct it, close it.
// Each journey does the steps an operator does, then reads the API and checks
// that the dashboard and the customer's payment page show what it holds. One
// test per kind of flow. The arithmetic behind the figures is tested in Go.

// The invoice fields these journeys read, beyond the shared fixture's.
interface InvoiceDetail extends Invoice {
  credits_applied_cents: number
  amount_paid_cents: number
  issued_at?: string
  due_at?: string
  paid_at?: string
  voided_at?: string
  billing_timezone?: string
}
interface LineItem { description: string; quantity: string; unit_amount_decimal: string; amount_cents: number }
interface CreditNote {
  id: string
  credit_note_number: string
  status: string
  total_cents: number
  credit_amount_cents: number
  refund_amount_cents: number
}

function getInvoice(id: string) {
  return api<{ invoice: InvoiceDetail; line_items: LineItem[] }>('GET', `/invoices/${id}`)
}

async function creditNotesFor(invoiceId: string): Promise<CreditNote[]> {
  return (await api<{ data: CreditNote[] }>('GET', `/credit-notes?invoice_id=${invoiceId}`)).data
}

async function creditBalance(customerId: string): Promise<number> {
  return usdBalance(await api<{ data: { currency: string; balance_cents: number }[] }>('GET', `/credits/balance/${customerId}`)).balance_cents
}

// Lets the next "Issue credit note" request reach the server, then drops the
// answer on its way back to the browser, as when a slow refund outlasts the
// browser's wait. serverStatus is what the server answered.
async function dropNextCreditNoteAnswer(page: Page) {
  let settle!: (status: number) => void
  const serverStatus = new Promise<number>(resolve => { settle = resolve })
  let dropped = false
  await page.route(url => url.pathname === '/v1/credit-notes', async route => {
    if (dropped || route.request().method() !== 'POST') return route.fallback()
    dropped = true
    // route.fetch sends the request from outside the page, without the
    // Sec-Fetch-Site header Chrome adds. The server's CSRF check would then
    // look up the Origin in its allowed list, empty on the test server, and
    // refuse. Send the header as Chrome does.
    const answer = await route.fetch({ headers: { ...route.request().headers(), 'sec-fetch-site': 'same-origin' } })
    await route.abort('failed')
    settle(answer.status())
  })
  return { serverStatus }
}

// A calendar date as the dashboard and the payment page print it: "Nov 18, 2026".
function day(iso: string, timeZone: string | undefined): string {
  return new Date(iso).toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric', timeZone: timeZone || 'UTC' })
}

// Cents as typed into a money field: 3900 → "39.00".
function dollars(cents: number): string {
  return (cents / 100).toFixed(2)
}

// Opens the invoice page's ⋯ menu.
async function invoiceActions(page: Page) {
  await page.getByRole('button', { name: 'More invoice actions' }).click()
  return page.getByRole('menu')
}

// The invoice's status pill showing exactly `label`.
function statusPill(page: Page, label: string) {
  return page.locator('[data-slot="badge"]').filter({ hasText: new RegExp(`^${label}$`) })
}

// A labelled figure (a total, a date, a balance) and its value.
function figure(scope: Page | Locator, label: string) {
  return scope.getByText(label, { exact: true }).locator('..')
}

// The customer page's row for an invoice under "Recent invoices".
function recentInvoice(page: Page, invoiceNumber: string) {
  const card = page.locator('[data-slot="card"]').filter({ has: page.getByText('Recent invoices', { exact: true }) })
  return card.getByRole('link').filter({ hasText: invoiceNumber })
}

// The Credit Notes page's "Issue credit note": pick the customer, then the
// invoice, and the invoice page's own dialog opens for that invoice.
async function issueFromCreditNotesPage(page: Page, customerName: string, invoiceNumber: string) {
  await page.getByRole('button', { name: 'Issue credit note' }).first().click()
  const picker = page.getByRole('dialog', { name: 'Choose an invoice to credit' })
  await picker.getByRole('button', { name: 'Select a customer...' }).click()
  await page.getByPlaceholder('Search...').fill(customerName)
  await page.getByRole('option', { name: new RegExp(customerName) }).click()
  await picker.getByRole('button', { name: new RegExp(invoiceNumber) }).click()
  await expect(picker).toBeHidden()
  const dialog = page.getByRole('dialog', { name: 'Issue credit note' })
  await expect(dialog.getByText(invoiceNumber, { exact: true })).toBeVisible()
  return dialog
}

// A one-off invoice for $49.00 that used `creditCents` of the customer's
// credit balance when it was finalized.
async function invoicePaidPartlyByCredit(creditCents: number) {
  const customer = await createCustomer()
  await api('POST', '/credits/grant', { customer_id: customer.id, currency: 'USD', amount_cents: creditCents, description: 'E2E credit used by an invoice' })
  const draft = await api<Invoice>('POST', '/invoices', {
    customer_id: customer.id,
    currency: 'USD',
    line_items: [{ description: 'E2E setup fee', line_type: 'add_on', quantity: 1, unit_amount_cents: 4900 }],
  })
  await api('POST', `/invoices/${draft.id}/finalize`)
  const { invoice } = await getInvoice(draft.id)
  return { customer, invoice }
}

test('a one-off invoice: save a draft from the customer page, add a line, finalize; Save & send finalizes even without email', async ({ page }) => {
  const customer = await createCustomer()
  const id = uniq()
  await page.goto(`/customers/${customer.id}`)

  // Save draft: one draft invoice holding the line as typed.
  await page.getByRole('button', { name: 'New invoice' }).click()
  const composer = page.getByRole('dialog', { name: 'New invoice' })
  await composer.getByPlaceholder('Implementation services').fill(`E2E onboarding ${id}`)
  await composer.getByPlaceholder('1', { exact: true }).fill('2')
  await composer.getByPlaceholder('2500', { exact: true }).fill('1250')
  await composer.getByRole('button', { name: 'Save draft' }).click()
  await expect(composer).toBeHidden()
  const savedToast = page.getByText(/^Draft \S+ saved$/)
  await expect(savedToast).toBeVisible()
  const savedText = await savedToast.textContent()

  const created = await invoicesFor(customer.id)
  expect(created).toHaveLength(1)
  const draft = await getInvoice(created[0].id)
  const number = draft.invoice.invoice_number
  expect(draft.invoice.status).toBe('draft')
  expect(draft.line_items.map(l => [l.description, l.quantity, l.unit_amount_decimal])).toEqual([[`E2E onboarding ${id}`, '2', '1250']])
  expect(savedText).toBe(`Draft ${number} saved`)
  const recent = recentInvoice(page, number)
  await expect(recent).toContainText('Draft')
  await expect(recent).toContainText(usd(draft.invoice.total_amount_cents))

  // Add a line on the draft: the API holds both lines, and the page shows
  // the new line and the API's subtotal.
  await recent.click()
  await expect(page.getByRole('heading', { level: 1, name: number, exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Add line item' }).click()
  const addLine = page.getByRole('dialog', { name: 'Add line item' })
  await addLine.getByLabel('Description').fill(`E2E support hours ${id}`)
  await addLine.getByLabel('Quantity').fill('3')
  await addLine.getByLabel(/^Unit price/).fill('40.00')
  await addLine.getByRole('button', { name: 'Add line item' }).click()
  await expect(addLine).toBeHidden()

  const twoLines = await getInvoice(draft.invoice.id)
  expect(twoLines.line_items).toHaveLength(2)
  const added = twoLines.line_items.find(l => l.description === `E2E support hours ${id}`)
  expect(added && [added.quantity, added.unit_amount_decimal]).toEqual(['3', '4000'])
  await expect(page.getByRole('row').filter({ hasText: `E2E support hours ${id}` })).toContainText(usd(added!.amount_cents))
  await expect(figure(page, 'Subtotal')).toContainText(usd(twoLines.invoice.subtotal_cents))

  // Finalize: the API issues it; the page drops the draft mark, reads Open,
  // and shows the issue and due dates the API set.
  await page.getByRole('button', { name: 'Finalize', exact: true }).click()
  await expect(page.getByText(`Invoice ${number} finalized`)).toBeVisible()
  const issued = (await getInvoice(draft.invoice.id)).invoice
  expect(issued.status).toBe('finalized')
  expect(issued.issued_at, 'a finalized invoice has an issue date').toBeTruthy()
  expect(issued.due_at, 'a finalized invoice has a due date').toBeTruthy()
  await expect(statusPill(page, 'Open')).toBeVisible()
  await expect(page.getByTestId('draft-watermark')).toHaveCount(0)
  await expect(figure(page, 'Issued')).toContainText(day(issued.issued_at!, issued.billing_timezone))
  await expect(figure(page, 'Due')).toContainText(day(issued.due_at!, issued.billing_timezone))
  await expect(figure(page, 'Due')).not.toContainText('from finalizing')

  // Save & send: the invoice is finalized either way. Without SMTP (the CI
  // server has none) the toast says the email was not sent, never "sent".
  const email = await api<{ smtp_configured: boolean }>('GET', '/settings/email')
  await page.goto(`/customers/${customer.id}`)
  await page.getByRole('button', { name: 'New invoice' }).click()
  await composer.getByPlaceholder('Implementation services').fill(`E2E retainer ${id}`)
  await composer.getByPlaceholder('2500', { exact: true }).fill('9900')
  await composer.getByRole('button', { name: 'Save & send' }).click()
  await expect(composer).toBeHidden()
  const outcome = page.getByText(email.smtp_configured ? /^Invoice \S+ sent$/ : /^Invoice \S+ finalized, email not sent$/)
  await expect(outcome).toBeVisible()
  const outcomeText = await outcome.textContent()
  if (!email.smtp_configured) {
    await expect(page.getByText("Email isn't set up on this server, so nothing was sent.")).toBeVisible()
  }

  const sent = (await invoicesFor(customer.id)).find(i => i.id !== draft.invoice.id)
  expect(sent?.status).toBe('finalized')
  expect(outcomeText).toContain(`Invoice ${sent!.invoice_number} `)
  await expect(recentInvoice(page, sent!.invoice_number)).toContainText('Open')
})

test('an open invoice: a credit note lowers what is owed, an offline payment settles it, and a credit note on the paid invoice goes to the credit balance', async ({ page }) => {
  const { customer, invoice } = await billedCustomer()
  const id = uniq()
  expect(invoice.status).toBe('finalized')
  const balanceBefore = await creditBalance(customer.id)
  await page.goto(`/invoices/${invoice.id}`)

  // A credit note on the unpaid invoice lowers its amount due by the note.
  await (await invoiceActions(page)).getByRole('menuitem', { name: 'Issue credit note' }).click()
  const unpaidDialog = page.getByRole('dialog', { name: 'Issue credit note' })
  await unpaidDialog.getByLabel('Reason').fill('Billing error')
  await unpaidDialog.getByLabel(/^Amount/).fill('10.00')
  await unpaidDialog.getByRole('button', { name: 'Issue credit note' }).click()
  await expect(unpaidDialog).toBeHidden()

  const [unpaidNote] = await creditNotesFor(invoice.id)
  expect(unpaidNote).toMatchObject({ status: 'issued', total_cents: 1000, credit_amount_cents: 0, refund_amount_cents: 0 })
  const reduced = (await getInvoice(invoice.id)).invoice
  expect(reduced.amount_due_cents).toBe(invoice.amount_due_cents - unpaidNote.total_cents)
  await expect(page.getByText(`Credit ${unpaidNote.credit_note_number}`)).toBeVisible()
  await expect(figure(page, 'Amount due')).toContainText(usd(reduced.amount_due_cents))

  // Record an offline payment: paid, nothing due, and the activity names it.
  await (await invoiceActions(page)).getByRole('menuitem', { name: 'Record offline payment' }).click()
  const record = page.getByRole('dialog', { name: 'Record offline payment' })
  await expect(record).toContainText(`The full ${usd(reduced.amount_due_cents)} is recorded as paid.`)
  await record.getByLabel('Reference (optional)').fill(`Wire e2e-${id}`)
  await record.getByRole('button', { name: 'Record payment' }).click()
  await expect(record).toBeHidden()

  const paid = (await getInvoice(invoice.id)).invoice
  expect(paid).toMatchObject({ status: 'paid', amount_due_cents: 0, amount_paid_cents: reduced.amount_due_cents })
  const timeline = await api<{ events: { event_type: string; description: string }[] }>('GET', `/invoices/${invoice.id}/payment-timeline`)
  expect(timeline.events).toContainEqual(expect.objectContaining({ event_type: 'invoice.paid', description: 'Offline payment received' }))
  await expect(statusPill(page, 'Paid')).toBeVisible()
  await expect(figure(page, 'Amount due')).toContainText(usd(0))
  await expect(page.getByText('Offline payment received', { exact: true })).toBeVisible()
  // Nothing is left to collect, so the collection actions are gone.
  const menu = await invoiceActions(page)
  await expect(menu.getByRole('menuitem', { name: 'Issue credit note' })).toBeVisible()
  for (const gone of ['Record offline payment', 'Mark uncollectible', 'Void invoice']) {
    await expect(menu.getByRole('menuitem', { name: gone })).toHaveCount(0)
  }
  await page.keyboard.press('Escape')

  // The customer's payment page says Paid, and offers no Pay button.
  await page.goto(`/invoice/${paid.public_token}`)
  const banner = page.getByRole('status').filter({ hasText: 'Received on' })
  await expect(banner).toContainText('Paid')
  await expect(banner).toContainText(`Received on ${day(paid.paid_at!, paid.billing_timezone)}`)
  await expect(figure(page, 'Amount paid')).toContainText(usd(paid.amount_paid_cents))
  await expect(page.getByRole('button', { name: /^Pay/ })).toHaveCount(0)
  await expect(page.getByText(/Online payment isn.t set up/)).toHaveCount(0)

  // A credit note on the paid invoice: it was paid outside Stripe, so there
  // is no card to refund, and the whole note goes to the credit balance.
  await page.goto(`/invoices/${invoice.id}`)
  await (await invoiceActions(page)).getByRole('menuitem', { name: 'Issue credit note' }).click()
  const paidDialog = page.getByRole('dialog', { name: 'Issue credit note' })
  await paidDialog.getByLabel('Reason').fill('Goodwill credit')
  await paidDialog.getByLabel(/^Amount/).fill(dollars(paid.amount_paid_cents))
  await expect(paidDialog.getByLabel(/^Refund to card/)).toBeDisabled()
  await expect(paidDialog.getByLabel('Credit balance')).toHaveValue(dollars(paid.amount_paid_cents))
  await paidDialog.getByRole('button', { name: 'Issue credit note' }).click()
  await expect(paidDialog).toBeHidden()

  const paidNote = (await creditNotesFor(invoice.id)).find(n => n.id !== unpaidNote.id)
  expect(paidNote).toMatchObject({ status: 'issued', credit_amount_cents: paid.amount_paid_cents, refund_amount_cents: 0 })
  const balance = await creditBalance(customer.id)
  expect(balance).toBe(balanceBefore + paidNote!.credit_amount_cents)
  await expect(page.getByText(`${usd(paidNote!.credit_amount_cents)} → credit`)).toBeVisible()
  await page.goto(`/customers/${customer.id}`)
  const credits = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Credits', exact: true }) })
  await expect(figure(credits, 'Balance')).toContainText(usd(balance))
})

test('closing an unpaid invoice: void gives back the credit it used, write-off can pause the subscription, and the payment page says so', async ({ page }) => {
  const [voidable, writeOff] = await Promise.all([invoicePaidPartlyByCredit(1000), billedCustomer()])

  // Void. The invoice used $10.00 of credit at finalize; voiding returns it.
  const { customer, invoice } = voidable
  expect(invoice.credits_applied_cents).toBe(1000)
  expect(await creditBalance(customer.id)).toBe(0)
  await page.goto(`/invoices/${invoice.id}`)
  await (await invoiceActions(page)).getByRole('menuitem', { name: 'Void invoice' }).click()
  const voidDialog = page.getByRole('alertdialog', { name: 'Void invoice' })
  await expect(voidDialog).toContainText('any credits applied to it are returned')
  const confirmVoid = voidDialog.getByRole('button', { name: 'Void invoice' })
  await expect(confirmVoid).toBeDisabled()
  await voidDialog.getByLabel(/^Type VOID to confirm/).fill('VOID')
  await confirmVoid.click()
  await expect(voidDialog).toBeHidden()

  const voided = (await getInvoice(invoice.id)).invoice
  expect(voided.status).toBe('voided')
  const returned = await creditBalance(customer.id)
  expect(returned).toBe(invoice.credits_applied_cents)
  await expect(statusPill(page, 'Void')).toBeVisible()
  await expect(figure(page, 'Amount due')).toContainText(usd(0))
  const menu = await invoiceActions(page)
  await expect(menu.getByRole('menuitem', { name: 'Download PDF' })).toBeVisible()
  for (const gone of ['Record offline payment', 'Issue credit note', 'Void invoice']) {
    await expect(menu.getByRole('menuitem', { name: gone })).toHaveCount(0)
  }
  await page.keyboard.press('Escape')

  await page.goto(`/customers/${customer.id}`)
  const credits = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Credits', exact: true }) })
  await expect(figure(credits, 'Balance')).toContainText(usd(returned))

  await page.goto(`/invoice/${voided.public_token}`)
  await expect(page.getByText(`Voided on ${day(voided.voided_at!, voided.billing_timezone)} — this invoice is no longer owed.`)).toBeVisible()
  await expect(page.getByRole('button', { name: /^Pay/ })).toHaveCount(0)
  await expect(page.getByText(/Online payment isn.t set up/)).toHaveCount(0)

  // Write off, and also pause collection on the subscription.
  const { invoice: open, subscription } = writeOff
  await page.goto(`/invoices/${open.id}`)
  await (await invoiceActions(page)).getByRole('menuitem', { name: 'Mark uncollectible' }).click()
  const writeOffDialog = page.getByRole('alertdialog', { name: 'Mark invoice uncollectible' })
  const pause = writeOffDialog.getByRole('checkbox', { name: 'Also pause collection on this subscription' })
  await expect(pause).not.toBeChecked()
  await pause.click()
  await expect(pause).toBeChecked()
  await expect(writeOffDialog).toContainText('they stay drafts and nothing charges them. Retries already running on its other unpaid invoices continue.')
  await writeOffDialog.getByLabel(/^Type WRITE OFF to confirm/).fill('WRITE OFF')
  await writeOffDialog.getByRole('button', { name: 'Mark uncollectible' }).click()
  await expect(writeOffDialog).toBeHidden()
  await expect(page.getByText(`${open.invoice_number} marked uncollectible, collection paused`)).toBeVisible()

  const [written, sub] = await Promise.all([
    getInvoice(open.id),
    api<{ pause_collection?: { behavior: string } }>('GET', `/subscriptions/${subscription.id}`),
  ])
  expect(written.invoice.status).toBe('uncollectible')
  expect(sub.pause_collection?.behavior).toBe('keep_as_draft')
  await expect(statusPill(page, 'Uncollectible')).toBeVisible()
  await expect(page.getByText('Written off', { exact: true })).toBeVisible()
  // Soft, so the rest of the journey still reports when the page is stale.
  await expect.soft(page.getByText('Collection on the subscription is paused.')).toBeVisible()

  // The subscription, opened from the invoice, offers to resume collection.
  // The invoice names it in its details and on the Activity card's rows;
  // this is the details link, so the Activity rows' links are left out.
  await page.getByRole('link', { name: subscription.display_name, exact: true })
    .and(page.locator(':not([data-activity-link])'))
    .click()
  await expect(page.getByRole('heading', { name: subscription.display_name })).toBeVisible()
  await expect.soft(page.getByText('New invoices are saved as drafts and not charged.')).toBeVisible()
  await expect.soft(page.getByRole('button', { name: 'Resume collection' })).toBeVisible()

  await page.goto(`/invoice/${written.invoice.public_token}`)
  await expect(page.getByText('This invoice is closed')).toBeVisible()
  await expect(page.getByRole('button', { name: /^Pay/ })).toHaveCount(0)
})

test('a voided invoice in euros owes €0.00 on its page', async ({ page }) => {
  // The page's void branch printed a literal "$0.00" whatever the currency,
  // while the Invoices list read "€0.00" (review of #1328).
  const customer = await createCustomer()
  const draft = await api<Invoice>('POST', '/invoices', {
    customer_id: customer.id,
    currency: 'EUR',
    line_items: [{ description: 'E2E setup fee', line_type: 'add_on', quantity: 1, unit_amount_cents: 4900 }],
  })
  await api('POST', `/invoices/${draft.id}/finalize`)
  await api('POST', `/invoices/${draft.id}/void`)
  expect((await getInvoice(draft.id)).invoice.status).toBe('voided')

  await page.goto(`/invoices/${draft.id}`)
  await expect(statusPill(page, 'Void')).toBeVisible()
  await expect(figure(page, 'Amount due')).toContainText('€0.00')
})

test('the credit notes page issues a note against an invoice, and issues or voids a draft', async ({ page }) => {
  const { customer, invoice } = await billedCustomer()
  await page.goto('/credit-notes')

  // Issue a note from the page: it opens the invoice page's dialog, whose
  // ceiling is what is still creditable on that invoice, and the invoice's
  // amount due drops by the note.
  const dialog = await issueFromCreditNotesPage(page, customer.display_name, invoice.invoice_number)
  await expect(dialog.getByText(`max ${usd(invoice.total_amount_cents)}`, { exact: true })).toBeVisible()
  await dialog.getByLabel('Reason').fill('Billing error')
  await dialog.getByLabel(/^Amount/).fill('12.00')
  await dialog.getByRole('button', { name: 'Issue credit note' }).click()
  await expect(dialog).toBeHidden()

  const [issued] = await creditNotesFor(invoice.id)
  expect(issued).toMatchObject({ status: 'issued', total_cents: 1200 })
  expect((await getInvoice(invoice.id)).invoice.amount_due_cents).toBe(invoice.amount_due_cents - issued.total_cents)
  await page.getByPlaceholder('Search by number, customer, invoice, or reason...').fill(invoice.invoice_number)
  const row = (note: CreditNote) => page.getByRole('row').filter({ has: page.getByText(note.credit_note_number, { exact: true }) })
  await expect(row(issued)).toContainText('issued')
  await expect(row(issued)).toContainText(usd(issued.total_cents))
  await expect(row(issued)).toContainText('applied to invoice')

  // Drafts come only from the API (the dashboard issues at once). One is
  // issued from its row, the other voided.
  const draftNote = (cents: number, reason: string) => api<CreditNote>('POST', '/credit-notes', {
    invoice_id: invoice.id, reason, lines: [{ description: reason, quantity: 1, unit_amount_cents: cents }],
  })
  const toIssue = await draftNote(500, 'E2E second note')
  const toVoid = await draftNote(300, 'E2E third note')
  expect([toIssue.status, toVoid.status]).toEqual(['draft', 'draft'])
  await page.reload()

  // The tab counts are the server's, so the Draft tab keeps its count while
  // the Issued tab is open.
  await page.getByRole('button', { name: /^Issued/ }).click()
  await expect(page.getByRole('button', { name: /^Draft\s*\d+$/ })).toBeVisible()
  await page.getByRole('button', { name: /^All/ }).click()

  await row(toIssue).getByRole('button', { name: 'Issue', exact: true }).click()
  await page.getByRole('alertdialog', { name: 'Issue credit note' }).getByRole('button', { name: 'Issue credit note' }).click()
  await expect(row(toIssue)).toContainText('issued')

  await row(toVoid).getByRole('button', { name: 'Void', exact: true }).click()
  const voidDialog = page.getByRole('alertdialog', { name: 'Void credit note' })
  await voidDialog.getByLabel(/^Type VOID to confirm/).fill('VOID')
  await voidDialog.getByRole('button', { name: 'Void credit note' }).click()
  await expect(voidDialog).toBeHidden()
  await expect(row(toVoid)).toContainText('voided')

  const notes = await creditNotesFor(invoice.id)
  expect(notes.find(n => n.id === toIssue.id)?.status).toBe('issued')
  expect(notes.find(n => n.id === toVoid.id)?.status).toBe('voided')
  // Issuing the draft lowered the amount due; voiding the other changed nothing.
  expect((await getInvoice(invoice.id)).invoice.amount_due_cents)
    .toBe(invoice.amount_due_cents - issued.total_cents - toIssue.total_cents)
})

test('issuing a credit note again after its answer was lost makes one note, from the invoice page and the credit notes page', async ({ page }) => {
  const { customer, invoice } = await billedCustomer()
  const issueButton = (dialog: Locator) => dialog.getByRole('button', { name: 'Issue credit note' })

  // Invoice page: the first answer is lost, so the operator clicks again. The
  // server answers with the note it already made, and makes no second one.
  let drop = await dropNextCreditNoteAnswer(page)
  await page.goto(`/invoices/${invoice.id}`)
  await (await invoiceActions(page)).getByRole('menuitem', { name: 'Issue credit note' }).click()
  const fromInvoice = page.getByRole('dialog', { name: 'Issue credit note' })
  await fromInvoice.getByLabel('Reason').fill('E2E lost answer')
  await fromInvoice.getByLabel(/^Amount/).fill('10.00')
  await issueButton(fromInvoice).click()
  expect(await drop.serverStatus, 'the first try reached the server and made the note').toBe(201)
  await issueButton(fromInvoice).click()
  await expect(fromInvoice).toBeHidden()
  expect(await creditNotesFor(invoice.id)).toHaveLength(1)

  // Credit notes page: the same, through the invoice it opens.
  const openListDialog = async () => {
    const dialog = await issueFromCreditNotesPage(page, customer.display_name, invoice.invoice_number)
    await dialog.getByLabel('Reason').fill('E2E lost answer')
    await dialog.getByLabel(/^Amount/).fill('5.00')
    return dialog
  }
  drop = await dropNextCreditNoteAnswer(page)
  await page.goto('/credit-notes')
  const fromList = await openListDialog()
  await issueButton(fromList).click()
  expect(await drop.serverStatus, 'the first try reached the server and made the note').toBe(201)
  await issueButton(fromList).click()
  await expect(fromList).toBeHidden()
  expect(await creditNotesFor(invoice.id)).toHaveLength(2)

  // Opening the dialog again starts a new action: the same values make a new note.
  const again = await openListDialog()
  await issueButton(again).click()
  await expect(again).toBeHidden()
  expect(await creditNotesFor(invoice.id)).toHaveLength(3)
})
