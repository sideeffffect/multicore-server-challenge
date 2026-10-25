# GC showcase: when do the concurrent collectors win?

A side experiment on this branch (`gc-showcase`), not part of the challenge submission: the submission folder is
unchanged, and this only adds a way to load it differently.

In the challenge as specified, **Parallel GC wins** for this server (see the submission's
[FINDINGS.md](../../submissions/scala-http4s-sideeffffect/FINDINGS.md)): the score is the most users before the CPU
runs out, and G1, ZGC and Shenandoah spend more CPU per request than Parallel, mostly in the barriers they add to the
application's own code. No heap size changes that.

Concurrent collectors trade CPU for shorter pauses. They win when **pauses, not CPU, decide the result**:

1. **Gigabytes of long-lived data.** Parallel's full collection stops the application and walks everything that is
   alive, roughly 0.5-1 s per GB.
2. **That data keeps changing**, so the old generation keeps filling up and full collections keep coming.
3. **Spare CPU**, so the concurrent collectors' extra work is free.
4. **A score that punishes a single long pause**, like a high percentile at a fixed load, instead of the most users at
   p99 < 1 s (where a few seconds of pause per five minutes hide in the last 1%).

## The variant

No change to the server. Only the load and the score change:

- **Slow users, many connections** (1-3): **100,000 users** with **5×** the workload's think time and session length.
  Every user keeps a keep-alive connection, and Ember holds ~35 KB of heap per open connection, so the long-lived data
  is ~3.5 GB, and it changes as users come and go.
- **A fixed, moderate load** (3): ~5,300 req/s, about 60% of what this setup can serve, so CPU is to spare.
- **Scored on the tail** (4): p99.9, p99.99 and the longest request over a **10-minute** measured window, plus the GC
  log's pauses.

## Running it

```bash
# Postgres with the box's settings and the full seed in Docker (mc-pg-box, port 5434), a build of the submission,
# and the load generator built at /tmp/benchmc/loadgen; then, per collector:
GC="-XX:+UseZGC" HEAP=45 bash run.sh zgc 100000 5 180s 600s
```

`run.sh` pins the app and Postgres to 4 hyper-threaded cores (8 logical CPUs) and four load generators to the other
four, one per target address (one client IP pair has only ~28,000 source ports). `tailq/` reads the percentiles beyond
p99 from the merged HdrHistogram, which the load generator's own summary stops at.

## Results

100,000 users, 5× think time, 3-minute ramp, **10 minutes measured**, heap 45% of a 16 GB cgroup, same code and
database for every run. All four passed the challenge's own criteria (p95 < 500 ms, p99 < 1 s, errors < 1%) except
Shenandoah, but the tails tell different stories:

| GC | req/s | p99 | **p99.9** | p99.99 | **max** | requests > 1 s | GC pauses | app CPU |
|---|---|---|---|---|---|---|---|---|
| Parallel | 5,296 | 793 ms | **2.13 s** | 2.90 s | **3.95 s** | 0.75% | 583, of them 8 full, longest **1.72 s** | 3.28 cores |
| G1 | 5,296 | 153 ms | **249 ms** | 360 ms | **779 ms** | 0 | 464, longest 155 ms | 4.51 cores |
| **ZGC** | 5,299 | **28 ms** | **110 ms** | 242 ms | **507 ms** | 0 | ~1,350 minor + 38 major cycles, concurrent, no allocation stalls | 4.67 cores |
| Shenandoah (generational) | 5,220 | 4.69 s | 6.83 s | 8.35 s | 10.0 s | 9.0% | 2,630, of them 8 full, longest 3.31 s | 4.73 cores |

Every run held ~100,000 open connections, Postgres used ~2.4 cores, and the load generators were never the limit.

- **Parallel** pays for its cheap CPU with **8 full collections over ~3.5 GB of connection state, up to 1.7 s each**:
  the p99.9 is 2.1 s and the worst request waited 4 s. Under the challenge's own score this run *passes* (p99 793 ms);
  under a tail score it is the worst of the three that work.
