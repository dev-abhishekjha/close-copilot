# POL-001 Accounting Policy Manual

Version 3.2. Applies to every company in the group that closes its books on ERPNext. All companies, names and figures in this manual are synthetic.

Owner: Group Finance Controller. Approved by: the Audit Committee. Review cycle: every twelve months, or earlier when a statutory rule changes.

This manual tells the finance team how to treat the ordinary transactions of a month, how to close the books, and who may approve what. Each rule is written in one paragraph under its own numbered heading so that it can be cited exactly. When a figure in this manual differs from a figure in a working paper, this manual is correct.

## 1. Scope

### 1.1 Companies covered

This policy applies to every legal entity whose ledger is kept in the group ERPNext site. Each entity keeps its own books, its own bank account and its own GST registration, and each closes its own month. Nothing in this manual allows one entity to book a cost on behalf of another.

### 1.2 Who must follow it

The policy binds the accounts payable clerk, the accounts receivable clerk, the bank reconciliation owner, the GST owner, the preparer of journals (the maker) and the reviewer of journals (the checker). It also binds the automated tools that help the team: a tool may propose an entry, but it never posts one, and every proposal is reviewed under section 9.

### 1.3 Basis of accounting

Books are kept on the accrual basis in Indian rupees, in whole rupees on reports and in paise in the ledger. Revenue and expenses are recognised in the period they relate to, not in the period cash moves. Where this manual is silent, the team follows the Indian Accounting Standards as applicable to the entity, and asks the Group Finance Controller when in doubt.

### 1.4 Materiality

A difference smaller than one rupee on a single document is a rounding difference and is left alone. Every other difference is explained before the month is closed. The team never nets two unrelated differences against each other to make a balance look clean.

### 1.5 Definitions

- **Close**: the set of tasks that finishes with the month locked against further entries.
- **Period**: one calendar month, written YYYY-MM.
- **Maker**: the person or tool that prepares an entry.
- **Checker**: a different person who approves it.
- **Recurring supplier**: a supplier that billed in most of the recent months, described in section 5.

## 2. Close calendar

### 2.1 Working days

The close runs in the first six working days after month end. Working day one is the first weekday after the month ends. A public holiday in the state of the registered office is not a working day. The table shows the target for each task.

| Working day | Task | Owner |
| --- | --- | --- |
| 1 | Cut-off review of purchases and sales | Accounts payable and receivable |
| 2 | Bank statement loaded and reconciled | Bank reconciliation owner |
| 3 | Accruals, prepaid releases and fixed asset checks | General ledger accountant |
| 4 | GST reconciliation against GSTR-2B | GST owner |
| 5 | Variance review and journal approvals | Controller |
| 6 | Month locked, close note written | Controller |

### 2.2 Order of work

Cut-off comes first because every later step depends on knowing which documents belong to the month. The bank reconciliation comes before accruals because a cleared payment shows that a bill is no longer accrued. GST comes after accruals because the credit depends on which invoices are in the books. Nobody starts a step whose input is not final unless the Controller agrees and notes it.

### 2.3 The close note

On the last day of the close the Controller writes a short close note for the month. It records what was unusual, which suppliers billed late, which annual costs fell due, and any decision that future months should follow. The note is kept with the month's working papers and is read at the start of the next close.

### 2.4 Locking the month

After the Controller signs the close note the month is locked. A correction after locking is made in the current open month with a clear reference to the original entry, never by reopening the locked month, unless the Audit Committee allows it in writing.

## 3. Fixed assets

### 3.1 Capitalisation threshold

An item that will serve the business for more than one year and costs ₹50,000 or more, before GST that can be claimed as credit, is capitalised as a fixed asset and depreciated; an item below ₹50,000 is expensed in the month it is bought.

<!-- rule:capitalisation.threshold_inr=50000 -->

### 3.2 What counts as an asset

Laptops, printers, servers, furniture and air conditioners are the usual assets. The test is the nature of the item, not the account it was first booked to. A laptop that was paid for through a repairs invoice is still a laptop, and a purchase that looks like repairs and maintenance but adds a new item of equipment above the threshold in 3.1 is capitalised.

### 3.3 Cost of an asset

The cost includes the purchase price, import duty, freight, installation and any other cost needed to bring the asset to working condition. Discounts are deducted. GST that the company can claim as input tax credit is not part of the cost; GST that cannot be claimed is.

### 3.4 Bundles and part payments

Several items bought together on one invoice are judged one at a time against the threshold in 3.1, unless they only work as a set, such as a server and its rack, in which case they are judged together. A part payment is booked as an advance and capitalised when the asset is delivered.

