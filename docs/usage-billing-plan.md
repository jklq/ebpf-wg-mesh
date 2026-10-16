# Usage billing plan

Research date: October 15, 2026. Status: proposed design. This plan assumes that the operator sells hosted application resources to customers.

**Recommendation:** use Railway's product model, Linux resource counters, the Go control plane, PostgreSQL, and Stripe Invoicing. Keep resource measurement, prices, credits, and invoice statements under platform control. Start with one paid plan and a small trial. Prove the meters before the first real charge.

1. **Use Railway as the product reference.**

   Railway charges a monthly minimum that includes an equal amount of resource usage. Its published container rates provide these comparison points. [Railway plans](https://docs.railway.com/pricing/plans)

   | Item | Railway reference |
   | --- | --- |
   | Hobby minimum | $5/month, with $5 of usage |
   | Pro minimum | $20/month, with $20 of usage |
   | CPU | $20/vCPU-month |
   | Memory | $10/GB-month |
   | Volumes | $0.15/GB-month |
   | Public egress | $0.05/GB |

   Railway advertises compute charges per second. Its volume documentation specifies a minute meter for actual used storage, with filesystem metadata. [Railway pricing](https://railway.com/pricing), [Railway volumes](https://docs.railway.com/volumes/reference)

   Railway exposes resource limits, email thresholds, and a hard limit that takes workloads offline. Private connectivity can avoid public egress charges. [Railway cost control](https://docs.railway.com/pricing/cost-control)

   These facts describe customer behavior. They do not establish Railway's internal meter, database, or payment architecture. The implementation below is a proposal for this repository.

2. **Build on the current resource model.**

   | Existing code | Reuse and required change |
   | --- | --- |
   | `CONTEXT.md` | Preserve Project → Environment → Service → Deployment → Allocation. Add a separate payer concept. |
   | `internal/agent/host_cgroup_linux.go` | Production already requires cgroup v2. Read counters from allocation cgroups. |
   | `internal/agent/runtime_containerd_engine_spec_linux.go` | CPU and memory fields enforce limits. They do not measure consumption. |
   | `internal/agent/local_state.go` | Reuse durable identity and restart patterns for a separate usage journal. |
   | `internal/volumestore/loop_linux.go` | Reuse the filesystem usage calculation. Preserve exact bytes before display rounding. |
   | `internal/controlplane/delivery/volumes.go` | Current volume observations are a live view, not durable invoice evidence. |
   | `internal/firewall/firewall.c` | Add byte counters beside allocation identity and packet policy. |
   | `internal/controlplane/authz/authz.go` | Add payer authorization beside the existing project scopes. |
   | `internal/controlplane/journal/` | Include payer references and execution restrictions in durable product state. |
   | `internal/controlplane/durablework/` | Reuse transactional enqueue, deduplication, retries, and fenced claims for invoice work. |
   | `internal/controlplane/logs/`, `internal/logpipeline/` | Reuse design experience. Their deliberate drop paths exclude them as the usage ledger. |
   | `internal/recovery/` | Extend recovery inventory and validation to financial records and producer receipts. |
   | `console/src/lib/dashboard/` | Keep browser authentication here. Keep financial authority in Go. |

   Add `internal/controlplane/billing/` as one deep module. Its interface hides invoice arithmetic, credits, settlement, retries, and provider state.

   Add `internal/metering/` for agent measurement, durable interval records, and delivery. Keep Linux counter details private to its implementation.

   Keep delivery decisions in `delivery`. A narrow execution-policy seam supplies the account restriction and reason. Delivery owns allocation and ingress changes.

   The Stripe adapter belongs inside the billing module. Use the official Go SDK with a pinned stable API version. Add no generic payment-provider framework.

3. **Define the payer and charge rules first.**

   A **Billing Account** owns payment details, a plan, credits, invoices, and a budget. Each user project belongs to exactly one Billing Account.

   Initially, each user owns one Billing Account. All projects that the user creates share its minimum and usage allowance. Project membership still controls project access.

   Only the account owner can change payment details, the plan, or account limits. Project viewers can inspect their project's costs without access to other projects.

   Exempt managed platform projects explicitly. Never infer exemption from a service name, missing payment details, or absent usage records. Defer payer transfers and delegated finance roles.

   Use one currency at launch. Define the period as UTC calendar-month anniversaries of activation. Clamp short months from the original anchor day.

   Schedule plan changes for the next period in version one. Keep currency, minimum, allowance, and rate version fixed within each period.

   Use this equation before taxes and explicit adjustments:

   ```text
   gross_usage = cpu_cost + memory_cost + volume_cost + egress_cost
   usage_after_allowance = max(0, gross_usage - included_usage)
   period_total = monthly_minimum + usage_after_allowance

   When monthly_minimum = included_usage:
   period_total = max(monthly_minimum, gross_usage)
   ```

   A $5 plan with $3 of usage costs $5. A $5 plan with $12 of usage costs $12. Apply the allowance once across the account.

   Apply trial credit to usage after the allowance, never to the paid minimum. Carry unused trial credit until its original expiry after an upgrade.

   Allocate credit under an account lock, earliest expiry first. Record each grant and consumption once. Refunds and corrections use explicit entries and provider credit notes.

   Use actual CPU time and sampled memory consumption. CPU and memory limits remain independent controls. Bill every allocation, plus startup, failed application runs, and deployment overlap.

   Exclude the builder, registry, ingress process, control plane, and managed console from customer compute meters. Apply build concurrency, duration, and storage limits separately.

   Measure build cost internally before a later product decision about build charges. This proposal does not assume that the operator can support unlimited free builds.

4. **Specify each meter as a versioned contract.**

   | Meter | Authoritative measurement | Stored quantity | Collection |
   | --- | --- | --- | --- |
   | CPU | Delta of allocation `cpu.stat:usage_usec` | CPU microseconds | Every 5 seconds and at lifecycle edges |
   | Memory | Integral of allocation `memory.current` | Byte-milliseconds | Every 5 seconds and at lifecycle edges |
   | Volume | Integral of used filesystem blocks | Byte-milliseconds | Every 60 seconds and at lifecycle edges |
   | Public egress | Allowed outbound logical IP bytes, classified at the allocation interface | Bytes | Kernel counter, read every 5 seconds |

   Linux documents `usage_usec` as cumulative CPU time. It documents `memory.current` as total cgroup memory, plus descendants. [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)

   Choose total cgroup memory for the first contract. It includes attributable page cache and kernel memory. Show this definition beside the memory graph.

   This is an explicit platform choice. Railway's public container documentation does not establish the exact equivalent kernel formula.

   Integrate gauges with the previous valid sample and monotonic elapsed time. Thus, memory and volume charges are sampled estimates. CPU and egress use cumulative counters.

   Define resource start and end events independently of readiness. Take the first sample at task start and the last available sample before cleanup.

   Give each task incarnation a counter epoch within its allocation. Cgroup recreation, interface recreation, and reboot create new epochs. Never subtract across a counter reset.

   Bound normal gauge intervals to two sample periods. Record a gap beyond that bound. Do not extend the last memory value across a long outage.

   For CPU, a cumulative counter that survives can recover a delta after a collector restart. Its distribution within the gap remains uncertain.

   Use monotonic time for duration and UTC for period assignment. Validate clock skew. Split normal intervals at UTC period and rate boundaries before they seal.

   If an uncertain interval crosses a financial boundary, flag it for settlement. Do not invent exact historical timestamps from a new observation.

   The collector emits usage only while customer resources exist. An assignment without a task consumes no compute. A stopped allocation consumes no CPU or memory.

   Sample volumes independently of allocations. Detached volumes still consume storage. Bill used filesystem blocks, with metadata, rather than the configured size or sparse image length.

   The current volume status rounds usage to 1 MiB. Add an exact-byte measurement method to `volumestore`. Treat an unreadable or unmounted filesystem as unknown, not zero.

   **Deletion policy:** stop customer storage charges at an accepted explicit volume deletion or ancestor deletion. Keep the current deletion grace as operator-funded retention.

   This deliberate policy prevents an offline agent or delayed destruction from increasing the customer's bill. Ordinary detachment and service suspension continue storage charges.

   Persist that cutoff outside resource rows. Volumes can outlive their project as destruction work, so invoice evidence must survive catalog garbage collection.

5. **Implement egress with one charge point.**

   Count traffic once at the host-side allocation interface, in the direction that carries packets from the allocation. The current TC ingress handler sees that direction.

   Add a pinned `BPF_MAP_TYPE_PERCPU_HASH` counter map. Use an allocation registration token plus counter epoch as the key. Keep the ifindex lookup separate.

   Use fixed-capacity maps with explicit admission failure. An LRU map can evict financial counters. Sum per-CPU values in the agent and checkpoint them without resets.

   Count after source validation and an allow decision. Exclude denied packets, private environment traffic, and explicit platform traffic classes.

   Classify direct public destinations and public ingress return traffic as billable. A proxy address inside a private CIDR does not make its customer responses free.

   Register trusted ingress source addresses as a distinct traffic class. Separate platform health probes by source identity or address. Do not accept customer-supplied traffic classes.

   This choice measures application output before Envoy's TLS, compression, or response changes. Describe it as logical IP bytes at the allocation interface.

   It does not reproduce exact ISP wire bytes. Include network headers and retransmissions as that contract specifies. Validate GSO behavior against the same definition.

   Envoy and WireGuard byte totals provide reconciliation evidence, not additional invoice quantities. No separate Envoy response meter charges the same bytes again.

   Verify IPv4, IPv6, same-agent private traffic, cross-agent private traffic, direct internet traffic, and Envoy responses. Include WebSockets, large transfers, retries, and interface reuse.

   Complete a topology proof on the production network before egress charges start. If attribution fails, keep this SKU in shadow mode while the other meters progress.

6. **Make delivery durable and safe to replay.**

   Use a separate bbolt usage journal on each fleet agent. Keep sample checkpoints and minute accumulation in the same transaction. Seal interval records every 60 seconds.

   Batch each agent's samples in one transaction per tick. Avoid a separate disk sync for every allocation.

   A normal record contains all applicable quantities for one resource and minute. Partial first and last minutes retain their true durations. Do not round runtime to a minute.

   ```protobuf
   // Proposed protocol shape, not generated code.
   message UsageInterval {
     string producer_id = 1;       // Durable usage-journal UUID.
     uint64 sequence = 2;          // Global order within this producer.
     string resource_id = 3;       // Allocation or volume.
     string counter_epoch = 4;
     google.protobuf.Timestamp start = 5;
     google.protobuf.Timestamp end = 6;
     uint64 cpu_microseconds = 7;
     string memory_byte_milliseconds = 8; // Arbitrary-precision integer.
     string volume_byte_milliseconds = 9;
     uint64 public_egress_bytes = 10;
     string meter_version = 11;
     string attribution_id = 12;   // Issued by the control plane.
   }
   ```

   Store boot ID, counter epoch, installation ID, and observation quality with the local evidence. Transport sessions do not define usage identity.

   Add `ReportUsage` and `UsageAcknowledgement` to `api/proto/agent.proto`. Keep financial acknowledgements independent of desired-state acknowledgements and ephemeral status.

   Authenticate the current transport session with current mTLS and session fences. Validate each record against retained control-plane resource attribution, with the assigned agent.

   Derive the payer and project from that attribution. Never trust payer IDs supplied by an application or producer. Permit authorized replay after catalog deletion.

   In one PostgreSQL transaction, validate the batch, insert new records, and advance the contiguous producer cursor. Acknowledge only after commit.

   Validate canonical integer quantities and physical bounds from each resource's limits. Reject negative durations, impossible counter deltas, and unbounded payloads.

   Key deduplication by `(installation_id, producer_id, sequence)`. Keep a payload hash and reject different payloads under the same key. Retry duplicates as successful no-ops.

   Validate non-overlap and one authorized producer per resource counter epoch. A replacement journal cannot submit a second charge for an interval from an older producer.

   Accept retransmission from the durable producer through a new authenticated session. Validate new resource records separately from historical replay authorization.

   Retain unacknowledged records locally. Keep acknowledged records for a bounded recovery window. The journal must never evict unacknowledged usage.

   Set an initial seven-day offline capacity target from measured encoded size. Alarm before exhaustion. Block new paid allocations when durable measurement fails.

   Before exhaustion, stop affected allocations through a controlled local path if the agent cannot preserve further evidence. Record any unavoidable gap and exclude unsupported charges.

   A total fleet-agent disk loss can destroy unreported evidence. Record that loss. Do not manufacture usage or advertise end-to-end exactly-once transport.

7. **Keep financial evidence in PostgreSQL.**

   | Proposed table or family | Purpose and invariants |
   | --- | --- |
   | `billing_accounts` | Owner, Stripe customer ID, currency, plan, budget, account condition |
   | `billing_periods` | Unique account/start, exact interval, minimum, allowance, rate version, close state |
   | `billing_resource_attributions` | Immutable payer/project/environment/resource/agent mapping, display names, charge cutoffs |
   | `usage_producers` | Durable producer identity, agent binding, accepted cursor, replay metadata |
   | `usage_intervals` | Accepted immutable evidence, source identity, quantities, meter version, payload hash |
   | `usage_rollups` | Rebuildable hourly quantities by account/project/environment/resource/meter |
   | `billing_rates` | Immutable rate version, currency, meter, unit definition, numerator and denominator |
   | `billing_credit_entries` | Trial grants, consumption, expiry, refunds, and explicit signed adjustments |
   | `billing_statements`, `billing_statement_lines` | Frozen period quantities, prices, allowance, rounding, credits, and provider references |
   | `durable_work_items`, `billing_provider_events` | Reuse the work queue for provider operations. Add a webhook inbox with unique Stripe event IDs. |

   Keep financial attribution independent of catalog foreign keys with cascade deletion. Restrict financial record deletion. A resource deletion must not erase its bill or payer.

   Use `NUMERIC(38,0)` for integrated quantities. Use `math/big` for wide integers and exact rational arithmetic. Avoid `float64` for quantities, money, and rates.

   Publish prices per vCPU-hour, GiB-hour, and GiB of transfer. If the UI also shows a month equivalent, define it as exactly 2,592,000 seconds.

   Railway uses GB labels. This platform proposal uses GiB explicitly. Do not copy rounded display prices as authoritative rates.

   Keep full precision through aggregation. Round the final currency subtotal once, with a documented half-up rule. Allocate cents deterministically across statement lines.

   A price change creates a new rate version for future periods. A meter definition change creates a new meter version. Neither rewrites closed statements.

   Partition raw usage by time. Start with hourly SQL rollups and indexed account/period reads. Keep financial producer receipts through the replay and archive retention window.

   Partition design must preserve deduplication across time. Use a producer cursor plus retained receipt hashes outside the raw partitions. An expired raw partition cannot permit replay charges.

   At 1,000 allocations, one record per minute produces 1.44 million records per day, before volumes. Benchmark this load before that scale.

   Start with PostgreSQL for authoritative evidence. The current ClickHouse path can receive a derived analytical copy later. It does not determine invoice totals.

   Choose hot evidence retention from close and dispute needs, initially 30 days. Archive sealed evidence with hashes before deletion. Keep statements, credits, and receipts longer.

   Product journal entries must cover payer assignment, charge cutoffs, and execution restrictions. Keep high-volume usage outside the journal's in-memory product projection.

   Include usage, financial records, receipts, and outbox state in PostgreSQL backup and restore. Extend explicit recovery inventories and validators where required.

   Reuse durable work claims for period close, invoice submission, reconciliation, and notices. Unique statement identities and row locks prevent two closers from creating distinct invoices.

   The queue can retry a completed task. Each financial operation must check its statement state before a provider call. Queue deduplication alone does not prevent another charge.

8. **Use Stripe for money collection.**

   Use Stripe-hosted Checkout in setup mode to save a payment method with off-session consent. Set the customer's invoice default from the confirmed setup. [Stripe future payments](https://docs.stripe.com/payments/checkout/save-and-reuse?payment-ui=stripe-hosted)

   The billing module owns the monthly calendar and allowance calculation. Use Stripe Invoicing to collect an internally rated statement. Avoid a second quantity authority in Stripe Meters.

   Stripe Meters remains a possible later adapter. It does not remove the need for durable local evidence, project attribution, budgets, or dispute explanations.

   On paid activation, create and collect the first minimum invoice. Activate the paid account only after confirmed payment. A saved card alone does not activate service.

   At renewal, close the previous period with a 24-hour lateness window. Create one invoice with previous-period usage overage and the next-period minimum.

   Show the dates on each line. A renewal cash charge covers two different resource periods. The period cost view still uses the earlier equation.

   If the account cancels, stop allocations at the effective cancellation time. Do not add a new minimum. Settle the final overage after the lateness window.

   Volumes require an explicit retention or deletion choice. Show continued storage charges before cancellation. Do not silently destroy data to make an invoice stop.

   Retained volumes use storage-only periods with no plan minimum or compute allowance. Keep the invoice cycle and payment method until storage charges end.

   Disclose this choice before cancellation. Define a small-balance policy for amounts below Stripe's collection minimum. Waive a terminal balance below that minimum.

   Period close can continue when producers are missing. Label gaps, alarm, and waive unsupported late charges after finalization in version one. Apply later customer credits explicitly.

   Retain late evidence with an explicit unbilled adjustment. It must not silently change a finalized statement or reappear as a future charge.

   Persist the frozen statement and outbox work atomically. A worker creates a Stripe draft with `auto_advance=false` and `collection_method=charge_automatically`.

   Attach every invoice item to that exact draft. Verify currency, customer, statement identity, line amounts, allowance, and tax configuration before finalization.

   Finalize explicitly, then enable automatic collection and configured retries. Handle authentication-required payments through Stripe's hosted invoice page. [Stripe invoice workflow](https://docs.stripe.com/invoicing/integration), [Automatic collection](https://docs.stripe.com/invoicing/automatic-collection)

   Use stable idempotency keys per statement operation and line. Persist every returned Stripe object ID. Stripe can prune v1 idempotency keys after 24 hours. [Stripe idempotency](https://docs.stripe.com/api/idempotent_requests)

   For an ambiguous operation beyond that window, reconcile the customer's invoices by statement metadata before retry. An unresolved match blocks collection and raises an alarm.

   Verify webhook signatures against the raw body. Persist each accepted event before a success response. Process it asynchronously through the billing module.

   Receive the public webhook through a console server route. Forward the raw bytes and Stripe signature over authenticated internal transport for Go validation and durable acceptance.

   Bound request size and forward unchanged bytes. Return success only after the control plane commits acceptance. Keep the Stripe secret and signing secret in operator-managed secrets.

   Stripe can deliver duplicate events and events out of order. Fetch current invoice state when necessary. Do not regress a paid invoice after an older failure event. [Stripe webhooks](https://docs.stripe.com/webhooks)

   Separate account conditions from invoice states. A paid invoice does not clear another unpaid invoice, a budget restriction, or a cancellation.

   Add a reconciliation job for local statements versus Stripe invoices and payments. Exercise missing webhooks, process crashes, refunds, and charge disputes.

   Let Stripe retain payment details and invoice documents. Configure tax collection for the launch market with finance review. Stripe integration alone does not establish tax obligations.

9. **Add cost controls to the execution path.**

   Expose current gross usage, included allowance, adjustments, estimated period cost, and forecast. Show the measurement watermark and any gap beside the estimate.

   Aggregate costs by project, environment, service, allocation, and volume. Include deleted resources. Explain costs through the same quantities and rates as the statement.

   Keep all-account billing pages separate from project settings. Add `console/src/features/billing/`, server operations, and account navigation through the current console structure.

   Add a project usage panel and a small service cost indicator. Provide a CSV statement export and Stripe invoice links. Refresh current estimates about once per minute.

   Extend `api/proto/platform.proto` with account summary, project usage, statement list, budget update, and payment-setup operations. Scope account operations to the payer.

   Use a forecast from recent hourly burn. Label the assumption and exclude it from credit consumption, account restrictions, and invoices.

   Support an email threshold and a resource stop threshold. Send notices at 75%, 90%, and 100%, with unique account/period/threshold delivery keys.

   Define the stop threshold against gross resource usage before the allowance, promotional credits, and tax. Display the minimum and retained-storage consequences separately.

   At the threshold, persist an account restriction. Block new builds, releases, restarts, scale increases, volume growth, and other new resource consumption.

   Let users inspect data, pay invoices, export records, and delete resources. Preserve drafts so payment recovery does not require reconstruction.

   Delivery reconciles the restriction into allocation stops and public route withdrawal. Enforce the check in autonomous build and replica reconciliation as well as user requests.

   Preserve requested replica counts. Once the restriction clears, delivery can restore desired state. Do not overload deletion or agent administration as financial suspension.

   Continue storage charges for retained volumes. The UI must state that the stop threshold is not an absolute invoice ceiling.

   Normal detection targets: usage visibility within 90 seconds and restriction application within the next reconciliation round. Measure these targets in shadow mode.

   A disconnected agent can continue resource use until it receives the restriction. There is no finite enforcement guarantee during an arbitrary partition.

   Keep that limitation explicit. If strict charge ceilings become a product requirement, add bounded account budget grants on agents and an operator-funded overshoot policy.

   For failed payment, notify immediately and block resource growth. Use a proposed seven-day grace before compute suspension. Recheck all unpaid debt before restoration.

   Start trials with small compute limits, replica limits, volume limits, build limits, a credit grant, and a local expiry deadline. Do not offer an uncapped anonymous trial.

   Trial expiry must work while disconnected. A credit balance alone cannot enforce an offline trial. Validate trial identity and abuse policy before public signup.

10. **Ship in reviewable increments.**

    | Increment | Concrete result | Exit condition |
    | --- | --- | --- |
    | 1. Contract and schema | Payer, units, periods, exact rates, authorization, charge cutoffs, fixture statements | Below-allowance, above-allowance, and cancellation examples agree |
    | 2. Compute and volume meters | Five-second samples, exact volume bytes, local journal, acknowledged minute records | Restarts, task exits, offline replay, and catalog deletion preserve correct totals |
    | 3. Egress meter | Classified eBPF counters with stable allocation attribution | Production topology and traffic matrix pass without double charges |
    | 4. Shadow cost view | SQL rollups, account/project views, meter quality, invoice previews | A full period and an accelerated close reconcile to independent measurements |
    | 5. Restrictions | Threshold notices, durable restriction, delivery enforcement, trial expiry, debt grace | User requests and autonomous reconciliation cannot restart restricted compute |
    | 6. Stripe sandbox | Setup, initial minimum, renewal, cancellation, webhooks, outbox, reconciliation | Duplicate delivery and crash recovery create one charge per statement |
    | 7. Paid pilot | Explicit accounts, frozen rates, enabled collection, operational recovery drill | Pilot statements explain every charge and restore cannot repeat a payment |

    Start increments 1 and 2 first. Increments 3 and 6 can progress independently after the contract and interfaces exist.

    Modes are `disabled`, `shadow`, and `enabled`, with explicit per-account activation. Preserve meter evidence across mode changes. Require a successful Stripe payment before paid activation.

    Use a kill switch that pauses invoice finalization and collection. Keep measurement and reconciliation active during an investigation.

    Since the platform is not deployed, change current schemas and protocols directly. Add no migration framework or hypothetical compatibility layer for this feature.

11. **Use focused verification and launch gates.**

    Test arithmetic through the billing module interface. Cover allowance allocation, month boundaries, exact rates, credit expiry, rounding, cancellation, and invoice correction.

    Use real PostgreSQL tests for duplicate batches, replay conflicts, cursor atomicity, concurrent close, and credit consumption. Verify financial records after catalog deletion.

    Use Linux tests for CPU counters, memory samples, task exits, counter resets, and volume growth. Verify each egress traffic class.

    Exercise the billing interface with a controlled clock and a Stripe sandbox. Test payment authentication, decline, duplicate webhooks, out-of-order webhooks, and ambiguous request outcomes.

    Add one end-to-end path from a paid account to deployment, measured usage, a stop threshold, payment, and restoration. Avoid tests that only mirror table helpers.

    Run the appropriate Go, console, PostgreSQL, and Linux VM checks for each increment. This plan changes no runtime code.

    Before paid launch, replay a complete archived period and reproduce its statement exactly. Restore a database backup and reconcile external payment state before collection resumes.

    Retain producer receipt coverage across restore. A restored database cannot assume that its old invoice state matches Stripe or that agents can replay all accepted usage.

    Default to paused collection after restore. Resume only after the operator verifies restored usage coverage and reconciles statement identities against Stripe.

    Alarm on stale producers, missing evidence, journal pressure, attribution rejection, settlement lag, provider mismatch, and restriction delay. Use the current operator alarm mechanism.

    Never ship a meter merely because it produces plausible graphs. Its launch gate is an explainable statement plus successful failure recovery.

12. **Set commercial prices after the shadow run.**

    Treat Railway's prices as a benchmark. Account for the operator's compute, disk, bandwidth, payment fees, build fleet, platform overhead, spare capacity, and support cost.

    Measure revenue per fleet agent at realistic utilization. The scheduler currently accounts for configured resource limits. Actual-use prices do not automatically produce efficient resource placement.

    Keep scheduler overcommit as a separate change. First establish cost per sellable CPU-hour, memory GiB-hour, retained GiB-hour, and public GiB.

    Decide the paid minimum, trial credit, margin target, and maximum credit exposure before launch. Use the proposed meter and invoice contracts for that decision.

    The first implementation milestone is a trustworthy shadow statement for one project's compute and volume usage. It establishes the evidence all later charges need.