- **G1** never needs a full collection: its pauses stay under 160 ms, at ~40% more app CPU.
- **ZGC** has the best tail by far: p99 28 ms (28× better than Parallel), p99.9 110 ms (19×), no request over 1 s, at
  ~42% more app CPU than Parallel. With spare cores, that CPU is free.
- **Shenandoah** (generational) collapsed at this load: its concurrent cycles couldn't keep up, so it fell back to
  stop-the-world collections (`Pause Degenerated GC`, then full collections up to 3.3 s). See below for where its
  limit is.

The same server, loaded the challenge's way (most users until the CPU runs out), ranks them the other way round:
Parallel first, G1 4-7% more CPU per request, ZGC 12% more, Shenandoah last. **Which collector wins is decided by the
workload and the score, not by the collector**: CPU-bound throughput favours Parallel; a big, changing live set with
spare cores and a tail-latency score favours ZGC.

## Generational Shenandoah: capabilities and limits

Why it collapsed, from its log: allocation runs at ~970 MB/s and a young cycle takes ~1 s (marking ~430 ms, updating
references ~350 ms). Only young cycles ran; the old generation filled with promoted objects that later died (the heap
after a young cycle grew from ~1.3 GB to 5.2 GB at 45%, 7 GB at 60%), and once that ate the free room, young cycles
couldn't finish before allocation ran out. Each attempt to fix it, at 100,000 users, 10 minutes measured:

| Variant (heap 45% unless noted) | p99 | p99.9 | errors | degenerated / full | longest pause | app CPU |
|---|---|---|---|---|---|---|
| defaults | 4.69 s | 6.83 s | 0 | many / 8 | 3.31 s | 4.73 cores |
| defaults, heap 60% | 4.29 s | 6.50 s | 0 | 27 / 6 | 2.99 s | 4.70 cores |
| old generation collected earlier (`ShenandoahMinOldGenGrowthPercent=10`, `ShenandoahIgnoreOldGrowthBelowPercentage=10`, `ShenandoahGuaranteedOldGCInterval=30000`) | 5.42 s | 7.26 s | 0 | 135 / 21 | 3.09 s | 5.22 cores |
| + objects promoted after 4 young cycles (`ShenandoahGenerationalMinTenuringAge=4`) | 10 s | 10 s | 4.0% | 165 / 30 | 3.22 s | 5.19 cores |
| + 6 GC threads, young cycles start at 20% free (`ConcGCThreads=6`, `ShenandoahMinFreeThreshold=20`) | 10 s | 10 s | 14.6% | **0 / 0** | **2.0 ms** | **5.98 cores** |
| 6 GC threads, 20% free, otherwise defaults | 10 s | 10 s | 5.5% | 10 / 0 | 477 ms | 4.86 cores |

Collecting the old generation earlier made things worse: old marking started 229 times but finished 13 times, because
young cycles keep preempting it. With enough threads it does manage everything concurrently (no degenerated or full
collection, pauses ≤ 2 ms), but then it needs ~6 of the 8 cores next to Postgres, and the requests starve for CPU
instead. **At this load, on this box, it runs out of CPU, where ZGC (4.67 cores) didn't.**

With more CPU to spare, at **70,000 users** (~3,700 req/s), 6 GC threads, 10 minutes:

| GC | p99 | p99.9 | p99.99 | max | longest GC pause | app CPU |
|---|---|---|---|---|---|---|
| **Shenandoah (generational)** | 47 ms | 152 ms | 292 ms | 451 ms | **0.6 ms**, nothing degenerated | 2.97 cores |
| ZGC | 8.6 ms | 65 ms | 133 ms | 306 ms | sub-millisecond | 2.86 cores |