### 3.5 Depreciation

Depreciation starts in the month the asset is ready for use and follows the rates in the asset category. A full month is charged for any asset ready on or before the fifteenth, and none for one ready after it. The fixed asset register is updated on working day three of the close.

### 3.6 Repairs and maintenance

Routine repair and upkeep of an existing asset is an expense. A repair that extends the life or capacity of an asset beyond its original standard is capitalised. The accountant checks every invoice booked to repairs and maintenance that mentions an asset keyword or is above the threshold in 3.1, and moves it to the asset account when it qualifies.

### 3.7 Disposal

When an asset is sold or scrapped the gain or loss is the difference between proceeds and the carrying value on the date of disposal. The disposal is approved under section 9 before it is posted.

### 3.8 Worked example

A company buys a laptop for ₹62,000 before GST on a day in the middle of a month. The laptop will be used for several years and the price is above ₹50,000, so it is booked to the computer equipment account, entered in the fixed asset register and depreciated from the month it is ready for use. A second laptop bought the same day for ₹38,000 is below the threshold and is expensed. The accountant records both decisions in the working paper with the invoice numbers.

## 4. Prepaid expenses

### 4.1 What is a prepaid expense

A cost paid or invoiced for a period that extends beyond the month of the invoice is a prepaid expense. Annual software licences, yearly insurance premiums, annual maintenance contracts and renewal fees are the usual examples. The invoice text often says annual, yearly, renewal or twelve months, and the team reads it for those words.

### 4.2 Spread annual costs monthly

An annual cost of ₹50,000 or more that is invoiced upfront is booked to a prepaid asset account and released to expense in equal monthly amounts over the months of the service period, starting with the month the service begins.

<!-- rule:prepaid.min_amount_inr=50000 -->

### 4.3 Small amounts

An annual cost below ₹50,000 may be expensed in the month of the invoice, because the monthly effect is too small to matter. The Controller may still require spreading for a cost that is repeated across many suppliers.

### 4.4 The release journal

The release journal is prepared on working day three of every close. It names the original invoice, the service period, the monthly amount and the months remaining. The last month takes the rounding so that the full invoice is released and nothing remains on the prepaid account.

### 4.5 Early termination

When a service ends early, the remaining prepaid balance is expensed in the month of termination, unless the supplier refunds it, in which case the refund is booked against the prepaid account.

### 4.6 Worked example

An annual software licence of ₹3,60,000 before GST is invoiced in April for the twelve months from April. The invoice is booked to the prepaid account. Each month from April the accountant releases ₹30,000 to the software expense account. After the March release the prepaid balance is nil. If the same invoice had been expensed in April, April would show a cost twelve times too high and every other month would show none, and the variance review in section 10 would flag both.

## 5. Accruals

### 5.1 Accrue recurring suppliers at their usual amount

When a recurring supplier has not billed for a month in which it delivered the service, the cost is accrued at the supplier's usual amount, using the average of the recent months, and the accrual is reversed when the real bill arrives.

<!-- rule:accruals.lookback_months=4 -->
<!-- rule:accruals.min_occurrences=3 -->
<!-- rule:accruals.amount_band_pct=20 -->

### 5.2 Who is a recurring supplier

A supplier is recurring when it billed in at least 3 of the last 4 months, with amounts within 20% of one another. Rent, utilities, telecom, broadband and standing services such as security and housekeeping are the usual examples. A supplier that billed once, or whose amount swings by more than 20%, is not accrued automatically; the accountant decides case by case.

### 5.3 Typical bill timing

Some bills always arrive after month end. An electricity bill usually arrives around the eighth of the next month, and a telephone bill around the middle of it. The close notes record each supplier's usual arrival day so that the accountant knows whether a missing bill is late or merely normal.

### 5.4 Reversal

An accrual is reversed on the first day of the next month. The real invoice is then booked normally. If the real invoice differs from the accrual, the difference falls in the month the invoice is booked and is not chased back.

### 5.5 Evidence

Every accrual journal names the supplier, the month, the amounts of the months it was based on, and the reason the bill is missing. A journal without that evidence is returned to the maker.

### 5.6 Worked example

An electricity supplier billed ₹30,000, ₹33,000, ₹36,000 and ₹31,000 in the last four months, so it is a recurring supplier. Its bill for the month just ended has not arrived, and the close note says the bill usually arrives around the eighth of the next month. The accountant accrues the average of the four months, ₹32,500, with a journal that quotes those amounts. When the real bill arrives the accrual is reversed and the invoice is booked.

## 6. GST and input tax credit

