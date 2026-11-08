# RFC-002: Email pipeline: inbound, outbound, rendering and hostile content

- **Status:** Draft
- **Date:** 2026-11-08
- **Author:** Pedro (backend lead)
- **Needs input from:** Amanda (Resend setup, rendering), Marcelo (templates, pilot mailboxes), the pilot companies (forwarding, retention)
- **Becomes:** an ADR once sections 3.1, 4 and 8 are decided
- **Related:** ADR-004 (staged pipeline), ADR-005 (inbound tenant resolution, approval token), ADR-006 (classification and extraction), RFC-004 (extraction contract), Notion comments 3, 4, 7 and 12

## 1. Scope

Everything that crosses the email boundary: receiving RFQs and tariff updates, sending the approval draft and the proposal, rendering the PDF and email, and treating inbound content as hostile. Classification and extraction logic belong to ADR-006 and RFC-004.

Facts about Resend this RFC relies on (checked against its documentation on 2026-11-08): the `email.received` webhook carries metadata only, and body, headers and attachments are fetched through the Received emails and Attachments APIs; an attachment `download_url` is valid for one hour; webhooks are signed with Svix; inbound mail should use a subdomain because a root domain pointed at Resend sends all of that domain's mail to it; Resend stores received mail even if the webhook endpoint is down. Plan limits (attachment size, inbound volume, number of verified domains) are **not** verified here and must be checked on the plan the team uses.

## 2. Inbound

### 2.1 Addresses and forwarding

- Each tenant gets `<slug>@in.<product domain>`. The company forwards its own pricing mailbox to that address (ADR-005 section 7 for resolution).
- **Forwarding method changes what we receive.** Server-side routing usually keeps the original sender; a mailbox "forward" rule may rewrite it or break SPF and DKIM alignment, which can hurt delivery to the inbound domain. This must be **tested on the four real mailboxes in the first week**, not assumed. **Owner: Marcelo with the pilot companies.**
- The customer's address is taken from the original `From` header returned by the retrieve endpoint, not from the webhook's bare address field. If a forwarder rewrote it, fall back to `Reply-To`, and in all cases show the analyst the proposed recipient on the approval screen before anything is sent.

### 2.2 Ingest (pipeline stage 1)

1. The webhook handler follows ADR-005 section 7 and ADR-004: verify, resolve the tenant, insert idempotently, enqueue.
2. The ingest job fetches the body and headers, then lists attachments. Because `download_url` expires in an hour, the job downloads and stores every attachment **immediately** under `tenants/{org}/rfqs/{rfq}/` and never keeps the URL.
3. Store the RFC `Message-ID`, `In-Reply-To` and `References` separately from Resend's email id (comment 12). Resend's id is the idempotency key; the RFC `Message-ID` detects the same customer email forwarded twice.

### 2.3 Attachment limits (our own caps, independent of Resend's)

| Rule | Proposed value |
|---|---|
| Files per email | 10 |
| Total size | 20 MB |
| Allowed types | PDF, XLSX, XLS, CSV, DOCX, plain text, common images |
| Rejected | archives, executables, macro-enabled Office files |
| Password-protected or unreadable files | kept, the RFQ goes to `NEEDS_REVIEW` with the reason |

Content type is decided by sniffing the bytes, not the filename. Filenames are sanitised before they are stored or shown. Virus scanning is not in the MVP; if the pilot companies' security people insist, add it before launch. **Owner: Pedro.**

### 2.4 Mail that must not get a reaction

- Skip auto-generated mail: `Auto-Submitted` other than `no`, `Precedence: bulk` or `junk`, `X-Auto-Response-Suppress`, and bounce notices. These become `IGNORED`.
- The system never replies on its own. Every outbound message follows a human approval (section 3).
- `IGNORED` mail stays visible in the inbox under a filter so an analyst can reclassify it. A wrongly ignored RFQ costs more than a wrongly accepted newsletter.

### 2.5 Cost abuse

