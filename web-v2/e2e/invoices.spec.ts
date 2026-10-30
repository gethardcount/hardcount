import { test, expect, type Locator, type Page } from '@playwright/test'
import { api, apiListAll, usd, usdBalance, uniq } from './fixtures/api'
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

// A labelled figure (a total, a date, a balance) and its value. On the
// invoice page the summary strip repeats some labels (Amount due, Due) above
// the document; the last match is the document's own figure.
function figure(scope: Page | Locator, label: string) {
  return scope.getByText(label, { exact: true }).last().locator('..')
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
  // The unit price is typed in dollars and sent in cents.
  await composer.getByLabel('Line 1 unit price in USD').fill('12.50')
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

  // A draft isn't issued yet, so it can't be emailed: the email asks the
  // customer to pay. The menu doesn't offer it (and the API refuses it).
  const draftMenu = await invoiceActions(page)
  await expect(draftMenu.getByRole('menuitem', { name: 'Preview PDF' })).toBeVisible()
  await expect(draftMenu.getByRole('menuitem', { name: 'Email invoice' })).toHaveCount(0)
  await page.keyboard.press('Escape')

  // Finalize asks first (#1109) and says what will happen: the invoice is
  // issued for good, its tax is worked out now (a manual invoice), and what
  // happens to anything left to pay. With Stripe not connected in this mode
  // (CI connects none), no card can be charged or added online, so it
  // promises neither. With Stripe connected, this customer, who has no card,
  // is emailed a link to add one, but only when this server can email setup
  // links (SMTP and PAYMENT_UPDATE_URL set). Cancel changes nothing.
  const { can_email_setup_links: canEmail } = await api<{ can_email_setup_links: boolean }>('GET', '/settings/email')
  const stripeConnected = (await api<{ data: { livemode: boolean }[] }>('GET', '/settings/stripe')).data.some(c => !c.livemode)
  await page.getByRole('button', { name: 'Finalize', exact: true }).click()
  const confirm = page.getByRole('alertdialog', { name: `Finalize invoice ${number}?` })
  await expect(confirm).toContainText(`${usd(twoLines.invoice.total_amount_cents)} to ${customer.display_name}.`)
  await expect(confirm).toContainText("You can't edit it afterwards, and the only way to cancel it is to void it.")
  await expect(confirm).toContainText('Tax is worked out now, so the total may change.')
  if (!stripeConnected) {
    await expect(confirm).toContainText("Stripe isn't connected in this mode, so no card can be charged or added online. Connect Stripe under Settings to collect by card.")
    await expect(confirm).not.toContainText('emails them')
    await expect(confirm).not.toContainText('copy a setup link')
  } else if (canEmail) {
    await expect(confirm).toContainText('The customer has no card on file, so if anything is left to pay, Hardcount emails them a link to add one.')
  } else {
    await expect(confirm).toContainText("The customer has no card on file and this server can't email setup links, so if anything is left to pay, copy a setup link from the customer page and send it to them.")
    await expect(confirm).not.toContainText('emails them')
  }
  await confirm.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(confirm).toBeHidden()
  expect((await getInvoice(draft.invoice.id)).invoice.status).toBe('draft')

  // Finalize: the API issues it; the page drops the draft mark, reads Open,
  // and shows the issue and due dates the API set.
  await page.getByRole('button', { name: 'Finalize', exact: true }).click()
  await confirm.getByRole('button', { name: 'Finalize', exact: true }).click()
  await expect(confirm).toBeHidden()
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
  // Once issued, it can be emailed.
  await expect((await invoiceActions(page)).getByRole('menuitem', { name: 'Email invoice' })).toBeVisible()
  await page.keyboard.press('Escape')

  // Save & send: the invoice is finalized either way. Without SMTP (the CI
  // server has none) the toast says the email was not sent, never "sent".
  const email = await api<{ smtp_configured: boolean }>('GET', '/settings/email')
  await page.goto(`/customers/${customer.id}`)
  await page.getByRole('button', { name: 'New invoice' }).click()
  await composer.getByPlaceholder('Implementation services').fill(`E2E retainer ${id}`)
  await composer.getByLabel('Line 1 unit price in USD').fill('99')
  // Finalizing collects payment at once, so Save & send asks first, with the
  // amount, and creates nothing until the answer.
  await composer.getByRole('button', { name: 'Save & send' }).click()
  const sendConfirm = page.getByRole('alertdialog', { name: 'Finalize and send this invoice?' })
  await expect(sendConfirm).toContainText(`$99.00 before tax to ${customer.display_name}.`)
  await expect(sendConfirm).toContainText(`Then the invoice is emailed to ${customer.email}.`)
  expect(await invoicesFor(customer.id)).toHaveLength(1)
  await sendConfirm.getByRole('button', { name: 'Finalize and send' }).click()
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
  // Without Stripe (CI connects none) the finalize itself emails this
  // card-less customer the invoice, so Save & send does not send it again:
  // the customer gets it once.
  if (email.smtp_configured && !stripeConnected) {
    const invoiceEmails = (await apiListAll<{ email_type: string; invoice_number?: string }>(`/customers/${customer.id}/sent-emails`))
      .filter(e => e.email_type === 'invoice' && e.invoice_number === sent!.invoice_number)
    expect(invoiceEmails, 'Save & send emails the invoice once').toHaveLength(1)
  }
  await expect(recentInvoice(page, sent!.invoice_number)).toContainText('Open')
})