### 6.1 Claim credit only for invoices in GSTR-2B

Input tax credit is claimed only for a purchase invoice that appears in the company's GSTR-2B for the month, with a tax amount that agrees with the books to within ₹1; credit for an invoice that is missing from GSTR-2B, or whose tax differs by more than that, is held back until the supplier has filed or corrected it.

<!-- rule:gstr2b.amount_tolerance_paise=100 -->

### 6.2 Follow up with suppliers

For each invoice held back under 6.1 the GST owner writes to the supplier within two working days, quotes the invoice number and date, and records the reply. An invoice that is still missing at the next close is escalated to the Controller, who decides whether to keep waiting, change supplier or write the tax off.

### 6.3 Matching

The GST owner matches books to GSTR-2B by supplier GSTIN, invoice number, invoice date and amounts. The comparison is for each invoice, not in total. A small difference on one invoice is never offset by an opposite difference on another.

### 6.4 Place of supply and the tax split

A supplier in the same state as the company charges CGST and SGST in equal halves. A supplier in another state charges IGST. A mismatch between the supplier's state and the tax charged is queried before credit is claimed.

### 6.5 Unregistered suppliers

A supplier that is not registered for GST does not charge GST and has no GSTIN. No credit is claimed on its bills. The accountant must not book tax on such a bill, and the bill is checked for the reverse charge rules where they apply.

### 6.6 Blocked credits

Credit is not claimed for items the law blocks, such as personal use items or goods lost or stolen. The accountant books the full amount, tax included, to the expense.

### 6.7 Returns

The GST owner files the returns on the due dates. A return is prepared from the locked books and reviewed by the Controller before filing. Any difference between the return and the books is explained in writing.

### 6.8 Worked example

The books hold five purchase invoices from one supplier in a month. GSTR-2B lists four of them. The fifth is held back, no credit is claimed for it in the month, and the GST owner writes to the supplier. If the supplier files later, credit is claimed in the month the invoice first appears in GSTR-2B. If an invoice appears in GSTR-2B with tax ₹2 different from the books, the difference is above the ₹1 limit and the credit is held back too, until the supplier or the books are corrected.

## 7. Bank reconciliation

### 7.1 Book bank charges monthly

Every month the bank's charges for the current account, such as account maintenance, transaction fees and cheque or transfer charges, are booked to the bank charges expense account on the date the bank debited them, and the entry is made in the month the bank debited, whether or not a debit advice has arrived.

<!-- rule:bank_match.date_window_days=3 -->

### 7.2 Book gateway settlements with fee and GST

A payment gateway settles to the bank after deducting its fee and the GST on that fee. The settlement is booked as one entry with three parts: the net amount received in the bank, the gateway fee to the gateway fee expense account, and the GST on the fee to input tax credit, with the gross amount cleared against the customer receipts it settles.

### 7.3 Matching rule

A bank transaction is matched to a ledger entry when the amount agrees exactly and the dates are within 3 days of each other, taking the later of the two dates as the match date. A transaction with no ledger entry within that window is unmatched and is explained before the month is closed.

### 7.4 Unmatched items

Every unmatched item is listed on the reconciliation with its date, amount, and the action to be taken. A debit on the bank statement with no ledger entry is usually a charge or a missing payment entry. A credit with no ledger entry is usually a customer receipt or a gateway settlement not yet booked.

### 7.5 Reconciliation statement

The reconciliation ends with a statement that shows the closing balance per bank statement, the items in transit, and the closing balance per books. The two sides must agree after the items in transit. The statement is attached to the close note.

### 7.6 Cheques and transfers in transit

A payment made on the last days of the month that clears in the next month is an item in transit. It is booked in the month it was issued and appears on the statement until the bank clears it.

### 7.7 Worked example

A gateway settles ₹97,640 into the bank for sales of ₹1,00,000. The gateway kept a fee of ₹2,000 and GST of ₹360 on the fee. The accountant books a debit of ₹97,640 to the bank, ₹2,000 to the gateway fee expense and ₹360 to input tax credit, and a credit of ₹1,00,000 to the gateway receivable account that holds the customer receipts. The bank statement line then matches the ledger line for the net amount.

## 8. Cut-off

### 8.1 Expenses belong to their service period

An expense is recorded in the month in which the service was delivered or the goods were received, and not in the month the invoice is dated or paid; an invoice for a service delivered in one month and booked in the next is moved back to the month of service during the close of that month.

### 8.2 Purchases

On working day one the accounts payable clerk reviews every purchase invoice booked in the first week of the new month and every goods receipt of the last week of the old month. An invoice that relates to the old month is booked to the old month, with an accrual if the invoice has not yet arrived.