Anyone who learns a tenant's alias can email it, and each message would reach an LLM. Controls:

1. The cheap classification step (ADR-006) runs first; only RFQs reach extraction.
2. A per-tenant cap on messages per hour and per day, as configuration.
3. Optional per-tenant sender allow-list (known customer domains). Default: open, because new customers write in. **[business] decide with the pilot companies.**

## 3. Outbound

### 3.1 Sender identity (decision needed)

| Option | How it looks | Cost |
|---|---|---|
| **A. Shared verified domain** | From: "Company Name via QuotePilot" on our domain, `Reply-To` the company's commercial address | None for the companies. The "via" wording is visible to the customer. |
| B. Per-tenant verified domain | From: the company's own domain | Each company must add DNS records (SPF, DKIM); depends on the Resend plan's domain limit |

**Recommendation: A for the pilot, B as an upgrade per company once DNS access exists.** Both keep DMARC alignment because the `From` domain is one we or they control. **Owner: Amanda and Marcelo.**

### 3.2 Content

- The email body is a short message with the company's branding from `branding_json`. The proposal itself is the PDF.
- **No LLM-written text is sent to a customer.** The body comes from a template and validated fields. The analyst may add a plain-text note, which is escaped when rendered.
- Threading: set `In-Reply-To` and `References` from the stored headers and keep the original subject with `Re:`.
- Idempotency key `quote_id:version` (ADR-004). A retry never sends twice.
- Bounce and complaint webhooks update the quote and show in the workspace. A complaint adds the address to a suppression list.

### 3.3 The approval draft

The analyst receives an internal email with the quote summary and the 1-click link (ADR-005 section 8). It goes out from the platform sender, carries no customer-facing branding, and is clearly marked internal.

## 4. Rendering location (decision needed)

The spec's "React Email" templates need Node, and a Go core does not run TSX (comment 3). The API only needs HTML.

| Option | Description | Cost |
|---|---|---|
| **A. `ai-engine`** | Jinja2 for the email HTML, an HTML-to-PDF library for the PDF | No third runtime. Marcelo and Amanda own HTML/CSS templates. System libraries needed in the image. |
| B. Go in `core-api` | `html/template` plus a headless browser for PDF | Heavy image and a browser in the core |
| C. Small Node service | Keeps React Email as designed | A third runtime and deploy |

**Recommendation: A.** One-hour check before committing: render the real company template to PDF inside the `ai-engine` image and compare it with Marcelo's design for fonts, page breaks, and pt-BR formatting (R$ 1.234,56 and dd/mm/aaaa). The choice of PDF library is made by that check, not here. **Owner: Amanda and Marcelo.**

Tenant data needed for rendering (lines, totals, branding) is passed in the request. That is allowed because it carries no `organization_id` (ADR-001 rule 3). The logo is sent as an inline data URI with a size cap, so `ai-engine` never needs a storage URL.

## 5. Hostile content

- **Display:** raw HTML is sanitised server-side with an allow-list and shown in a sandboxed iframe without scripts. Remote images are blocked until the analyst chooses to load them. Link targets are shown in full. Attachments are download-only in the MVP (comment 12).
- **Prompt injection:** extraction output is validated by schema and bounds (RFC-004), the agents have no tool that sends mail or reads tenant data (comment 7), and outbound text is template-built (section 3.2).
- **Reply headers** sent by customers are never trusted for routing; the tenant is fixed at the inbound address.

## 6. Tariff-update emails and CSV uploads

A wrong tariff silently corrupts every later quote, so activation is a human step.

