# Deploying at a customer

How Close Copilot would go into a real company's month-end close, and what it can and can't do today. Nothing here has been done with a real customer. Every run so far used the two synthetic companies in `config/companies/`. Steps the code doesn't support yet are marked **future work**. Deploying the software itself is covered in [deploy.md](deploy.md).

## 1. Discovery questions

Ask these before installing anything. The answers decide whether the current checks fit, what the company profile must say, and what an adapter has to produce.

**Chart of accounts**

- Which ERPNext company or companies close, and is each a separate legal entity with its own GSTIN?
- Which accounts are the bank accounts, the payment-gateway clearing account, bank charges, prepaid expenses and fixed assets? Do the names follow the standard India chart or a custom one?
- What capitalisation threshold, prepaid-spread rule and cut-off rule does the company apply? These go into `config/rules.yaml`.
- Has every earlier fiscal year's profit and loss been closed to equity? The trial balance tool refuses an unbalanced opening.

**Bank**

- Which banks, and how many accounts per company? In what format does each statement arrive (CSV, Excel, PDF, MT940, a bank API), and how often?
- Does the narration carry the payment reference that the books record? Which charges does the bank post (SMS, account maintenance, NEFT and RTGS fees, GST on charges)?
- Is there a payment gateway, does it settle net of fees, and is there a settlement report?

**GST**

- What is the filing cadence (monthly or quarterly, QRMP)? Who downloads GSTR-2B, and when in the month?
- How are late supplier filings handled today: deferred credit, follow-up with the supplier, or ignored below an amount?
- Which suppliers are unregistered, composition dealers or sole proprietors? A sole proprietor's GSTIN carries a personal PAN. Such suppliers must be marked `individual` in the profile so their names are pseudonymised too.

**Approval hierarchy**

- Who prepares adjusting entries, and who approves them? Is there an amount above which a second approver is needed?
- Can the same person ever both prepare and approve? Close Copilot refuses self-approval and has one checker level only.
- Who should see which company? Per-user company lists don't exist yet (**future work**), so today every signed-in user sees every configured company.

**Close calendar**

- What is the close deadline (business day N), and which steps happen on which day?
- Which checks are done today, by whom, and how long does each take? This is the baseline for minutes saved.
- What list of errors did the last few closes find? Collect it in writing. It is the customer's own ground truth for shadow mode.

**ERPNext version and customisations**

- Which ERPNext and Frappe version, and is India Compliance installed? Close Copilot is built and tested against version-15 with India Compliance only.
- What custom fields, custom DocTypes, server scripts or workflows touch Purchase Invoice, Payment Entry, Journal Entry or GL Entry?
- Is ERPNext self-hosted or Frappe Cloud, and can a new API user be created with a narrow role?
- Is the audit trail on? India Compliance can't turn it off once it is on.

## 2. Read-only credentials first

Start with the read path only, and add the write path last.

What the read path needs:

- **An ERPNext API user with read access to the books.** The development set-up uses `copilot-bot`, an Accounts User created by `deploy/erpnext/api-users.sh`. Run `cmd/probe auth` and `cmd/probe perms` to confirm that the key logs in and has the read access and the refusals the books tools expect. The books server uses this key (`ERP_API_KEY`, `ERP_API_SECRET`) for every read tool.
- **A company profile** in `config/companies/<id>.yaml`. It maps the short company ID that every tool uses to the ERPNext company name (`erp_company`), and names the bank account and the individual suppliers. Today's profiles also carry the seeder's generation settings. A customer profile would need only the fields the checks and the pseudonymiser read (**future work**: a profile format without seeder fields).
- **Postgres, TEI and the MCP servers** as in [deploy.md](deploy.md), with fresh `MCP_TOKEN_AGENT`, `MCP_SCOPE_KEY` and `PSEUDONYM_KEY` values.

Why the admin token comes last:

- Nothing reaches the ledger without the admin route. `agent serve` runs without `MCP_TOKEN_ADMIN`, and its proposal and approval routes then answer 501, so makers and checkers can review findings but nothing can post.
- The write path needs a second ERPNext user (`copilot-poster`) whose role allows only Journal Entry read, write, create and submit. The books server holds its key (`ERP_POST_API_KEY`) and uses it only on `/mcp-admin`. Only `internal/approvals` holds `MCP_TOKEN_ADMIN`.
- Today `mcp-books` over HTTP refuses to start without `MCP_TOKEN_ADMIN` and the posting key pair. Until a read-only books server mode exists (**future work**), a shadow deployment would give the books server those values but keep `MCP_TOKEN_ADMIN` out of `agent serve`. It would also leave the poster user's role without submit rights in ERPNext, so a post would fail even if attempted.

## 3. Connecting a real bank feed and the GSTR-2B export

`cmd/load` is the only way evidence gets into Postgres. Today it reads two file formats, one file per company and month, and a load replaces that company-month's rows in one transaction.

**Bank statement** (`load bank --company <id> --month YYYY-MM --file bank.csv`):