// With Stripe connected, the Finalize confirmation of a customer with no card
// says Hardcount emails them a link to add one only when this server can email
// setup links (whoami.can_email_setup_links), and otherwise sends the
// operator to copy one. CI connects no Stripe and sets no PAYMENT_UPDATE_URL,
// so both answers are set in the browser: the page must read them on a draft.
test('finalize confirmation with Stripe connected: the no-card email is promised only when the server can send it', async ({ page }) => {
  const customer = await createCustomer()
  const draft = await api<Invoice>('POST', '/invoices', {
    customer_id: customer.id,
    currency: 'USD',
    line_items: [{ description: 'E2E finalize copy', line_type: 'add_on', quantity: 1, unit_amount_cents: 4900 }],
  })
  await page.route(url => url.pathname === '/v1/settings/stripe', route => route.request().method() === 'GET'
    ? route.fulfill({ json: { data: [{ id: 'e2e_stripe_test', livemode: false, secret_key_last4: '0000', publishable_key: 'pk_test_e2e', has_webhook_secret: true }] } })
    : route.fallback())
  let canEmail = true
  await page.route(url => url.pathname === '/v1/whoami', async route => {
    const answer = await route.fetch({ headers: { ...route.request().headers(), 'sec-fetch-site': 'same-origin' } })
    await route.fulfill({ response: answer, json: { ...(await answer.json()), can_email_setup_links: canEmail } })
  })

  for (const can of [true, false]) {
    canEmail = can
    await page.goto(`/invoices/${draft.id}`)
    await expect(page.getByRole('heading', { level: 1, name: draft.invoice_number, exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Finalize', exact: true }).click()
    const confirm = page.getByRole('alertdialog', { name: `Finalize invoice ${draft.invoice_number}?` })
    if (can) {
      await expect(confirm).toContainText('The customer has no card on file, so if anything is left to pay, Hardcount emails them a link to add one.')
    } else {
      await expect(confirm).toContainText("The customer has no card on file and this server can't email setup links, so if anything is left to pay, copy a setup link from the customer page and send it to them.")
    }
    await expect(confirm).not.toContainText("Stripe isn't connected")
    await confirm.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(confirm).toBeHidden()
  }
  expect((await getInvoice(draft.id)).invoice.status).toBe('draft')
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
  // The credit note issued before payment is a row of its own, so the rows
  // add up to what was paid (M-04).
  const noteRow = page.getByRole('term').filter({ hasText: `Credit note ${unpaidNote.credit_note_number}` }).locator('..')
  await expect(noteRow).toContainText(`−${usd(unpaidNote.total_cents)}`)

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
  // The customer's page lists it under Refunds and credits.
  await page.goto(`/invoice/${paid.public_token}`)
  const back = page.getByRole('region', { name: 'Refunds and credits' })
  await expect(back.getByRole('listitem').filter({ hasText: `Credit note ${paidNote!.credit_note_number}` }))
    .toContainText(`${usd(paidNote!.credit_amount_cents)} added to your account credit`)
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
  await expect(row(issued)).toContainText('Issued')
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
  await expect(row(toIssue)).toContainText('Issued')

  await row(toVoid).getByRole('button', { name: 'Void', exact: true }).click()
  const voidDialog = page.getByRole('alertdialog', { name: 'Void credit note' })
  await voidDialog.getByLabel(/^Type VOID to confirm/).fill('VOID')
  await voidDialog.getByRole('button', { name: 'Void credit note' }).click()
  await expect(voidDialog).toBeHidden()
  await expect(row(toVoid)).toContainText('Void')

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

// Collect payment charges the customer's card at once, so it asks first,
// naming the amount and the card (C-01). The card list is stubbed in the
// browser (CI connects no Stripe); Cancel sends nothing. A card the bank
// refused for good gets no Collect button: the server refuses that charge
// (#966) and the notice offers Update payment method instead.
test('Collect payment asks first, naming the amount and the card, and is not offered on a card refused for good', async ({ page }) => {
  const { customer, invoice } = await billedCustomer()
  expect(invoice.status).toBe('finalized')
  await page.route(url => url.pathname === `/v1/customers/${customer.id}/payment-methods`, route => route.fulfill({
    json: { data: [{ id: 'pm_e2e', customer_id: customer.id, stripe_payment_method_id: 'pm_e2e', card_brand: 'visa', card_last4: '4242', is_default: true, created_at: new Date().toISOString() }], has_more: false },
  }))
  const collects: string[] = []
  page.on('request', r => { if (r.method() === 'POST' && new URL(r.url()).pathname.endsWith('/collect')) collects.push(r.url()) })

  await page.goto(`/invoices/${invoice.id}`)
  await page.getByRole('button', { name: 'Collect payment' }).click()
  const amount = usd(invoice.amount_due_cents)
  const confirm = page.getByRole('alertdialog', { name: `Collect ${amount} now?` })
  await expect(confirm).toContainText(`${amount} due on ${invoice.invoice_number} from ${customer.display_name}.`)
  await expect(confirm).toContainText('Any credit balance the customer has is used first.')
  await expect(confirm).toContainText('Whatever is left is charged to Visa ····4242 right away.')
  await expect(confirm.getByRole('button', { name: `Charge ${amount}` })).toBeVisible()
  await confirm.getByRole('button', { name: 'Cancel' }).click()
  await expect(confirm).toBeHidden()
  expect(collects, 'Cancel charges nothing').toHaveLength(0)

  // The bank refused this card for good: no Collect button.
  await page.route(url => url.pathname === `/v1/invoices/${invoice.id}`, async route => {
    const answer = await route.fetch()
    const body = await answer.json()
    body.invoice.attention = { severity: 'critical', reason: 'payment_failed', code: 'payment.card_refused', message: "The customer's bank refused this card for good.", actions: [{ code: 'update_payment_method', label: 'Update payment method' }] }
    await route.fulfill({ response: answer, json: body })
  })
  await page.reload()
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()
  await expect(page.getByText("The customer's bank refused this card for good.")).toBeVisible()
  await expect(page.getByRole('button', { name: 'Collect payment' })).toHaveCount(0)
})

// A refresh that fails after the page loaded keeps the page, and an open
// dialog with what was typed in it, and says so in one line with Retry
// (B-06). It used to swap the whole page for an error screen. An open
// invoice polls every 10 seconds, so the failed poll arrives on its own.
test('a failed refresh keeps the invoice page and an open dialog, and Retry recovers', async ({ page }) => {
  const { invoice } = await billedCustomer()
  await page.goto(`/invoices/${invoice.id}`)
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true })).toBeVisible()
  await (await invoiceActions(page)).getByRole('menuitem', { name: 'Email invoice' }).click()
  const dialog = page.getByRole('dialog', { name: 'Email invoice' })
  await dialog.getByLabel('Recipient email').fill('typed-before-outage@example.com')

  const route = (url: URL) => url.pathname === `/v1/invoices/${invoice.id}`
  await page.route(route, r => r.fulfill({ status: 503, json: { error: { type: 'api_error', message: 'unavailable' } } }))
  // includeHidden: the open dialog hides the page behind it from role queries.
  const notice = page.getByRole('status', { includeHidden: true }).filter({ hasText: "Couldn't refresh this invoice." })
  await expect(notice).toBeAttached({ timeout: 20_000 })
  await expect(dialog.getByLabel('Recipient email')).toHaveValue('typed-before-outage@example.com')
  await expect(page.getByRole('heading', { level: 1, name: invoice.invoice_number, exact: true, includeHidden: true })).toBeAttached()

  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await page.unroute(route)
  await notice.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(notice).toHaveCount(0)
})

// The list finds a customer's invoices by the customer: search matches the
// name or email, not only the invoice number, and a picker filters to one
// customer (C-15).
test('invoices: search by customer name or email, and filter by customer', async ({ page }) => {
  const { customer, invoice } = await billedCustomer()
  await page.goto('/invoices')
  const rows = page.getByRole('table').locator('tbody tr')
  const search = page.getByPlaceholder('Search by number, customer or email')
  for (const term of [customer.display_name, customer.email]) {
    await search.fill(term)
    await expect(rows).toHaveCount(1)
    await expect(rows.first()).toContainText(invoice.invoice_number)
  }
  await search.fill('')

  await page.getByRole('button', { name: /All customers/ }).click()
  await page.getByPlaceholder('Search...').fill(customer.display_name)
  await page.getByRole('option', { name: new RegExp(customer.display_name) }).click()
  await expect(page).toHaveURL(new RegExp(`customer=${customer.id}`))
  await expect(rows).toHaveCount(1)
  await expect(rows.first()).toContainText(invoice.invoice_number)
})

// On a phone (390px) an invoice row and its lines keep the amount and status
// on screen: the list stacks each row, the invoice puts quantity and unit
// price under the item (L-04). They were off the right edge.
test('invoices on a phone: the list row and the lines keep amount and status on screen', async ({ page }) => {
  const { customer, invoice } = await billedCustomer()
  await page.setViewportSize({ width: 390, height: 844 })
  const onScreen = async (l: Locator) => {
    const box = (await l.boundingBox())!
    expect(box.x + box.width).toBeLessThanOrEqual(390)
  }

  await page.goto(`/invoices?customer=${customer.id}`)
  const row = page.getByRole('table').locator('tbody tr').first()
  await expect(row).toContainText(invoice.invoice_number)
  await onScreen(row.getByText(usd(invoice.amount_due_cents), { exact: true }).filter({ visible: true }))
  await onScreen(row.getByText('Open', { exact: true }).filter({ visible: true }))

  await page.goto(`/invoices/${invoice.id}`)
  const line = page.getByRole('table').locator('tbody tr').first()
  await expect(line).toContainText('×')
  await onScreen(line.locator('td').last())
  expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0)
})

// Refunds made in Stripe outside Hardcount (ADR-134). CI connects no Stripe,
// so the refund, the invoice's card payment and the server's answers to
// Record, Attach and Dismiss are stubbed in the browser; the invoice and its
// customer are real. Each button asks first and sends exactly the refund.
const STRIPE_REFUND_ID = 're_e2e_dashboard'

async function finalizedOneOff() {
  const customer = await createCustomer()
  const draft = await api<Invoice>('POST', '/invoices', {
    customer_id: customer.id,
    currency: 'USD',
    line_items: [{ description: 'E2E setup fee', line_type: 'add_on', quantity: 1, unit_amount_cents: 4900 }],
  })
  await api('POST', `/invoices/${draft.id}/finalize`)
  return { customer, invoice: (await getInvoice(draft.id)).invoice }
}

// Shows the invoice as paid by card, with the notice the server sends for a
// refund made in Stripe that it flagged, and that refund.
async function stubPaidWithRefundNotice(page: Page, invoiceId: string, reviewReason: string) {
  await page.route(url => url.pathname === `/v1/invoices/${invoiceId}`, async route => {
    if (route.request().method() !== 'GET') return route.fallback()
    const answer = await route.fetch()
    const body = await answer.json()
    Object.assign(body.invoice, {
      status: 'paid',
      payment_status: 'succeeded',
      amount_paid_cents: body.invoice.total_amount_cents,
      amount_due_cents: 0,
      stripe_payment_intent_id: 'pi_e2e_dashboard',
      attention: {
        severity: 'warning',
        reason: 'refunded_in_stripe',
        code: `payment.refunded_in_stripe.${reviewReason}`,
        message: 'A refund of $40.00 was made in Stripe and not recorded here. Record it with a credit note linked to the refund, or dismiss it if you handled it outside Hardcount.',
      },
    })
    await route.fulfill({ response: answer, json: body })
  })
  const now = new Date().toISOString()
  await page.route(url => url.pathname === `/v1/invoices/${invoiceId}/stripe-refunds`, route => route.fulfill({
    json: {
      data: [{
        id: 'hc_srf_e2e', invoice_id: invoiceId, stripe_refund_id: STRIPE_REFUND_ID, stripe_payment_intent_id: 'pi_e2e_dashboard',
        amount_cents: 4000, currency: 'USD', status: 'succeeded', reason: 'requested_by_customer',
        outcome: 'needs_review', review_reason: reviewReason, refund_created_at: now,
        livemode: false, created_at: now, updated_at: now,
      }],
      total: 1,
    },
  }))
}

// A credit note as the list API returns it, for a stubbed list.
function stubNote(invoiceId: string, customerId: string, over: Record<string, unknown>) {
  const now = new Date().toISOString()
  return {
    id: 'cn_e2e_stub', invoice_id: invoiceId, customer_id: customerId, credit_note_number: 'CN-E2E-1', status: 'issued',
    reason: 'Refunded in Stripe', subtotal_cents: 4000, total_cents: 4000, refund_amount_cents: 4000, credit_amount_cents: 0,
    out_of_band_amount_cents: 0, refund_status: 'succeeded', currency: 'USD', issued_at: now, created_at: now,
    is_simulated: false, recorded_from_stripe: false, ...over,
  }
}

// Answers every GET of the credit-note list (the invoice page's, by
// invoice_id, and the Credit notes page's) with `notes`.
async function stubCreditNoteList(page: Page, notes: unknown[]) {
  await page.route(url => url.pathname === '/v1/credit-notes', route => route.request().method() === 'GET'
    ? route.fulfill({ json: { data: notes, total: notes.length } })
    : route.fallback())
}

test('a refund made in Stripe: the notice records it as a credit note or dismisses it, asking first', async ({ page }) => {
  const { customer, invoice } = await finalizedOneOff()
  await stubPaidWithRefundNotice(page, invoice.id, 'seen_late')
  const sent: { path: string; body: unknown; idempotencyKey?: string }[] = []
  await page.route(url => url.pathname === '/v1/credit-notes', async route => {
    if (route.request().method() !== 'POST') return route.fallback()
    sent.push({ path: 'create', body: route.request().postDataJSON(), idempotencyKey: route.request().headers()['idempotency-key'] })
    await route.fulfill({ status: 201, json: stubNote(invoice.id, customer.id, { recorded_from_stripe: true, stripe_refund_id: STRIPE_REFUND_ID }) })
  })
  await page.route(url => url.pathname === `/v1/invoices/${invoice.id}/stripe-refunds/${STRIPE_REFUND_ID}/dismiss`, async route => {
    sent.push({ path: 'dismiss', body: route.request().postDataJSON() })
    await route.fulfill({ json: { stripe_refund_id: STRIPE_REFUND_ID, outcome: 'dismissed' } })
  })

  await page.goto(`/invoices/${invoice.id}`)
  await expect(page.getByText('Refunded in Stripe', { exact: true })).toBeVisible()
  await expect(page.getByText(/^A refund of \$40\.00 was made in Stripe and not recorded here\./)).toBeVisible()

  // Dismiss needs a note: an empty one is refused under the box and sends nothing.
  await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
  const dismiss = page.getByRole('dialog', { name: 'Dismiss this refund?' })
  await expect(dismiss).toContainText("No credit note is made, so this invoice and its PDF still won't show the refund.")
  await dismiss.getByRole('button', { name: 'Dismiss refund' }).click()
  await expect(dismiss.getByText('Say how you handled this refund.')).toBeVisible()
  expect(sent).toHaveLength(0)
  await dismiss.getByLabel('How you handled it').fill('  Recorded in our books  ')
  await dismiss.getByRole('button', { name: 'Dismiss refund' }).click()
  await expect(dismiss).toBeHidden()
  await expect(page.getByText('Refund dismissed')).toBeVisible()
  expect(sent).toEqual([{ path: 'dismiss', body: { note: 'Recorded in our books' } }])

  // Record: Cancel sends nothing; confirming sends the refund as the whole
  // card refund of an issued note, with an Idempotency-Key.
  sent.length = 0
  await page.getByRole('button', { name: 'Record as credit note' }).click()
  const record = page.getByRole('alertdialog', { name: 'Record $40.00 as a credit note?' })
  await expect(record).toContainText(`Hardcount records it on ${invoice.invoice_number} as a credit note whose card refund is that refund.`)
  await expect(record).toContainText('The customer is not refunded again.')
  await record.getByRole('button', { name: 'Cancel' }).click()
  await expect(record).toBeHidden()
  expect(sent).toHaveLength(0)
  await page.getByRole('button', { name: 'Record as credit note' }).click()
  await record.getByRole('button', { name: 'Record $40.00' }).click()
  await expect(record).toBeHidden()
  await expect(page.getByText('Recorded as CN-E2E-1')).toBeVisible()
  expect(sent).toHaveLength(1)
  expect(sent[0].idempotencyKey).toBeTruthy()
  expect(sent[0].body).toEqual({
    invoice_id: invoice.id,
    reason: 'Refunded in Stripe: requested by customer',
    refund_amount_cents: 4000,
    credit_amount_cents: 0,
    out_of_band_amount_cents: 0,
    auto_issue: true,
    stripe_refund_id: STRIPE_REFUND_ID,
    lines: [{ description: `Refund made in Stripe (${STRIPE_REFUND_ID})`, quantity: 1, unit_amount_cents: 4000 }],
  })
  // The page may still be refreshing the stubbed invoice; let the test end without it.
  await page.unrouteAll({ behavior: 'ignoreErrors' })
})

// A refund made in Stripe that may replace a credit note's failed refund can
// be attached to that note. Retry refund on the note is refused until then,
// with the server's reason and a way back to the invoice.
test('a refund made in Stripe after a failed refund: Attach to the note, and Retry refund says why it is refused', async ({ page }) => {
  const { customer, invoice } = await finalizedOneOff()
  await stubPaidWithRefundNotice(page, invoice.id, 'credit_note_refund_failed')
  const failed = stubNote(invoice.id, customer.id, { id: 'cn_e2e_failed', credit_note_number: 'CN-E2E-FAILED', reason: 'Goodwill', refund_status: 'failed' })
  await stubCreditNoteList(page, [failed])
  const attached: unknown[] = []
  await page.route(url => url.pathname === '/v1/credit-notes/cn_e2e_failed/attach-stripe-refund', async route => {
    attached.push(route.request().postDataJSON())
    await route.fulfill({ json: { ...failed, refund_status: 'succeeded', stripe_refund_id: STRIPE_REFUND_ID, recorded_from_stripe: true } })
  })

  await page.goto(`/invoices/${invoice.id}`)
  await page.getByRole('button', { name: 'Attach to CN-E2E-FAILED' }).click()
  const attach = page.getByRole('alertdialog', { name: 'Attach this refund to CN-E2E-FAILED?' })
  await expect(attach).toContainText('The card refund of $40.00 on CN-E2E-FAILED failed.')
  await expect(attach).toContainText('No new credit note is made, and the customer is not refunded again.')
  await attach.getByRole('button', { name: 'Attach to CN-E2E-FAILED' }).click()
  await expect(attach).toBeHidden()
  await expect(page.getByText('Refund attached to CN-E2E-FAILED')).toBeVisible()
  expect(attached).toEqual([{ stripe_refund_id: STRIPE_REFUND_ID }])
  // Record and Dismiss stay offered: the refund in Stripe may be a separate one.
  await expect(page.getByRole('button', { name: 'Record as credit note' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible()

  // Credit notes page: Retry refund on the failed note is refused (409).
  const refusal = 'A refund of $40.00 was made in Stripe on this invoice (re_e2e_dashboard) and is not recorded here. If it replaces this refund, attach it to this credit note; otherwise record it or dismiss it. Then retry. Retrying now could refund the customer twice.'
  await page.route(url => url.pathname === '/v1/credit-notes/cn_e2e_failed/retry-refund', route => route.fulfill({
    status: 409, json: { error: { type: 'invalid_request_error', code: 'stripe_refund_unresolved', message: refusal } },
  }))
  await page.goto('/credit-notes')
  await page.getByRole('row').filter({ hasText: 'CN-E2E-FAILED' }).getByRole('button', { name: 'Retry refund' }).click()
  await expect(page.getByText('Refund not retried')).toBeVisible()
  await expect(page.getByText(refusal)).toBeVisible()
  await page.getByRole('button', { name: 'Open invoice' }).click()
  await expect(page).toHaveURL(new RegExp(`/invoices/${invoice.id}$`))
  // The page may still be refreshing the stubbed invoice; let the test end without it.
  await page.unrouteAll({ behavior: 'ignoreErrors' })
})

// A credit note whose card refund is a refund made in Stripe carries a
// "Recorded from Stripe" tag: on the Credit notes page, with the refund's id
// in its tooltip and no extra width for the table, and under the paid invoice
// with the refund's id and a link to the payment in Stripe.
test('a credit note recorded from Stripe is tagged, with its refund, on the credit notes page and the invoice', async ({ page }) => {
  const { customer, invoice } = await finalizedOneOff()
  await stubPaidWithRefundNotice(page, invoice.id, 'seen_late')
  let recorded = true
  await page.route(url => url.pathname === '/v1/credit-notes', route => route.request().method() === 'GET'
    ? route.fulfill({ json: { data: [stubNote(invoice.id, customer.id, recorded ? { recorded_from_stripe: true, stripe_refund_id: STRIPE_REFUND_ID } : {})], total: 1 } })
    : route.fallback())
  // How far the list's table runs past its card, which scrolls it sideways.
  const overflow = () => page.locator('[data-slot="table-container"]').evaluate(c => c.scrollWidth - c.clientWidth)

  await page.goto('/credit-notes')
  const row = page.getByRole('row').filter({ hasText: 'CN-E2E-1' })
  const tag = row.getByText('Recorded from Stripe', { exact: true })
  await expect(tag).toBeVisible()
  await expect(tag).toHaveAttribute('title', `Stripe refund ${STRIPE_REFUND_ID}`)
  const withTag = await overflow()
  // The same note without the tag: the table is no wider with it.
  recorded = false
  await page.reload()
  await expect(row).toBeVisible()
  await expect(tag).toHaveCount(0)
  expect(withTag, 'the tag widens the credit notes table').toBeLessThanOrEqual(await overflow())
  recorded = true

  await page.goto(`/invoices/${invoice.id}`)
  const adjustments = page.getByText('Post-payment adjustments', { exact: true }).locator('..')
  await expect(adjustments.getByText('Recorded from Stripe', { exact: true })).toBeVisible()
  await expect(adjustments.getByText(`Stripe refund ${STRIPE_REFUND_ID}`, { exact: true })).toBeVisible()
  await expect(adjustments.getByRole('link', { name: 'Open in Stripe' })).toHaveAttribute('href', 'https://dashboard.stripe.com/test/payments/pi_e2e_dashboard')
  // The page may still be refreshing the stubbed invoice; let the test end without it.
  await page.unrouteAll({ behavior: 'ignoreErrors' })
})
