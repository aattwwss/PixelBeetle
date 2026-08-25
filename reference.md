Tuesday, August 25, 2026

 [**Hacker Times**](https://times.hntrends.net/)

[**Hacker Times**](https://times.hntrends.net/)

# Building a high-performance ticketing system with TigerBeetle

[renerocks.ai](https://renerocks.ai/blog/2025-11-02--tigerfans/)

[163](https://news.ycombinator.com/item?id=45852755 "163 points") [6](https://news.ycombinator.com/item?id=45852755 "6 comment threads")

**A journey from simple curiosity to 977 tickets per second**

![](https://wsrv.nl/?url=https://renerocks.ai/demo.gif)

TigerFans demo showing ticket checkout and payment flow

## What Started Everything

**” [Too easy: TigerBeetle.](https://x.com/jorandirkgreef/status/1959268005407252929)”**

That was Joran Dirk Greef’s response when someone on Twitter asked how you’d build a ticketing solution for an Oasis-scale concert—hundreds of thousands of people flooding your website at once, where you need to guarantee no ticket gets sold twice and everyone who pays gets a ticket. Joran is the CEO and founder of TigerBeetle.

He was right. Everyone who knows TigerBeetle would give the same advice. TigerBeetle is a financial transactions database designed for exactly this kind of problem: counting resources with absolute correctness under extreme load.

But I wanted to understand the concrete implementation. Not just conceptually—I needed to see the actual code.

How do you model ticket transactions as financial transactions? What does the account structure look like? How do the transfers flow through a realistic booking system with payment providers? What about pending reservations that timeout? What about idempotency when webhooks retry?

The best way to learn: build it myself.

So I built it. Three days later, I had a working demo. What started as an educational project became a 19-day optimization journey that pushed the system to 977 ticket reservations per second—15x faster than the Oasis baseline [1](https://times.hntrends.net/story/45852755#fn-1). All in Python.

## Building It: The HOW

The goal was clear: build a working demo that answers the HOW question. Not a toy example—a realistic booking flow with all the messy complexities of real payment systems.

The first challenge: TigerBeetle isn’t a general-purpose database. It’s a financial transactions database. That means it forces you to think in double-entry accounting primitives—accounts, transfers, debits, and credits.

So the question became: how do you model tickets as financial transactions?

Think about how banks handle money. They use double-entry bookkeeping—a system that’s been used for hundreds of years because it provides built-in error detection and perfect audit trails. Every transaction affects at least two accounts: one is debited, another is credited. Debits always equal credits, so the system is always balanced and errors are immediately obvious.

TigerBeetle demanded we do the same with tickets.

For each resource—Class A tickets, Class B tickets, those limited-edition t-shirts—we set up three TigerBeetle accounts: an Operator account that holds all available inventory, a Budget account that represents what’s available to sell, and a Spent account for consumed inventory.

First, we initialize by funding the Budget account from the Operator account:

![](https://wsrv.nl/?url=https://renerocks.ai/THE_WRITEUP_1.png)

When someone starts checkout, we create a pending transfer from Budget to Spent with a five-minute timeout. The crucial part: TigerBeetle’s `DEBITS_MUST_NOT_EXCEED_CREDITS` constraint flag. This makes overselling architecturally impossible—not prevented through careful programming, but impossible by design. The database enforces correctness.

![](https://wsrv.nl/?url=https://renerocks.ai/THE_WRITEUP_2.png)

When payment succeeds, we post the pending transfer to make it permanent:

![](https://wsrv.nl/?url=https://renerocks.ai/THE_WRITEUP_3.png)

When payment fails or times out, we void the transfer. It just vanishes. No cleanup jobs, no race conditions, perfect audit trail.

The demo stack was deliberately simple. FastAPI—the Python async web framework—because it’s quick to build and easy to understand. SQLite because that’s one less process to manage. TigerBeetle in dev mode. And MockPay—a simulated payment provider that mimicked the real webhook flow you’d get from Stripe or similar.

I wrote the code, built the UI, documented the account model. Everything worked. The two-phase checkout flow with webhooks, the pending reservations that auto-expire, the whole thing.

You can try the live demo yourself at [tigerfans.io](https://tigerfans.io/) — it’s still running, complete with the MockPay simulator.

Mission accomplished. I understood the HOW.

## Reaching Out to TigerBeetle

Before sharing the demo with the world, I wanted to show it to Joran and the TigerBeetle team first. The email I sent was careful:

> “Before sharing it with the world, (posting about it), I wanted to show it to you. I don’t want to spread ‘anti-patterns’.”

After all, I had made it all up. Maybe there is a better, more accounting-like, way of modeling tickets with TigerBeetle. I didn’t want my educational project become a bad example.

Joran was traveling, stuck dealing with flight delays somewhere, but he took the time to reply. The response was warm and encouraging. Rafael Batiati, one of TigerBeetle’s core developers, joined in with a note of caution: people would inevitably start benchmarking it once released. Oh. Right. Thinking about it, they probably would.

But then Joran turned it into a friendly challenge. He mentioned the Oasis ticket sale [1](https://times.hntrends.net/story/45852755#fn-1)—approximately 1.4 million tickets over six hours, or roughly 65 tickets per second. Then came the kicker: _“It would be pretty sweet if you could do better than six hours.”_

That number—65 tickets per second—became my new baseline.

## Performance Journey

What started as an educational demo shifted into something else entirely. The patterns were proven. The implementation was correct. But now a different question emerged: how many hidden inefficiencies could we eliminate?

Not the fundamental stuff—Python is Python, and there’s only so fast an interpreted language can go. But the bottlenecks we could actually fix. The architectural inefficiencies. How well could we really utilize TigerBeetle?

![](https://wsrv.nl/?url=https://renerocks.ai/progression.svg)

Performance progression for ticket reservations from 115 to 977 ops/s

* * *

### Initial Infrastructure Upgrades

To prepare for performance testing, I needed to eliminate database bottlenecks. SQLite’s blocking I/O would serialize request handling in FastAPI’s event loop, while PostgreSQL’s async drivers would allow true concurrent processing. So I switched from SQLite to PostgreSQL.

This meant upgrading the infrastructure—from a tiny 2-vCPU spot instance to a proper c7g.xlarge EC2 machine with 4 vCPUs and 8GB of RAM. PostgreSQL runs as its own process, so I wanted one vCPU for the OS, one for the HTTP workers, one for the database—with room to experiment with multiple workers later.

I also optimized the SQL queries, tuned transaction handling, and threw in uvloop for a faster event loop to have a good baseline for performance tests.

The result: 115 tickets per second. Better than the Oasis baseline.

I sent the numbers to Joran with a bit of playful humor: _“We already redefined TPS as tickets per second. Why not redefine big O notation as big Oasis notation? O(1) = 65 TPS. We’re currently at ~O(1.7).”_

Joran’s response was encouraging as always, but also included a reality check: _“I was surprised the TPS is so low. It should be at least approximately 10K.”_

Ten thousand. We were at 115.

TigerBeetle is famous for its >1000X performance. It’s built for this. So why was the whole system so sluggish?

* * *

### Understanding the Bottleneck

I sat down and drew out the complete sequence diagram. Every API request, every database roundtrip, every operation. The exercise was revealing:

**Checkout flow:**

1. Browser → Server (ticket class, email)
2. Server → TigerBeetle: Create PENDING transfers
3. Server → PostgreSQL: INSERT Order + PaymentSession
4. Server → Browser: Redirect to MockPay

**Webhook flow (payment confirmation):**

1. MockPay → Server: Payment succeeded
2. Server → PostgreSQL: Check idempotency
3. Server → TigerBeetle: POST pending transfers
4. Server → PostgreSQL: BEGIN TX, INSERT idempotency keys, UPDATE order, COMMIT TX

Every single API request was hitting PostgreSQL 2-4 times. PostgreSQL was in the critical path. Always.

To understand the bottleneck better, I split the measurements into two phases:

- **Phase 1**: Checkout/Reservations (create holds, save sessions)
- **Phase 2**: Payment Confirmations/Webhooks (commit/cancel, persist orders)

The results confirmed the problem:

- **Phase 1 (Reservations)**: ~150 ops/s
- **Phase 2 (Webhooks)**: ~130 ops/s

PostgreSQL was slow in BOTH phases. It was the bottleneck everywhere.

* * *

### The Redis Experiment

After seeing PostgreSQL as the bottleneck in both phases, I started questioning our architecture. Do we even need a relational database when we don’t really do anything relational in it? We’re basically just storing orders and idempotency keys.

I implemented Redis as a complete DATABASE\_URL replacement. The system now supported three swappable backends: SQLite, PostgreSQL, or Redis. It was the same interface, just different storage. I replaced ALL of PostgreSQL with Redis—not just sessions, but everything.

I ran the benchmarks with Redis in everysec fsync mode—balancing performance with some durability.

The results were impressive:

- **Phase 1 (Reservations)**: **930 ops/s** (6x improvement!)
- **Phase 2 (Webhooks)**: **450 ops/s** (3.4x improvement!)

The numbers were exciting. But there was a problem: Redis in everysec mode could lose up to 1 second of orders on crash. The faster Redis gets, the worse this becomes. I had replaced ALL of PostgreSQL with Redis, including the durable orders. That’s not acceptable, even for a demo.

I sent the impressive benchmark results (and the durability concern) to Rafael.

* * *

### Rafael’s Hot/Cold Path Compromise

Rafael’s response brought the architectural insight that would transform the system. He appreciated the detailed benchmarks but cautioned against gambling with durability even for a demo. His insight: separate ephemeral session data from durable order records.

This was the hot/cold path insight—a compromise between my speed experiment and proper durability. Instead of replacing ALL of PostgreSQL with Redis (which sacrificed durability), use Redis ONLY for payment sessions (hot path), NOT for orders (cold path).

Payment sessions are ephemeral. They only matter for a few minutes while the user is paying. Once payment succeeds or fails, we don’t need the session anymore. Same with idempotency keys—temporary deduplication data that prevents double-charging when webhooks retry.

But orders? Those need to be durable forever. We need PostgreSQL for that.

The insight was brilliant: not all data needs immediate durability!

What a great idea!

Any crash or abandoned cart would eventually get reverted by TigerBeetle’s timeout mechanism. In a real application, if a webhook callback is not found in Redis or already expired in TigerBeetle, you’d reverse the payment with the payment gateway.

The architecture clicked into place:

- **Hot path (reservations)**: TigerBeetle + Redis (in-memory, fast)
- **Cold path (confirmed payments)**: TigerBeetle + PostgreSQL (durable, slower)

* * *

### Correct Hot/Cold Implementation

I rebuilt the system with the correct separation: Redis for payment sessions, TigerBeetle for accounting, PostgreSQL for durable orders.

The hot path became truly hot—Redis and TigerBeetle handling every request. PostgreSQL only gets written to when payment actually succeeds. Failed checkouts never touch the database at all.

![](https://wsrv.nl/?url=https://renerocks.ai/THE_WRITEUP_4.png)

The impact was immediate. Throughput jumped to **865 tickets per second**. Moving PostgreSQL out of the critical path unlocked massive performance gains.

This was the working hot/cold architecture—fast where it needed to be fast, durable where it needed to be durable.

* * *

### Three Configuration Levels

With the hot/cold architecture working, Joran suggested something interesting: could we show “the before and after”? What if we used PostgreSQL for everything? What if we were smart and just added Redis? People would want to see what TigerBeetle actually makes possible.

He was right. The progression itself would tell the story—showing exactly where the performance gains came from, while also checking our assumptions about what was actually slow. As Mark Twain said: _“It isn’t what you don’t know that gets you; it’s what you know for sure that just ain’t so.”_ **Unchecked assumptions are silent killers.**

This required restructuring the code to support swappable backends, allowing fair comparisons between configurations.

I built three distinct configurations:

- **Level 1 (PG Only)**: PostgreSQL for everything—sessions, accounting, orders
- **Level 2 (PG + Redis)**: Redis for sessions, PostgreSQL for accounting and orders
- **Level 3 (TB + Redis)**: Redis for sessions, TigerBeetle for accounting, PostgreSQL for orders

The refactoring itself cleaned up some inefficiencies—Level 3 maintained its strong performance.

The results told a clear story:

- **Level 1**: 175 ops/s (reservations), 34 ops/s (webhooks)
- **Level 2**: 263 ops/s (1.5x better), 245 ops/s (7.3x better!)
- **Level 3**: 900 ops/s (5.1x vs L1), 313 ops/s (9.3x vs L1)

TigerBeetle speeds up everything.

* * *

### Comprehensive Testing Infrastructure

With the 3-level comparison structure in place, I built comprehensive testing infrastructure to understand exactly where time was being spent.

I had already split measurements into two phases earlier (during the bottleneck investigation):

- **Phase 1**: Checkout/Reservations (create holds, save sessions)
- **Phase 2**: Payment Confirmations/Webhooks (commit/cancel, persist orders)

Now I added isolated endpoints that measured just the core TigerBeetle operations: raw checkout (just the reservation accounting), raw webhook (just the commit accounting). Strip away everything else and see what TigerBeetle itself could do.

But I wanted more. The HTTP request timings were real-world relevant, but they didn’t show POTENTIAL. If, hypothetically, PostgreSQL took 4ms, Redis 2ms, TigerBeetle 1ms, and you had 2 operations per request, you’d see a 2x speedup when switching from PostgreSQL+Redis to TigerBeetle+Redis—when TigerBeetle was actually 4x faster. The Redis offset would hide the true speed in this hypothetical.

The solution: instrument transaction times inside the server. Query them after tests complete. Compare actual operation times, not just request-to-response latency.

Then I built TigerBench—an interactive visualization showing the complete progression across three configurations:

- **Level 1 (PG Only)**: PostgreSQL for everything—sessions, accounting, orders
- **Level 2 (PG + Redis)**: Redis for sessions, PostgreSQL for accounting and orders
- **Level 3 (TB + Redis)**: Redis for sessions, TigerBeetle for accounting, PostgreSQL for orders

The page shows both phases, both isolated operations, all the timings broken down by component. You can see exactly where the time goes in each configuration.

![](https://wsrv.nl/?url=https://renerocks.ai/tigerbench.png)

TigerBench visualization showing performance comparison across three configurations

You can explore it yourself: [tigerfans.io/bench](https://tigerfans.io/bench)

* * *

### Discovering the Batching Problem

Then I added more instrumentation to see what was actually happening inside TigerBeetle calls. The numbers told a strange story: we were sending TigerBeetle batches of size 1.

One transfer per request. Every single time.

TigerBeetle has an interface created for batching. Yet I was using it with many concurrent requests, each creating a batch size of 1.

But TigerBeetle can handle 8190 operations per request. That’s where its performance shines. Yet FastAPI’s request-oriented design means every `await` fires off immediately, creating an interface impedance mismatch. We’re flying a 747 to deliver individual passengers.

The solution was a custom batching layer—the LiveBatcher. It sits between the application and TigerBeetle, collecting concurrent requests as they arrive and packing them into efficient batches. While a batch is being processed, new requests queue up. When processing completes, queued requests are immediately packed and sent, continuously chaining to keep the pipeline full.

![](https://wsrv.nl/?url=https://renerocks.ai/THE_WRITEUP_5.png)

Result: batch sizes averaging 5-6 transfers. Throughput jumped to 977 reservations per second—15x faster than the Oasis baseline.

I added the batching metrics to TigerBench too—you can see the batch size distributions across different concurrency levels, watch how batching efficiency changes the performance curve.

![](https://wsrv.nl/?url=https://renerocks.ai/tb.batch_size_line.png)

Line chart showing TigerBeetle batch size distribution across concurrency levels

* * *

### When More Is Less

Then came the most counter-intuitive discovery of the entire project.

For this test, I upgraded to c7g.2xlarge (8 vCPU, 16 GB RAM).

I tried running with multiple workers. Standard practice, right? Eight CPUs, so run multiple workers. Utilize all the cores.

The hypothesis: more workers = more throughput?

**Result**: **NO**.

Testing 1,000 reservations:

- **1 worker**: 977 ops/s, average batch size 5.3
- **2 workers**: 966 ops/s (1% slower), average batch size 3.9
- **3 workers**: 770 ops/s (21% slower!), average batch size 2.9

The measurements were clear, but they made no sense. Until I understood what was happening.

Multiple workers fragment batches across event loops. The load balancer sends request 1 to worker 1, request 2 to worker 2, request 3 to worker 3. Each worker’s batcher only sees a fraction of the concurrent load. Smaller batches. And since TigerBeetle processes those batches sequentially anyway, the fragmentation doesn’t buy us parallelism—just overhead.

When batching efficiency is critical, consolidation beats parallelism. It’s Amdahl’s Law in action.

![](https://wsrv.nl/?url=https://renerocks.ai/workers.svg)

Single worker vs multi-worker batch fragmentation

* * *

### Final Performance Results

After weeks of iteration and measurement, the system hit 977 ticket reservations per second. Fifteen times faster than the Oasis baseline. All in Python.

Median latency: 11 milliseconds. Even at the 99th percentile, requests completed in 26 milliseconds. The batch sizes averaged 5-6 transfers—not close to TigerBeetle’s theoretical optimum, but good enough to unlock real performance.

The limiting factor was clear: Python’s event loop overhead, about 5 milliseconds per request. That’s 45% of the total time. Even with infinitely fast TigerBeetle, you can’t get much faster without ditching Python.

But that was the point. If it works this well with Python’s overhead, the architecture is sound.

* * *

## TigerBeetle Ticket Challenge

The recipe is proven. This implementation—in Python, with all its overhead—achieves 977 reservations per second. The architecture is documented, the patterns are explained, the lessons are captured.

Imagine this same architecture in **Go**, where removing Python’s 5ms overhead could yield **10-30x better throughput**. Or in **Zig**, where manual optimization might push it to **50-100x faster**.

**The TTC challenge is simple**: Build your version, any language, any stack, and share your results. Let’s see how fast ticketing can be when TigerBeetle’s batch-oriented design meets systems programming languages.

**Resources available**:

- Live demo: [tigerfans.io](https://tigerfans.io/)
- TigerBench visualization: [tigerfans.io/bench](https://tigerfans.io/bench)
- Source code: [github.com/renerocksai/tigerfans](https://github.com/renerocksai/tigerfans)
- Technical deep-dives on all four patterns _(see menu at the end)_
- Reproducible benchmarks

## Gratitude

Thank you to **Joran Dirk Greef** for creating TigerBeetle, for the “benchmark would be nice” challenge that started the optimization journey, and for being so encouraging throughout.

Thank you to **Rafael Batiati** for the crucial hot/cold path compromise, the perfect balance of speed and durability, the deep code reviews, and diving into TigerBeetle’s Python batching behavior to help unlock the final performance tier.

Thank you to the entire **TigerBeetle team** for building an amazing database and being so generous with their knowledge.

As the final commit message said:

**“I had the time of my life working on this 😊!”**

> **Sometimes the best projects aren’t the ones you plan. They’re the ones that grab you, challenge you, teach you, and refuse to let go until you’ve explored every corner, answered every question, and squeezed every last drop of insight from the problem.**

* * *

## Related Documents

**Overview**: [Executive Summary](https://renerocks.ai/projects/tigerfans/)

**Technical Details**:

- [Resource Modeling with Double-Entry Accounting](https://renerocks.ai/projects/tigerfans/DEEPDIVE_RESOURCE_MODELING/)
- [Hot/Cold Path Architecture](https://renerocks.ai/projects/tigerfans/DEEPDIVE_HOT_COLD_PATH/)
- [Auto-Batching](https://renerocks.ai/projects/tigerfans/DEEPDIVE_AUTO_BATCHING/)
- [The Single-Worker Paradox](https://renerocks.ai/projects/tigerfans/DEEPDIVE_SINGLE_WORKER_PARADOX/)
- [Amdahl’s Law Analysis](https://renerocks.ai/projects/tigerfans/ANALYSIS_AMDAHL_AND_PLANES/)

**Resources**:

- [Live Demo](https://tigerfans.io/)
- [TigerBench Visualization](https://tigerfans.io/bench)
- [Source Code](https://github.com/renerocksai/tigerfans)

## Discussion

[ripberge](https://news.ycombinator.com/user?id=ripberge)• [10 months ago](https://news.ycombinator.com/item?id=45881130)

Having built a ticketing system that sold some Oasis level concerts there's a few misconceptions here:

Selling an event out takes a long time to do frequently because tickets are VERY frequently not purchased--they're just reserved and then they fall back into open seating. This is done by true fans, but also frequently by bots run by professional brokers or amateur resellers. And Cloudflare and every other state of the art bot detection platform doesn't detect them. Hell, some of the bots are built on Cloudflare workers themselves in my experience...

So whatever velocity you achieve in the lab--in the real world you'll do a fraction of it when it comes to actual purchases. That depends upon the event really. Events that fly under the radar may get you a higher actual conversion rate.

Also, an act like Oasis is going to have a lot of reserved seating. Running through algorithms to find contiguous seats is going to be tougher than this example and it's difficult to parallelize if you're truly giving the next person in the queue the actual best seats remaining.

There are many other business rules that accrue after years of features to win Oasis like business unfortunately that will result in more DB calls and add contention.

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45886291)

\> Selling an event out takes a long time to do frequently because tickets are VERY frequently not purchased--they're just reserved and then they fall back into open seating.

TigerBeetle actually includes native support for "two phase pending transfers" out of the box, to make it easy to coordinate with third party payment systems while users have inventory in their cart:

[https://docs.tigerbeetle.com/coding/two-phase-transfers/](https://docs.tigerbeetle.com/coding/two-phase-transfers/)

\> Also, an act like Oasis is going to have a lot of reserved seating. Running through algorithms to find contiguous seats is going to be tougher than this example and it's difficult to parallelize if you're truly giving the next person in the queue the actual best seats remaining.

It's actually not that hard (and probably easier) to express this in TigerBeetle using transfers with deterministic IDs. For example, you could check (and reserve) up to 8K contiguous seats in a single query to TigerBeetle, with a P100 less than 100ms.

\> There are many other business rules that accrue after years of features to win Oasis like business unfortunately that will result in more DB calls and add contention.

Yes, contention is the killer.

We added an Amdahl's Law calculator to TigerBeetle's homepage to let you see the impact: [https://tigerbeetle.com/#general-purpose-databases-have-an-o...](https://tigerbeetle.com/#general-purpose-databases-have-an-oltp-limit)

As you move "the data to the code" in interactive transactions with multiple queries, to process more and more business rules, you're holding row locks across the network. TigerBeetle's design inverts this, to move "the code to the data" in declarative queries, to let the DBMS enforce the transactional business rules directly in the database, with a rich set of debit/credit primitives and audit trail.

[andersmurphy](https://news.ycombinator.com/user?id=andersmurphy)• [10 months ago](https://news.ycombinator.com/item?id=45892643)

It's almost like stored procedures were a good idea.

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45896350)

If only. But you also need to fix the internal concurrency control of the DBMS storage engine. TB here is very different to PG.

For example, if you have 8K transactions through 2 accounts, a naive system might read the 2 accounts, update their balances, then write the 2 accounts… for all 8K (!) transactions.

Whereas TB does vectorized concurrency control: read the 2 accounts, update them 8K times, write the 2 accounts.

This is why stored procedures only get you typically about a 10x win, you don’t see the same 1000x as with TB, especially at power law contention.

[andersmurphy](https://news.ycombinator.com/user?id=andersmurphy)• [10 months ago](https://news.ycombinator.com/item?id=45898253)

Huge fan of what tiger beatle promotes. Even in simple system/projects batching and reducing contention can be massive win. Batching + single application writer alone in something like sqlite can get you to pretty ridiculous inserts/updates per second (although transactions become at the batch level).

I sometimes wonder how many fewer servers we would need if the aproaches promoted by Tiger Style were more widespread.

What datasteucture does Tiger Beatle use for it's client? I'm assuming its multi writer single reader. I've always wondered what the best choice is there. A reverse LMAX disruptor (multiple producers single consumer).

[alamsterdam](https://news.ycombinator.com/user?id=alamsterdam)• [10 months ago](https://news.ycombinator.com/item?id=45885780)

Agree with the above, we built and run a ticketing platform, the actual transaction of purchasing the ticket at the final step in the funnel is not the bottleneck.

The shopping process and queuing process puts considerably more load on our systems than the final purchase transaction, which ultimately is constrained by the size of the venue, which we can control by managing the queue throughput.

Even with a queue system in place, you inevitably end up with the thundering heard problem when ticket sales open, as a large majority of users will refresh their browsers regardless of instructions to the contrary

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45897921)

You would use TigerBeetle for everything: not only the final purchase transaction, but the shopping cart process, inventory management and queuing/reserving.

In other words, to count not only the money changing hands, but also the corresponding goods/services being exchanged.

These are all transactions: goods/services and the corresponding money.

[0cf8612b2e1e](https://news.ycombinator.com/user?id=0cf8612b2e1e)• [10 months ago](https://news.ycombinator.com/item?id=45881228)

Does that mean that there is some smoke and mirrors when, eg Taylor Swift, says they sold out the concert in minutes? Or are the mega acts truly that high demand?

[ripberge](https://news.ycombinator.com/user?id=ripberge)• [10 months ago](https://news.ycombinator.com/item?id=45881623)

You can get the seats into "baskets" (reserved) in minutes. In my experience they will not sell out for some time as they usually keep dropping back into inventory. "Sold Out" is a matter of opinion. There are usually lots of single seats left sometimes for weeks or months. The promoter decides when to label the event as "sold out".

[kelseydh](https://news.ycombinator.com/user?id=kelseydh)• [10 months ago](https://news.ycombinator.com/item?id=45883391)

I recently did performance testing of Tigerbeetle for a financial transactions company. The key thing to understand about Tigerbeetle's speed is that it achieves very high speeds through batching transactions.

\-\-\--

In our testing:

For batch transactions, Tigerbeetle delivered truly impressive speeds: ~250,000 writes/sec.

For processing transactions one-by-one individually, we found a large slowdown: ~105 writes/sec.

This is much slower than PostgreSQL, which row updates at ~5495 sec. (However, in practice PostgreSQL row updates will be way lower in real world OLTP workloads due to hot fee accounts and aggregate accounts for sub-accounts.)

One way to keep those faster speeds in Tigerbeetle for real-time workloads is microbatching incoming real-time transactions to Tigerbeetle at an interval of every second or lower, to take advantage of Tigerbeetle's blazing fast batch processing speeds. Nonetheless, this remains an important caveat to understand about its speed.

[rbatiati](https://news.ycombinator.com/user?id=rbatiati)• [10 months ago](https://news.ycombinator.com/item?id=45887037)

Hi! Rafael from TigerBeetle here!

\> One way to keep those faster speeds in Tigerbeetle for real-time workloads is microbatching incoming real-time transactions to Tigerbeetle at an interval of every second or lower, to take advantage of Tigerbeetle's blazing fast batch processing speeds.

We don’t recommend artificially holding transfers just for batching purposes.
René actually had to implement a batching worker API to work around a limitation in Python’s FastAPI, which handled requests per process, and he’s been very clear in suggesting that such would be better reimplemented in Go.

Unlike most connection-oriented database clients, the TigerBeetle client doesn’t use a connection pool, because there’s no concept of a “connection” in TigerBeetle’s VSR protocol.

This means that, although you can create multiple client instances, in practice less is better. You should have a single long-lived client instance per process, shared across tasks, coroutines, or threads (think of a web server handling many concurrent requests).

In such a scenario, the client can efficiently pack multiple events into the same request, while your application logic focuses solely on business-event-oriented chains of transfers. Typically, each business event involves only a handful of transfers, which isn't a problem of underutilization, as they'll be submitted together with other concurrent events as soon as possible.

However, if you’re dealing with a non-concurrent workload, for example, a batch process that bills thousands of customers for their monthly invoices, then you can simply submit all transfers at once.

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45886467)

Joran from TigerBeetle!

\> For processing transactions one-by-one individually

If you're artificially restricting the load going into TigerBeetle, by sending transactions in one-by-one individually, then I think predictable latency (and not TPS) would be a better metric.

For example, TB's multi-region/multi-AZ fault-tolerance will work around gray failure (fail slow of hardware, as opposed to fail stop) in your network links or SSDs. You're also getting significantly stronger durability guarantees with TB \[0\]\[1\].

It sounds like you were benchmarking on EBS? We recommend NVMe. We have customers running extremely tight 1 second SLAs, seeing microsecond latencies, even for one at a time workloads. Before TB, they were bottlenecking on PG. After TB, they saturated their central bank limit.

I would also be curious to what scale you tested? We test TB to literally 100 billion transactions. It's going to be incredibly hard to replicate that with PG's storage engine. PG is a great string DBMS but it's simply not optimized for integers the way TB is. Granted, your scale likely won't require it, but if you're comparing TPS then you should at least compare sustained scale.

There's also the safety factor of trying to reimplement TB's debit/credit primitives over PG to consider. Rolling it yourself. For example, did you change PG's defaults away from Read-Committed to Serializable and enable checksums in your benchmarks? (PG's checksums, even if you enable them, are still not going to protect you from misdirected I/O like the recent XFS bug.) Even the business logic is deceptively hard, there are thousands of lines of complicated state machine code, and we've invested literally millions into testing and audits.

Finally, it's important that your architecture as a whole, the gateways around TB, designs for concurrency first class, and isn't "one at a time", or TigerBeetle is probably not going to be your bottleneck.

\[0\] [https://www.youtube.com/watch?v=\_jfOk4L7CiY](https://www.youtube.com/watch?v=_jfOk4L7CiY)

\[1\] [https://jepsen.io/analyses/tigerbeetle-0.16.11](https://jepsen.io/analyses/tigerbeetle-0.16.11)

[NathanaelRea](https://news.ycombinator.com/user?id=NathanaelRea)• [10 months ago](https://news.ycombinator.com/item?id=45883448)

Doesn't the Tigerbeetle client automatically batch requests?

[kelseydh](https://news.ycombinator.com/user?id=kelseydh)• [10 months ago](https://news.ycombinator.com/item?id=45883822)

We didn't observe any automatic batching when testing Tigerbeetle with their Go client. I think we initiated a new Go client for every new transaction when benchmarking, which is typically how one uses such a client in app code. This follows with our other complaint: it handles so little you will have to roll a lot of custom logic around it to batch realtime transactions quickly.

[matdehaast](https://news.ycombinator.com/user?id=matdehaast)• [10 months ago](https://news.ycombinator.com/item?id=45885155)

I'm a bit worried you think instantiating a new client for every request is common practice. If you did that to Postgres or MySQL clients, you would also have degradation in performance.

PHP has created mysqli or PDO to deal with this specifically because of the known issues of it being expensive to recreate client connects per request

[kelseydh](https://news.ycombinator.com/user?id=kelseydh)• [10 months ago](https://news.ycombinator.com/item?id=45886104)

Ok your comment made me double check our benchmarking script in Go. Can confirm we didn't instantiate a new client with each request.

For transparency here's the full Golang benchmarking code and our results if you want to replicate it: [https://gist.github.com/KelseyDH/c5cec31519f4420e195114dc9c8...](https://gist.github.com/KelseyDH/c5cec31519f4420e195114dc9c8eb22f)

We shared the code with the Tigerbeetle team (who were very nice and responsive btw), and they didn't raise any issues with the script we wrote of their Tigerbeetle client. They did have many comments about the real-world performance of PostgreSQL in comparison, which is fair.

[matdehaast](https://news.ycombinator.com/user?id=matdehaast)• [10 months ago](https://news.ycombinator.com/item?id=45896559)

Thanks for the code and clarification. I'm surprised the TB team didn't pick it up, but your individual transfer test is a pretty poor representation. All you are testing there is how many batches you can complete per second, giving no time for the actual client to batch the transfers. This is because when you call createTransfer in GO, that will synchronously block.

For example, it is as if you created an HTTP server that only allows one concurrent request. Or having a queue where only 1 worker will ever do work. Is that your workload? Because I'm not sure I know of many workloads that are completely sync with only 1 worker.

To get a better representation for individual\_transfers, I would use a waitgroup

```
  var wg sync.WaitGroup
  var mu sync.Mutex
  completedCount := 0

  for i := 0; i < len(transfers); i++ {
    wg.Add(1)
    go func(index int, transfer Transfer) {
     defer wg.Done()

     res, _ := client.CreateTransfers([]Transfer{transfer})
     for _, err := range res {
      if err.Result != 0 {
       log.Printf("Error creating transfer %d: %s", err.Index, err.Result)
      }
     }

     mu.Lock()
     completedCount++
     if completedCount%100 == 0 {
      fmt.Printf("%d\n", completedCount)
     }
     mu.Unlock()
    }(i, transfers[i])
   }

  wg.Wait()
  fmt.Printf("All %d transfers completed\n", len(transfers))
```

This will actually allow the client to batch the request internally and be more representative of the workloads you would get. Note, the above is not the same as doing the batching manually yourself. You could call createTransfer concurrently the client in multiple call sites. That would still auto batch them

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45897265)

Appreciate your kind words, Kelsey!

I searched the recent history of our community Slack but it seems it may have been an older conversation.

We typically do code review work only for our customers so I’m not sure if there was some misunderstanding.

Perhaps the assumption that because we didn’t say anything when you pasted the code, therefore we must have reviewed the code?

Per my other comment, your benchmarking environment is also a factor. For example, were you running on EBS?

These are all things that our team would typically work with you on to accelerate you, so that you get it right the first time!

[kelseydh](https://news.ycombinator.com/user?id=kelseydh)• [10 months ago](https://news.ycombinator.com/item?id=45907708)

Yeah it was back in February in your community Slack, I did receive a fairly thorough response from you and others about it. However then there were no technical critiques of the Go benchmarking code, just how our PostgreSQL comparison would fall short in real OLTP workloads (which is fair).

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45911090)

Yes, thanks!

I don’t think we reviewed your Go benchmarking code at the time—and that there were no technical critiques probably should not have been taken as explicit sign off.

IIRC we were more concerned at the deeper conceptual misunderstanding, that one could “roll your own” TB over PG with safety/performance parity, and that this would somehow be better than just using open source TB, hence the discussion focused on that.

[NathanaelRea](https://news.ycombinator.com/user?id=NathanaelRea)• [10 months ago](https://news.ycombinator.com/item?id=45883865)

Interesting, I thought I had heard that this is automatically done, but I guess it's only through concurrent tasks/threads. It is still necessary to batch in application code.

[https://docs.tigerbeetle.com/coding/clients/go/#batching](https://docs.tigerbeetle.com/coding/clients/go/#batching)

But nonetheless, it seems weird to test it with singular queries, because Tigerbeetle's whole point is shoving 8,189 items into the DB as fast as possible. So if you populate that buffer with only one item your're throwing away all that space and efficiency.

[kelseydh](https://news.ycombinator.com/user?id=kelseydh)• [10 months ago](https://news.ycombinator.com/item?id=45884359)

We certainly are losing that efficiency, but this is typically how real-time transactions work. You write real-time endpoints to send off transactions as they come in. Needing to roll more than that is a major introduction of complexity.

We concluded where Tigerbeetle really shines is if you're a large entity like a central bank or corporation sending massive transaction files between entities. Tigerbeetle is amazing for moving large numbers of batch transactions at once.

We found other quirks with Tigerbeetle that made it difficult as a drop-in replacement for handling transactions in PostgreSQL. E.g. Tigerbeetle's primary ID key isn't UUIDv7 or ULID, it's a custom id they engineered for performance. The max metadata you can save on a transaction is a 128-bit unsigned integer on the user\_data\_128 field. While this lets them achieve lightning fast batch transaction processing benchmarks, the database allows for the saving of so little metadata you risk getting bottlenecked by all the attributes you'll need to wrap around the transaction in PostgreSQL to make it work in a real application.

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45886587)

\> you risk getting bottlenecked by all the attributes you'll need to wrap around the transaction in PostgreSQL to make it work in a real application.

The performance killer is contention, not writing any associated KV data—KV stores scale well!

But you do need to preserve a clean separation of concerns in your architecture. Strings in your general-purpose DBMS as "system of reference" (control plane). Integers in your transaction processing DBMS as "system of record" (data plane).

Dominik Tornow wrote a great blog post on how to get this right (and let us know if our team can accelerate you on this!):

[https://tigerbeetle.com/blog/2025-11-06-the-write-last-read-...](https://tigerbeetle.com/blog/2025-11-06-the-write-last-read-first-rule/)

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45886555)

\> We didn't observe any automatic batching when testing Tigerbeetle with their Go client.

This is not accurate. All TigerBeetle's clients also auto batch under the hood, which you can verify from the docs \[0\] and the source \[1\], provided your application has at least some concurrency.

\> I think we initiated a new Go client for every new transaction when benchmarking

The docs are careful to warn that you shouldn't be throwing away your client like this after each request:

```
  The TigerBeetle client should be shared across threads (or tasks, depending on your paradigm), since it automatically groups together batches of small sizes into one request. Since TigerBeetle clients can have at most one in-flight request, the client accumulates smaller batches together while waiting for a reply to the last request.
```

Again, I would double check that your architecture is not accidentally serializing everything. You should be running multiple gateways and they should each be able to handle concurrent user requests. The gold standard to aim for here is a stateless layer of API servers around TigerBeetle, and then you should be able to push pretty good load.

\[0\] [https://docs.tigerbeetle.com/coding/requests/#automatic-batc...](https://docs.tigerbeetle.com/coding/requests/#automatic-batching)

\[1\] The core batching logic powering all language clients: [https://github.com/tigerbeetle/tigerbeetle/blob/main/src/cli...](https://github.com/tigerbeetle/tigerbeetle/blob/main/src/clients/c/tb_client/context.zig#L517-L547)

[kelseydh](https://news.ycombinator.com/user?id=kelseydh)• [10 months ago](https://news.ycombinator.com/item?id=45890141)

Thanks for reaching out. I shared this benchmarking script with your team when we tested Tigerbeetle, but this is it again: [https://gist.github.com/KelseyDH/c5cec31519f4420e195114dc9c8...](https://gist.github.com/KelseyDH/c5cec31519f4420e195114dc9c8eb22f)

Was there something wrong with our test of the individual transactions in our Go script that caused the drop in transaction performance we observed?

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45890905)

Thanks Kelsey!

We’d love to roll up our sleeves and help you get it right. Please drop me an email.

[lossolo](https://news.ycombinator.com/user?id=lossolo)• [10 months ago](https://news.ycombinator.com/item?id=45893530)

So what was wrong with his isolated benchmark code that he shared here?

[matdehaast](https://news.ycombinator.com/user?id=matdehaast)• [10 months ago](https://news.ycombinator.com/item?id=45896589)

Not from Tigerbeetle, but having looked at his code this is what I saw [https://news.ycombinator.com/item?id=45896559](https://news.ycombinator.com/item?id=45896559)

[nickmonad](https://news.ycombinator.com/user?id=nickmonad)• [10 months ago](https://news.ycombinator.com/item?id=45883444)

Did the company end up using it?

[kelseydh](https://news.ycombinator.com/user?id=kelseydh)• [10 months ago](https://news.ycombinator.com/item?id=45883514)

We didn't rule out using Tigerbeetle, but the drop in non-batch performance was disappointing and a reason we haven't prioritised switching our transaction ledger from PostgreSQL to Tigerbeetle.

There was also poor Ruby support for Tigerbeetle at the time, but that has improved recently and there is now a (3rd party) Ruby client: [https://github.com/antstorm/tigerbeetle-ruby/](https://github.com/antstorm/tigerbeetle-ruby/)

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45886704)

I think the drop in non-batch performance was more a function of the PoC than of TB. Would love to see what our team could do for you here! Feel free to reach out to peter@tigerbeetle.com

[nickmonad](https://news.ycombinator.com/user?id=nickmonad)• [10 months ago](https://news.ycombinator.com/item?id=45881364)

It seems to me that, in practice, you'd want the "LiveBatcher" to have some durability as well. Is there a scenario where a customer could lose their place because of a horribly timed server shutdown, where those transfers hadn't even been sent to TigerBeetle as pending yet? Or am I misunderstanding the architecture here?

Edit: Yes, I think I misunderstood something here. The user wouldn't even see their request as having returned a valid "pending" ticket sale since the batcher would be active as the request is active. The request won't return until its own transfer had been sent off to TigerBeetle as pending.

[overfeed](https://news.ycombinator.com/user?id=overfeed)• [10 months ago](https://news.ycombinator.com/item?id=45884270)

Obligatory Jepsen report [https://jepsen.io/analyses/tigerbeetle-0.16.11](https://jepsen.io/analyses/tigerbeetle-0.16.11)

[vivzkestrel](https://news.ycombinator.com/user?id=vivzkestrel)• [10 months ago](https://news.ycombinator.com/item?id=45883814)

Why cant this be done with PostgreSQL?

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45886322)

The short answer is that we tried, back in 2020, while working on a central bank payment switch by the Gates Foundation. We found we were hitting the limits of Amdahl's Law, given Postgres' concurrency control with row locks held across the network as well as internal I/O, leading to the design of TigerBeetle. To specialize not for general purpose but only for transaction processing.

On the one hand, yes, you could use a general purpose string database to count/move integers, up to a certain scale. But a specialized integer database like TigerBeetle can take you further. It's the same reason, that yes, you could use Postgres as object storage or as queue, or you could use S3 and Kafka and get separation of concerns in your architecture.

I did a talk diving into all this recently, looking at the power law, OLTP contention, and how this interacts with Amdahl's Law and Postgres and TigerBeetle: [https://www.youtube.com/watch?v=yKgfk8lTQuE](https://www.youtube.com/watch?v=yKgfk8lTQuE)

[vivzkestrel](https://news.ycombinator.com/user?id=vivzkestrel)• [10 months ago](https://news.ycombinator.com/item?id=45886550)

i am not an exact expert on the limitation you claim to have encountered on postgresql but perhaps someone with more postgresql expertise can chime in on this comment and give some insight

[ants\_a](https://news.ycombinator.com/user?id=ants_a)• [10 months ago](https://news.ycombinator.com/item?id=45898964)

For updating a single resource where the order of updates matters the best throughput one can hope for is the inverse of locking duration. Typical postgres using applications follow the pattern where a transaction involves multiple round trips between the application and the database to make decisions in the code running on the application server.

But this pattern is not required by PostgreSQL, it's possible to run arbitrarily complex transactions all on server side using more complex query patterns and/or stored procedures. In this case the locking time will be mainly determined by time-to-durability. Which, depending on infrastructure specifics, might be one or two orders of magnitude faster. Or in case of fast networks and slow disks, it might not have a huge effect.

One can also use batching in PostgreSQL to update the resource multiple times for each durability cycle. This will require some extra care from application writer to avoid getting totally bogged down by deadlocks/serializability conflicts.

What will absolutely kill you on PostgreSQL is high contention and repeatable read and higher isolation levels. PostgreSQL handles update conflicts with optimistic concurrency control, and high contention totally invalidates all of that optimism. So you need to be clever enough to achieve necessary correctness guarantees with read committed and the funky semantics it has for update visibility. Or use some external locking to get rid of contention in the database. The option for pessimistic locking would be very helpful for these workloads.

What would also help is a different kind of optimism, that would remove durability requirement from lock hold time, which would then result in readers having to wait for durability. Postgres can do tens of thousands of contended updates per second with this model. See the Eventual Durability paper for details.

[jorangreef](https://news.ycombinator.com/user?id=jorangreef)• [10 months ago](https://news.ycombinator.com/item?id=45886608)

You don't have to be an expert to understand Amdahl's Law. Definitely watch the talk if you haven't already: [https://www.youtube.com/watch?v=yKgfk8lTQuE](https://www.youtube.com/watch?v=yKgfk8lTQuE)

[kevinak](https://news.ycombinator.com/user?id=kevinak)• [10 months ago](https://news.ycombinator.com/item?id=45881319)

Is FastAPI just bad with SQLite? I would have expected SQLite to smoke Postgres in terms of ops/s.

[koakuma-chan](https://news.ycombinator.com/user?id=koakuma-chan)• [10 months ago](https://news.ycombinator.com/item?id=45881784)

I think Python is bad in general if you want “high-performance”

[makapuf](https://news.ycombinator.com/user?id=makapuf)• [10 months ago](https://news.ycombinator.com/item?id=45881476)

SQLite is in process, but concurrent write / performance is a complex matter : [https://sqlite.org/wal.html](https://sqlite.org/wal.html)

[kevinak](https://news.ycombinator.com/user?id=kevinak)• [10 months ago](https://news.ycombinator.com/item?id=45881540)

Yes, that's why I would expect it to smoke Postgres here, in process is orders of magnitude faster. Do you really need concurrency here when you can do 10-100k+ inserts per second?

[ies7](https://news.ycombinator.com/user?id=ies7)• [10 months ago](https://news.ycombinator.com/item?id=45882379)

If 100k users each hit purchase button at the same time will sqlite write it in 1 second?

This is different than 1 user doing the purchase for 100k fans

[kevinak](https://news.ycombinator.com/user?id=kevinak)• [10 months ago](https://news.ycombinator.com/item?id=45887267)

I mean it depends on the query and what you're doing of course, but it's not impossible to reach writes/s in the 80k range.

[0cf8612b2e1e](https://news.ycombinator.com/user?id=0cf8612b2e1e)• [10 months ago](https://news.ycombinator.com/item?id=45882487)

Also surprised. My yardstick was this post which showed SQLite beating Postgres in a Django app. Benchmarking is hard, and the author said the Postgres results were not tuned to the same degree as SQLite, so buyer beware.
[https://blog.pecar.me/django-sqlite-benchmark](https://blog.pecar.me/django-sqlite-benchmark)

Building a high-performance ticketing system with TigerBeetle – Hacker Times
