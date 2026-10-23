# How Velox compares

This page compares Velox with four open-source billing engines: Lago, OpenMeter, Flexprice and Kill Bill. Each cell comes from that project's own source or docs, linked under the table. Cells marked ¹ and "None found" are our reading, not the project's own words.

**Checked on 2026-10-12.** Projects change. Re-check any cell you plan to rely on.

**Re-checked on 2026-10-23:** the licence, prepaid-credit, retry, webhook, tenant-isolation and payment-provider cells changed that day. They were read in OpenMeter [`f04b561f`](https://github.com/openmeterio/openmeter/tree/f04b561f8dd2dafd1e77a02fb4258fab0edbef8f), Flexprice [`1c23b81`](https://github.com/flexprice/flexprice/tree/1c23b81dabe900e86c0694da10598ecd6f8d3521) and lago-api [`2e52dfc`](https://github.com/getlago/lago-api/tree/2e52dfc13ad00a2d108a6ab964263dbe24b6bb77), all from 2026-10-22. Other cells are from the commits linked under [Sources](#sources).

Velox is pre-1.0 and has no production users yet. The other four are in production use at other companies. Weigh everything below with that in mind.

## In short

**Where Velox differs in your favour**

- One MIT licence, and every feature is in it.
- A test clock for each test customer, on your normal deployment.
- On API request paths, Postgres row-level security keeps accounts apart, on top of application code. Background jobs filter by account in application code.
- Failed-payment retries are included. Kill Bill includes them too.
- Webhooks are signed, with a timestamp, without running another service.
- The throughput figure comes with its hardware and method.

**Where another project is ahead**

- Payment providers: Velox supports Stripe cards only. Through Stripe, Lago also takes bank debits such as SEPA, ACH and BACS. OpenMeter's Stripe invoices offer the methods turned on in the Stripe account.
- SDKs: Velox has none.
- Invoice preview: Velox's leaves out tax and credits.
- Fewest services to run: Kill Bill's Compose file has three.
- Previewing a plan change: Flexprice and Kill Bill both do it, and Lago does with a premium licence.

## The comparison

| | Velox | Lago | OpenMeter | Flexprice | Kill Bill |
|---|---|---|---|---|---|
| **Licence** | MIT, one edition | AGPL-3.0. A licence key unlocks premium features. | Apache-2.0. No operator UI in the open-source distribution. | AGPL-3.0 at the root. Every service, from event ingest to invoices and wallets, is in `internal/ee`. That code's licence allows production use only with a Flexprice Enterprise licence. The README calls 99% of the code AGPL. | Apache-2.0. A commercial plugin, Aviate, adds raw-event metering (aggregation), wallets and coupons. The open-source core bills usage you have already added up. |
| **Services in the Compose file** | 5, including a front proxy that the others' files do not have | 8, one of them a one-off migration step | 11, described as "not a production topology" | 9 by default, plus a separate dashboard | 3 |
| **Price by model × token type on one meter** | Yes: pricing rules match on event dimensions | Yes: charge filters on a billable metric | One filtered feature per combination¹ | A meter filters events by property, and a price references one meter. No per-dimension rate found. | No dimensions on usage; one unit per combination¹ |
| **Prepaid credits** | Yes. Credit from a prepaid commit is usable before its invoice is paid. | Wallets are free. Recurring top-ups and real-time balance need a premium licence. | Usage grants on entitlements. Monetary credits sit behind a config flag that defaults to off; the quickstart config turns it on. | Wallets with prepaid and promotional credits, auto top-up, expiry and a real-time balance. The wallet service is in `internal/ee` (enterprise licence). | Account credits. Wallets are in Aviate. |
| **Test clock** | Per customer, in test mode on the normal deployment | None found | None in the public API. A sandbox app and an invoice-simulate endpoint exist. | None found | One clock for the whole server; needs test mode at start-up |
| **Failed-payment retries** | Included | Automatic dunning is a premium add-on. Free: manual retry per invoice. | None of its own. With the Stripe app, Stripe Invoicing collects and retries, and Stripe's Invoicing fee applies. | No retry schedule found. A manual payment attempt per invoice, and subscriptions can auto-cancel after a grace period. | Included: a retry schedule and overdue states |
| **Outbound webhooks** | HMAC over timestamp and body; 6 attempts over about 26.5 hours | JWT (default) or HMAC, no timestamp signed; 3 attempts | Delivered through Svix, a separate service | Self-hosted default: unsigned, sent to one endpoint per tenant set in `config.yaml`, with no API to add one. Retries: up to 3 with backoff, a job every 2 minutes for stale deliveries (up to 5 attempts), and a manual retry route. With Svix enabled: signed. | Unsigned; retries at 15m, 30m, 2h, 12h, 1d |
| **Tenant isolation** | Postgres row-level security on API request paths, plus application code. Background jobs: application code. | Application code | Application code; one static namespace in the open-source server | Application code | Application code |
| **Payment providers** | Stripe, cards only | Stripe, GoCardless, Cashfree, Adyen, Flutterwave, Moneyhash. Through Stripe: card, SEPA, ACH and BACS debit, Link, Boleto, crypto and bank transfer. | Stripe, through Stripe Invoicing, with the methods turned on in the Stripe account; or your own invoicing integration | Stripe, Razorpay, Nomod, Moyasar, Paddle, Whop, Chargebee | Plugins for Adyen, Braintree, Qualpay, Stripe, Hyperswitch |
| **SDKs** | None | 8 listed; the Java and C# repos are archived | JavaScript, Python, Go, all pre-release | Go, Python, TypeScript | Java, PHP, Python, Ruby, Node, Go |
| **Published throughput** | An ingest benchmark, published with its hardware and method | 10K events/s on one Postgres, 1 to 3M with ClickHouse and Kafka; configuration named, no hardware sizing or method | "Meter millions of events per second"; no hardware or method | "40k+ events processed per second"; no hardware or method | None found |
| **Invoice preview** | Free. Subtotal only: no tax or credits. | Needs a premium licence. Covers tax, credits and plan changes. | An invoice-simulate endpoint | Preview routes, including subscription changes | Dry run, including subscription changes |

¹ Our reading of the project's schema or API spec. No sentence in its docs states it.

"None found" means a search of the project's source and docs found nothing. The project has not said so itself.

**Lago on scale.** Lago's docs send everything under 10,000 events a second to its REST API, and recommend Kafka only above that ([source](https://getlago.com/docs/guide/events/ingesting-usage), checked 2026-10-14).

**Fresh installs.** Users of these projects have reported a fresh install failing before the first invoice, often at sign-up, for example [lago#514](https://github.com/getlago/lago/issues/514), [lago#368](https://github.com/getlago/lago/issues/368) and [flexprice#819](https://github.com/flexprice/flexprice/issues/819) (open on 2026-10-23).

## Sources

Links to other projects are pinned to the commit that was read.

**Velox**

- Licence: [`LICENSE`](../LICENSE). Services: [`deploy/compose/docker-compose.yml`](../deploy/compose/docker-compose.yml).
- Pricing by dimension: [AI pricing](ai-pricing.md#how-velox-picks-the-price-for-an-event). Prepaid commits: [ADR-078](adr/078-prepaid-commits-phase-1.md).
- Webhooks: [`webhooks.md`](webhooks.md). Throughput: [`benchmarks/sustained-throughput.md`](benchmarks/sustained-throughput.md).
- Invoice preview: [The Velox API](api-surface.md#invoices-and-billing). Card-only checkout: [`internal/payment/checkout.go`](../internal/payment/checkout.go).
- Tenant isolation, and what runs outside row-level security: [Architecture](architecture.md#design-rules).

**Lago**

- Licence and premium list: [`LICENSE`](https://github.com/getlago/lago/blob/052739102beaf9cb81288b4136060fe463ce398d/LICENSE), [`organization.rb`](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/models/organization.rb#L141-L172), [`license.rb`](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/lib/lago_utils/lago_utils/license.rb#L10-L21).
- Services: [`docker-compose.yml`](https://github.com/getlago/lago/blob/052739102beaf9cb81288b4136060fe463ce398d/docker-compose.yml#L89-L347).
- Charge filters: [docs](https://getlago.com/docs/guide/plans/charges/charges-with-filters).
- Wallets: [docs](https://getlago.com/docs/guide/wallet-and-prepaid-credits/overview), [top-up rule gate](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/services/wallets/recurring_transaction_rules/create_service.rb#L16).
- Dunning: [docs](https://getlago.com/docs/guide/dunning/automatic-dunning), [gate](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/services/dunning_campaigns/bulk_process_service.rb#L8).
- Webhooks: [signing](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/models/webhook.rb#L57-L86), [attempts](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/services/webhooks/send_http_service.rb#L61-L63).
- Tenant scoping: [`base_controller.rb`](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/controllers/api/base_controller.rb#L32-L40).
- Providers: [`customer.rb`](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/models/customer.rb#L137). SDKs: [docs](https://getlago.com/docs/developers#sdks-and-clients).
- Stripe payment methods (2026-10-23): [`stripe_customer.rb`](https://github.com/getlago/lago-api/blob/2e52dfc13ad00a2d108a6ab964263dbe24b6bb77/app/models/payment_provider_customers/stripe_customer.rb#L5-L7).
- Throughput: [docs](https://getlago.com/docs/guide/events/usage-performances). Preview: [gate](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/services/invoices/preview_service.rb#L19-L22), [tax and credits](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/services/invoices/preview_service.rb#L291-L321), [plan change](https://github.com/getlago/lago-api/blob/4c4f02df9a6dba69fb28e22c67f70338656af0b9/app/services/invoices/preview/subscription_plan_change_service.rb).

**OpenMeter**

- Licence, UI, services, maturity: [`README.md`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/README.md#L40-L41), [`quickstart/docker-compose.yaml`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/quickstart/docker-compose.yaml#L8-L147).
- Features and prices: [`features.tsp`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/api/spec/packages/legacy/src/productcatalog/features.tsp#L52-L73), [`price.tsp`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/api/spec/packages/aip/src/productcatalog/price.tsp#L17-L47).
- Credits flag: [`credits.go`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/app/config/credits.go#L25-L26), [quickstart config](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/quickstart/config.yaml#L33-L35). Entitlement grants: [`openapi.yaml`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/api/openapi.yaml#L4679).
- Sandbox and simulate: [`marketplace.go`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/openmeter/app/sandbox/marketplace.go#L9-L11), [`openapi.yaml`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/api/openapi.yaml#L1606-L1615).
- Collection: [docs](https://openmeter.io/docs/integrations/stripe/invoicing), [`invoice.go`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/openmeter/app/stripe/client/invoice.go#L19-L55).
- Retries and payment methods through the Stripe app (2026-10-23): [`appinvoice.go`](https://github.com/openmeterio/openmeter/blob/f04b561f8dd2dafd1e77a02fb4258fab0edbef8f/openmeter/app/stripe/appinvoice.go#L96-L104), [`invoice.go`](https://github.com/openmeterio/openmeter/blob/f04b561f8dd2dafd1e77a02fb4258fab0edbef8f/openmeter/app/stripe/client/invoice.go#L24-L41).
- Webhooks: [`notification.go`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/app/common/notification.go#L125-L131), [docs](https://openmeter.io/docs/integrations/notifications/webhooks).
- Namespace: [`main.go`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/cmd/server/main.go#L159). Apps: [`appbase.go`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/openmeter/app/appbase.go#L14-L16).
- SDKs: [`README.md`](https://github.com/openmeterio/openmeter/blob/e00163c4b8a0726198c95a9ccb158d38ea8c05b8/README.md#L51). Throughput: [homepage](https://openmeter.io/).

**Flexprice**

- Licences: [`LICENSE`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/LICENSE#L1-L4), [`internal/ee/LICENSE`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/ee/LICENSE#L6-L20), [`internal/ee/service`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/ee/service), [`README.md`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/README.md#L191).
- Licence scope (2026-10-23): [`internal/ee/LICENSE`](https://github.com/flexprice/flexprice/blob/1c23b81dabe900e86c0694da10598ecd6f8d3521/internal/ee/LICENSE#L5-L9), [`internal/ee/service`](https://github.com/flexprice/flexprice/tree/1c23b81dabe900e86c0694da10598ecd6f8d3521/internal/ee/service), [`README.md`](https://github.com/flexprice/flexprice/blob/1c23b81dabe900e86c0694da10598ecd6f8d3521/README.md#L189-L191).
- Services: [`docker-compose.yml`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/docker-compose.yml#L1-L241).
- Meter and price: [`meter/model.go`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/domain/meter/model.go#L19-L31), [`price/model.go`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/domain/price/model.go#L107-L108).
- Wallets: [docs](https://docs.flexprice.io/docs/wallet/prepaid-and-promotional), [auto top-up](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/domain/wallet/model.go#L24), [expiry](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/domain/wallet/transaction.go#L28), [real-time balance](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/api/router.go#L415). Preview of subscription changes: [`router.go`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/api/router.go#L373-L380).
- Unpaid invoices: [docs](https://docs.flexprice.io/docs/settings/settings), [manual attempt](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/api/router.go#L446).
- Webhooks: [signing](https://docs.flexprice.io/developers/webhooks/signature-verification). Endpoints and retries (2026-10-23): [`config.yaml`](https://github.com/flexprice/flexprice/blob/1c23b81dabe900e86c0694da10598ecd6f8d3521/internal/config/config.yaml#L336-L366), [`schedules.go`](https://github.com/flexprice/flexprice/blob/1c23b81dabe900e86c0694da10598ecd6f8d3521/internal/temporal/service/schedules.go#L84-L89), [`router.go`](https://github.com/flexprice/flexprice/blob/1c23b81dabe900e86c0694da10598ecd6f8d3521/internal/api/router.go#L715-L718).
- Tenant scoping: [`invoice.go`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/repository/ent/invoice.go#L345).
- Gateways: [`payment_gateway.go`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/types/payment_gateway.go#L11-L17). SDKs: [docs](https://docs.flexprice.io/developers/sdks/overview).
- Throughput: [homepage](https://flexprice.io/). Preview: [`router.go`](https://github.com/flexprice/flexprice/blob/15d780cc425989a045086f668cc8051f2d107e5a/internal/api/router.go#L443).

**Kill Bill**

- Licence and Aviate: [`LICENSE`](https://github.com/killbill/killbill/blob/3868a5b2339c3b86531904d6738a36e1e82933bf/LICENSE#L1-L3), [`README.md`](https://github.com/killbill/killbill/blob/3868a5b2339c3b86531904d6738a36e1e82933bf/README.md#L38-L40), [premium features](https://github.com/killbill/killbill-docs/blob/d8dade1b7c8eaba715a94a909fd1232361fbb738/userguide/getting-started/premium_features.adoc#L15).
- Services: [`docker-compose.kb.yml`](https://github.com/killbill/killbill-cloud/blob/e05d07c0ec6a0555abbb1e6ab848eb7818831890/docker/compose/docker-compose.kb.yml#L5-L39).
- Usage records: [`UsageRecord.java`](https://github.com/killbill/killbill-api/blob/13d8d448ccc1228202f13ac53aebc44c7b22d898/src/main/java/org/killbill/billing/usage/api/UsageRecord.java#L25-L29), [metering](https://github.com/killbill/killbill-docs/blob/d8dade1b7c8eaba715a94a909fd1232361fbb738/userguide/aviate/aviate-metering.adoc#L13).
- Credits and wallets: [credits](https://github.com/killbill/slate/blob/4ad3bbf6c2fd4a02bc8f052bd26ae5b1c2174843/source/includes/_credit.md#L5-L7), [wallets](https://github.com/killbill/killbill-docs/blob/d8dade1b7c8eaba715a94a909fd1232361fbb738/userguide/aviate/aviate-wallet.adoc#L11).
- Test clock: [`KillbillPlatformModule.java`](https://github.com/killbill/killbill-platform/blob/4a08e66de1307f8ebfe0d8866314235c93ba4ea8/server/src/main/java/org/killbill/billing/server/modules/KillbillPlatformModule.java#L113-L123).
- Retries and overdue: [`PaymentConfig.java`](https://github.com/killbill/killbill/blob/3868a5b2339c3b86531904d6738a36e1e82933bf/util/src/main/java/org/killbill/billing/util/config/definition/PaymentConfig.java#L31-L34), [overdue](https://github.com/killbill/killbill-docs/blob/d8dade1b7c8eaba715a94a909fd1232361fbb738/userguide/tutorials/overdue.adoc#L18-L24).
- Notifications: [`PushNotificationListener.java`](https://github.com/killbill/killbill/blob/3868a5b2339c3b86531904d6738a36e1e82933bf/profiles/killbill/src/main/java/org/killbill/billing/server/notifications/PushNotificationListener.java#L132-L154), [`NotificationConfig.java`](https://github.com/killbill/killbill/blob/3868a5b2339c3b86531904d6738a36e1e82933bf/util/src/main/java/org/killbill/billing/util/config/definition/NotificationConfig.java#L31-L34).
- Tenant scoping: [`EntitySqlDao.sql.stg`](https://github.com/killbill/killbill/blob/3868a5b2339c3b86531904d6738a36e1e82933bf/util/src/main/resources/org/killbill/billing/util/entity/dao/EntitySqlDao.sql.stg#L133-L135).
- Plugins: [docs](https://github.com/killbill/killbill-docs/blob/d8dade1b7c8eaba715a94a909fd1232361fbb738/userguide/tutorials/plugin_introduction.adoc#L152-L161). Clients: [API docs](https://github.com/killbill/slate/blob/4ad3bbf6c2fd4a02bc8f052bd26ae5b1c2174843/source/includes/_index.md#L10-L12).
- Dry run: [modes](https://github.com/killbill/killbill-docs/blob/d8dade1b7c8eaba715a94a909fd1232361fbb738/userguide/subscription/includes/invoicing-dry-run.adoc#L3-L9), [`InvoiceResource.java`](https://github.com/killbill/killbill/blob/3868a5b2339c3b86531904d6738a36e1e82933bf/jaxrs/src/main/java/org/killbill/billing/jaxrs/resources/InvoiceResource.java#L442-L452).

## Not compared here

Stripe Billing, Orb and Metronome are hosted products with no source to read. For where Velox sits against them, see [Why Velox exists](why-velox.md).