- A CSV with exactly this header: `txn_id,date,value_date,narration,ref,withdrawal,deposit,balance`.
- Dates are `YYYY-MM-DD`. Exactly one of `withdrawal` and `deposit` is set on each row, as a positive rupee amount, and it is stored as signed paise.
- `txn_id` is the row's key within a company, so it must be unique and stay the same across reloads. The loader checks that the first line's opening balance follows on from the previous month's closing balance.
- A malformed row rejects the file and names its line number.

**GSTR-2B** (`load gstr2b --company <id> --month YYYY-MM --file gstr2b.json`):

- A simplified form of the portal's GSTR-2B JSON. Only `data.rtnprd` (`MMYYYY`, which must equal the month) and the B2B section are read: `data.docdata.b2b[]` with `ctin` and `trdnm`, and for each invoice `inum`, `dt`, `val`, `txval`, `igst`, `cgst`, `sgst` and `itcavl`.
- Amounts are decoded as exact decimals and stored as paise. Invoice numbers are normalised for matching.
- Other sections of the real export (credit notes, amendments, ISD, imports) are ignored (**future work**).

What an adapter would have to produce (**future work** for every bank and for the portal download):

- A converter from each bank's statement format to the CSV above. It needs stable transaction IDs, the bank's own reference in `ref` where there is one, and running balances. For a PDF statement this also means a reliable extraction step.
- A converter from the portal's GSTR-2B JSON or Excel download to the simplified JSON, keeping the B2B section. A direct GST portal API connection is out of scope until a GSP integration is decided.
- A schedule that loads each month's files before the close run starts. Preflight refuses a run whose month has no bank lines loaded.

## 4. Data residency

| Component | Where it runs | What it holds |
| --- | --- | --- |
| ERPNext | The customer's existing instance | The books; Close Copilot only reads them, and posts only approved entries |
| Postgres | Inside the deployment, on an internal-only network in the production compose file | Bank lines, GSTR-2B entries, document chunks, findings, prompts and responses, the audit log |
| TEI (embeddings and rerank) | Inside the deployment; offline after the one-time model download | Nothing persistent |
| docling | Inside the deployment, only while ingesting | Nothing persistent |
| Model provider | Outside: `LLM_PROVIDER=claude-cli` runs the local `claude -p`, and `LLM_PROVIDER=anthropic` calls the Anthropic API | Receives pseudonymised prompts only |
| Traces | Langfuse Cloud over OTLP, if configured | Spans without prompts (`TRACE_PROMPTS` is `false` in the production compose file) |

- **Pseudonymisation before model calls.** GSTINs, PANs and the names of suppliers marked `individual` are replaced with keyed per-run tokens before every model call, and stored prompts hold only tokens (CC-710). Amounts, account names and company suppliers' business names are sent as they are. The customer should agree to that explicitly.
- **Known gaps** must be fixed before real data: an identifier glued to other letters or digits, or written in lower case, is not masked, and evidence strings are cut before masking. See the README's Known limitations.
- `claude-cli` is a development convenience tied to one person's Claude plan. A customer deployment would use `anthropic` with a key and a spend limit in a workspace the customer controls. The Anthropic provider has not been used for any recorded measurement yet.
- **Keeping all inference in India or on-premises** would need a provider implementation for a model the customer hosts. `internal/llm` has a provider interface, but only `claude-cli` and Anthropic exist (**future work**).

## 5. Shadow-mode rollout

Run Close Copilot beside the human close, without the write path, for at least three monthly closes, so a quarterly GST cycle and the variance check's three-month window are both covered.

1. **Month 0, set-up.** Do discovery, set up the read-only user and profile, load the last three months of bank and GSTR-2B files, and ingest the policy manual, contracts and close notes. Run closes on past months and compare them with the errors the team actually found then.
2. **Months 1 to N, in parallel.** The team closes as usual. Close Copilot runs each month's close as soon as the files are loaded. No admin token is configured. Accountants mark each finding accept or dismiss in the UI, and keep their own list of errors as usual.
3. **After each month, compare.** Every error on the accountants' list is either caught by a finding or missed. Every finding is either on their list, a real issue they hadn't found, or a false alarm. Record why each miss and false alarm happened: data format, a check rule, a profile setting, or an explanation the verifier rejected.
4. **Go or no go for the write path.** Enable the admin token and the posting user only when the owner and the customer's approver agree on the numbers below. Start with one entry type, such as bank charges. Each enabled type needs its maker-checker pair named.

## 6. Success metrics

Measure on the customer's own months, with the customer's list as ground truth, and report counts with their denominators, not only percentages.

| Metric | Definition | Source |
| --- | --- | --- |
| Recall | Errors on the accountants' list caught by a finding, out of all errors on the list | Shadow-mode comparison |
| Precision | Findings the accountants accept, out of all scored findings | Accept and dismiss in the UI |
| False-alarm rate | Dismissed findings per close, and any finding in a month the team considers clean | Accept and dismiss in the UI |
| Verified rate | Findings whose explanation passed the verifier and isn't flagged `needs_review`, out of all explained findings | `eval score`-style counts over the runs |
| Minutes saved | Time the team spends on the covered checks per close, before against during shadow mode | Self-reported per close against the discovery baseline |
| Time to close | Business days from month end to sign-off | The customer's close calendar |

The synthetic suites set no expectations for these numbers. A real chart of accounts, real narrations and real filing behaviour will find failure modes the planted errors don't have.