1. A tariff-update email or an uploaded CSV creates a `private_tariff_batches` row in status `PENDING_REVIEW`.
2. A user allowed to activate (proposed: `ADMIN` or `OWNER`, see open question 9) sees a diff against the **effective tariff set** (the rows selection below would use, resolved per natural key across all activated batches, not the latest batch alone, because batches are sparse): rows added, removed and changed, with the price change measured against the effective price for that key at the start of the window the pending row changes (its `valid_from`), so a future-dated change is compared with the price in force when it starts; how already-started, backdated or expired windows are presented is left to the implementing ADR, with changes beyond a configured percentage highlighted.
3. On confirmation by a user allowed to activate (open question 9) the batch becomes active. Tariffs are versioned per key, append-only (comment 11, rule 9):
   - Tariff rows are immutable: never updated, never deleted. A row's **natural key** is (carrier, route, modal, equipment, charge_category, charge_code).
   - A batch carries only the rows it changes. A changed or new price is a new row; a removed price is a **tombstone row** (`is_removed = true`) for the same natural key. Rows a batch does not mention stay in force, so a one-carrier update leaves the other carriers untouched.
   - Activation writes `activated_at` (timestamptz) once on the batch. That is the only write: no batch is ever marked as replaced.
   - **Selection** (RFC-001 step 3): among the rows of a natural key whose own validity covers the quote date (`valid_from <= date <= valid_until`) and whose batch was activated at or before the pricing instant (`activated_at <= priced_at`), take the row from the batch with the latest `activated_at`. If that row is a tombstone, that natural key is dropped from the candidates (for example one carrier's rate was removed); whether the quote is then `INCOMPLETE` follows the offer rule in RFC-001 open question 12 (by default a carrier offer is usable only if that carrier still covers every required group), so a removal never silently blocks a quote that another complete offer can price.
   - A quote stores `priced_at` (timestamptz, the pricing instant, in addition to the quote date) and the ids of the tariff rows it used. **Replay reads those recorded row ids** and never re-runs selection, so tariffs activated later cannot change a past quote. Recalculation re-runs selection and creates a new quote version (rule 9).
   - Impact: data model v1.2 needs batch `activated_at`, row `is_removed` plus an index on the natural key, and quote `priced_at`.

## 7. Retention and privacy

Raw emails contain personal data (names, addresses) and commercial secrets. Proposal: keep raw email and attachments for a fixed period (default 12 months) and delete a tenant's content on request. Resend and the LLM providers are processors that need to be named in the pilot agreements. **[business] decide the period and the consent wording with the pilot companies.** This is the "consent to send tenant data to third-party APIs" item in the plan.

## 8. Open questions

| # | Question | Owner | Default if unanswered |
|---|---|---|---|
| 1 | Forwarding behaviour on the four real mailboxes | Marcelo, pilot companies | Test in week 1 |
| 2 | Sender identity A or B | Amanda, Marcelo | A |
| 3 | Rendering location A, B or C | Amanda, Marcelo | A, after the one-hour check |
| 4 | Resend plan limits: attachment size, inbound volume, verified domains | Amanda | Verify before launch |
| 5 | Per-tenant sender allow-list | Pilot companies | Open (no allow-list) |
| 6 | Retention period and deletion process | Pilot companies | 12 months |
| 7 | Virus scanning | Pedro | Not in the MVP |
| 8 | Resend retry behaviour for non-2xx responses | Pedro | Verify before relying on it (ADR-005) |
| 9 | Who may activate a tariff batch: `ADMIN`/`OWNER` only (matches the ADR-005 matrix) or `PRICING_ANALYST` too? | Pedro and Marcelo | `ADMIN`/`OWNER` only (proposed) |

## 9. Impact

- **Data model v1.2:** `kind` and `IGNORED` status on RFQs; RFC `Message-ID` and header columns; batch status `PENDING_REVIEW`; batch `activated_at`; tariff row `is_removed` and natural-key index; quote `priced_at` (see step 3, tariff versioning); suppression list; per-tenant caps.
- **ADR-004:** stage 1 gains the immediate-download rule and the skip rules; stage 5 and 7 gain the rendering and sender decisions.
- **`packages/contracts`:** a render request and response, with money as strings.

## 10. Acceptance for moving to an ADR

Sections 3.1 and 4 decided; the forwarding test done on at least two real mailboxes; Resend plan limits recorded.