### 8.3 Sales

A sale is recognised when the goods are delivered or the service is performed. An invoice raised in advance of delivery is deferred. The receivables clerk checks the dispatch records of the last week of the month against the sales invoices.

### 8.4 Payroll

Salaries are expensed in the month the employees worked, even when they are paid on the last day or in the first days of the next month. A bonus or arrear is expensed in the month it is earned.

### 8.5 Duplicate invoices

When a supplier's invoice is booked twice, the second entry is cancelled and the supplier's contract is checked for the agreed amount and billing frequency, so that the right entry is the one kept. A duplicate is never settled by paying twice and asking for a refund.

### 8.6 Worked example

A cleaning company delivers its service in the last week of a month and its invoice, dated the third of the next month, reaches the accounts payable clerk on the fifth. On working day one the clerk sees the invoice describes the service of the earlier month, books it to that month, and the cost appears in the right period. If the invoice had not yet arrived, the clerk would have accrued the cost under section 5.

## 9. Journal approvals

### 9.1 Maker and checker

Every manual journal has a maker and a checker, and they are different persons. The maker prepares the entry with its evidence. The checker reads the evidence, confirms the amounts and approves or rejects it. Nobody approves their own entry, and a tool that proposes an entry counts as a maker.

### 9.2 Approval limits

A checker who is an accountant may approve a journal of up to ₹1,00,000. A journal above that, up to ₹10,00,000, needs the Controller. A journal above ₹10,00,000, and any journal that touches the retained earnings, needs the Group Finance Controller.

### 9.3 Evidence

The journal states the reason, names the documents it is based on and quotes the section of this manual that applies. The checker rejects a journal whose figures cannot be traced to a document.

### 9.4 Posting

Only an approved journal is posted. The posting is made under the checker's approval and carries the approval reference. A rejected journal is returned to the maker with the reason.

### 9.5 Reversals and corrections

A journal is corrected by a reversing entry and a new entry, never by editing or deleting the original. The reversal follows the same maker and checker steps.

## 10. Variance review

### 10.1 Month-on-month review

On working day five the Controller compares each expense and income account with the previous month. A change of more than 25% that is also more than ₹50,000 in amount is investigated and explained in the close note.

<!-- rule:variance.pct_threshold=25 -->
<!-- rule:variance.abs_threshold_inr=50000 -->

### 10.2 Explaining a variance

An explanation names the cause, such as a new supplier, a one-time purchase, a price change or a cut-off error, and points to the documents. An explanation of the type the cost was higher this month is not accepted.

### 10.3 Action

A variance that arises from an error is corrected under section 9. A variance that is real and expected is noted. A variance that cannot be explained is escalated before the month is locked.

## 11. Records

### 11.1 Retention

Books, vouchers, bank statements, GST returns and close notes are kept for the period the law requires, and in any case not less than eight years. Documents are stored in the document system with the month and the company, and are not deleted.

### 11.2 Access

Access to the ledger is given by role. Writing to the ledger needs approval under section 9. Reading is open to the finance team of the company and to the auditors.

### 11.3 Changes to this manual

A change to this manual is proposed by the Group Finance Controller, reviewed by the Audit Committee and takes effect from the first day of a month. The previous version is kept. A figure that is changed in the working rules of the finance system is changed here at the same time.

## Appendix A. Summary of figures

The figures below repeat the rules in the numbered sections. If a figure here differs from its section, the section is correct.

| Topic | Section | Figure |
| --- | --- | --- |
| Capitalise an asset | 3.1 | ₹50,000 or more |
| Spread an annual cost | 4.2 | ₹50,000 or more |
| Recurring supplier history | 5.2 | 3 of the last 4 months |
| Recurring supplier amount band | 5.2 | within 20% |
| GSTR-2B tax tolerance | 6.1 | ₹1 per invoice |
| Bank matching date window | 7.3 | 3 days |
| Variance review | 10.1 | more than 25% and more than ₹50,000 |
| Accountant approval limit | 9.2 | up to ₹1,00,000 |
| Controller approval limit | 9.2 | up to ₹10,00,000 |

## Appendix B. Document control

| Version | Change | Approved by |
| --- | --- | --- |
| 3.0 | Added the bank charge and gateway settlement rules | Audit Committee |
| 3.1 | Added the variance review and the worked examples | Audit Committee |
| 3.2 | Clarified cut-off for payroll and the treatment of duplicate invoices | Audit Committee |

Questions about this manual go to the Group Finance Controller. The team never relies on memory or on an old copy: it reads the current version before each close.