So generational Shenandoah is fully concurrent, with sub-millisecond pauses, at the same CPU cost as ZGC, **as long
as it has headroom**; between 70,000 and 100,000 users on this box it stops keeping up with ~1 GB/s of allocation
plus the old generation of ~100,000 connections, and no setting tried (heap, threads, old-generation triggers,
tenuring) moved that limit without starving the application. ZGC handled 100,000 without a stall, and at 70,000 its
tail is still 2-5× better.

## Ron Pressler's view, and where this data agrees

Ron Pressler (Java architect, Oracle) gave two talks in 2026 on why the JDK's collectors work the way they do:
[Principles of Memory Management in Java](https://www.youtube.com/watch?v=xr73mR7ii9M) (JavaOne, March 2026) and its
sequel [Big Runtimes Make Efficient Programs](https://www.youtube.com/watch?v=vhDwy1eCXbs) (Devoxx Belgium, 2026),
plus the keynote [Java – The Year of the Runtime!](https://www.youtube.com/watch?v=fXBGBovpH88). Read from the
YouTube captions, his points are:

1. **Moving, generational collectors turn RAM into less CPU.** Collection cost is roughly live set × allocation rate
   / headroom; generations make the two terms add instead of multiply, so more headroom pays for a higher allocation
   rate. "RAM chips become hardware accelerators."
2. **Think in RAM per core.** Every machine you can buy has at least ~1 GB per core; a program at 100% CPU holds all
   of it anyway, so unused RAM saves nothing. Always set the heap size (the 25% default "is almost never what you
   want"); automatic heap sizing for ZGC is coming.
3. **ZGC removed GC pauses.** Go's non-moving collector is "primitive", and Go programs hit problems Java had ten years
   ago.
4. **Benchmarks do not generalize.** A result holds only if your whole program is the benchmark; most benchmarks use
   too much CPU and too little RAM.
5. **Allocate naturally, don't pool objects.** Pooled objects get promoted, and writing references into old objects
   costs barrier work. Pooling sockets or connections is fine.

**Where this data agrees:**

- ZGC has no pauses that matter here: at 100,000 users, p99.9 110 ms, no allocation stalls, no request over 1 s.
  Parallel's full collections (up to 1.7 s) are what fail it.
- The submission sets the heap explicitly (45% next to Postgres, with `AlwaysPreTouch`); 45% is a split with
  Postgres's cache on the same box, not RAM frugality.
- It pools only connections and Postgres sessions, the exception he allows.

**Where it adds a caveat:**

- **His cost model leaves out the barriers in the application's own code.** Headroom makes collections rarer; it
  does not remove barrier work. On the challenge's own score (CPU saturated), ZGC needs ~12% more CPU per request
  than Parallel and G1 4-7% more, at 45% and at 60% heap alike, so Parallel wins there at every heap size tried.
- **Objects that live for minutes break the "small old-generation allocation" assumption.** Here each connection's
  state lives minutes, is promoted, then dies. Generational Shenandoah collapsed on exactly that; ZGC coped, but at
  4.67 cores against Parallel's 3.28 (+42%), part of it marking ~3.5 GB of live connection state.
- **"Benchmarks are useless" applies to the challenge too.** It is a whole program with a realistic request mix, but
  its score runs the CPU to saturation, the unnatural mix he warns about. This variant (~60% CPU, a big changing live
  set, a tail score) is closer to the programs he means, and there his conclusion holds: ZGC first.
- **His Rust comparison** (Caffeine with ZGC beat the Rust cache Moka, which spent ~40% CPU deciding when to free) is
  an in-process concurrent cache, and he says himself it does not generalize. This server keeps its state in
  Postgres, and per-request allocations are cheap for jemalloc/mimalloc, so a Rust server may still match or beat ZGC
  on this tail. Not measured.

**Net:** for the services Java is built for (spare CPU, latency targets, a big live set), ZGC with enough heap is the
right default, as he says. A score that runs the CPU to 100% and only checks p99 rewards the opposite trade (no
barriers, occasional long pauses), which is why Parallel wins the challenge as specified.
