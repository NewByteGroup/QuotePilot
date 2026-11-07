# ADR-006: AI agents architecture in ai-engine (Jev decision layer + structured LLM extraction)

- **Status:** Approved
- **Owner:** @Marcelo
- **Date:** 2026-11-07
- **Deciders:** @Pedro  @Amanda Ballet

## Context

Per ADR-001, `apps/ai-engine` is a stateless Python 3.12 service with no database or storage credentials, no `organization_id`, no payload logging, and no cross-request cache. Per ADR-004, the async pipeline calls `ai-engine` for cognitive work in **Stage 2 (`Classify`)** and **Stage 3 (`Extract`)**, while **Stage 4 (`Price`)** runs as a pure, deterministic Go package (`internal/pricing`) with zero I/O and zero LLM calls.

This creates two strict engineering requirements for our AI agents:

1. **Calibrated, injection-proof decisions for routing and confidence:** Standard generative LLMs are poorly calibrated when self-reporting confidence (often outputting `.95+` while hallucinating) and are vulnerable to indirect prompt injection from external customer emails.
2. **Complete customs normalization during Stage 3 (`Extract`):** Because Stage 4 (`Price`) does not call an LLM, Stage 3 must normalize informal port/airport mentions into official `UN/LOCODE`s, validate `NCM` codes, standardize units (`0GP`, `40HC`, `WM_TON_M3`, `KG`), and anchor exact text quotes for the Split-Screen UI.

## Decision

**Use a two-tier cognitive architecture inside `apps/ai-engine/agents/`: TypeSafe's Jev (`System One API`) for calibrated probability decisions (classification, routing, and confidence auditing), paired with a `temperature=0` structured-output LLM (Pydantic v2) for multimodal field extraction and customs normalization.**

### 1. Stage 2 (`Classify`) — Jev Decision Model (`agents/classifier.py`)

Instead of a generative LLM, Stage 2 uses **Jev** (`System One API` — cloud inference in production, compatible with Ollama `/v1/systemone` locally using open-weight decision models like `nimble` or `tev1`).

- **Why Jev:** Jev reads up to 32K tokens and answers narrow typed questions (`Choice`, `Score`, `Noul`) in a single parallel pass, returning calibrated probabilities rather than prose. Because it cannot generate free text or tool calls, structured-output errors and prompt-injection takeovers drop to zero by construction.
- **Parallel questions evaluated per email:**
    - `intent_kind`: `Choice(["RFQ", "TARIFF_UPDATE", "OTHER"])`
    - `has_origin_and_destination`: `Noul()`
    - `has_cargo_metrics`: `Noul()`
    - `has_carrier_rate_sheet`: `Noul()`
    - `is_marketing_or_autoresp`: `Noul()`
    - `modal_hint`: `Choice(["FCL", "LCL", "AIR", "UNKNOWN"])`
- **Auditable routing in code:** The probability thresholds that map Jev's output to `RFQ`, `TARIFF_UPDATE`, or `OTHER` (`status = IGNORED`) live as versioned code in the repository.

### . Stage 3 (`Extract`) — 3-Step Deterministic DAG (`agents/rfq_extractor.py`)

When Stage 2 classifies an email as `RFQ`, Stage 3 runs a strict three-step pipeline:

1. **Step 3.1 — Attachment-to-Markdown (`Attachment Parser`):**
    - Converts PDF (*Commercial Invoice*, *Packing List*), XLSX/CSV, and DOCX bytes passed in the request into clean `parsed_markdown` preserving table structures.
2. **Step 3.2 — Structured Extraction & Public Reference Normalization (`RFQ Extractor`):**
    - Wraps email body and `parsed_markdown` inside `<untrusted_customer_email>` XML tags.
    - Uses a `temperature=0` frontier LLM constrained by generated Pydantic v2 schemas (`packages/contracts/`).
    - **Public-only tools (ADR-001 Rule 4):** The extractor may query only in-memory static reference tables bundled in `apps/ai-engine/reference_data/` (`UN/LOCODE` dictionary with aliases, official 8-digit `NCM` table, Incoterms 2020).
    - Outputs normalized `origin_locode`, `dest_locode`, `incoterm`, `modal`, `equipment_or_unit`, `ncm`, gross weight, volume ($m^3$), cargo value/currency, canonical `search_description`, and its `VECTOR(1024)` embedding (`embedding_model`).
    - Requires an `exact_quote` and `source_origin` (`EMAIL_BODY` or `ATTACHMENT:<name>`) for every extracted field to drive Split-Screen UI highlights.
3. **Step 3.3 — Calibrated Confidence Auditing (`Confidence Auditor` via Jev):**
    - Rather than trusting the generative LLM's self-reported confidence, Step 3.3 passes the original text + extracted fields + `exact_quote`s to **Jev** to evaluate rubric questions per field (`Noul` / `Score`: *"Is the origin port explicitly stated in the quote without geographic inference?"*, *"Are weight and volume explicit rather than estimated?"*).
    - Produces calibrated `field_confidence` and `extraction_confidence` (`NUMERIC(5,4)`). Any inferred field scores `< 1.0000`, flagging the line in amber for mandatory human review (`NEEDS_REVIEW`) in Stage 4.

### 3. Private Tariff Structuring (`agents/tariff_parser.py`)

For `TARIFF_UPDATE` emails and CSV/XLSX uploads, extracts and normalizes rows into the exact `private_tariffs` contract (`carrier_or_agent`, `modal`, `charge_category`, `charge_code`, `origin_locode`, `dest_locode`, `equipment_or_unit`, `currency`, `net_amount`, `min_amount`, `valid_from`, `valid_until`, `search_description`, `embedding_model`, `embedding` 1024d) for append-only insertion by `core-api`.

### 4. Prompt Injection & Security Guardrails

1. **Zero privilege by architecture:** Even if an adversarial email manipulates the extraction LLM, `ai-engine` has no database access, no S3 access, and no awareness of any tenant.
2. **Untrusted output validation (ADR-001 Rule 5):** `core-api` validates all returned LOCODEs, enums, numeric bounds, and date ranges (`valid_from <= valid_until`) against `packages/contracts/` before persisting.
3. **Zero-payload logging (ADR-001 Rule 2):** FastAPI middleware logs only `correlation_id`, route, latency, status code, and token counts — never request or response bodies.