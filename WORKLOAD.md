# Workload

What the load generator does to your server, and how a run is judged. It's the same workload as the video.

## Seed data (deterministic, `bench/seed`)

| Table | Rows | Distribution |
|---|---|---|
| users | 100,000 | 30% are authors |
| articles | 500,000 | 2 years of history; author ~ Zipf(0.8) over authors, so a few are prolific; body ~ 1 KB |
| tags | 200 | 1–4 per article, tag ~ Zipf(1.0) |
| favorites | ~5,000,000 | per article ~ lognormal (mean ≈ 10, cap 2,000), so some articles are viral |
| comments | ~2,500,000 | per article ~ lognormal (mean ≈ 5, cap 200) |
| follows | ~2,000,000 | each user follows ~20 authors (lognormal), author ~ Zipf(0.8) |

That's 2.35 GB in Postgres, so the whole database fits in the benchmark box's RAM.
All seeded users have the password `password123`.

## The virtual user (closed model, one goroutine = one human)

Think time and session length follow **TPC-W**, the classic e-commerce benchmark's user model
(Menascé, CMG 2001):

- **Think time** between page views: negative-exponential, mean **7 s**, capped at 70 s.
- **Session length:** negative-exponential, mean **15 min**, capped at 60 min. Then a new session starts.
- **Users who arrive during the ramp start mid-session** (already logged in). This keeps session starts, and the
  bcrypt logins they include, at the steady-state rate (users ÷ 15 min) from the first second. The first pilot
  started every user with a fresh session, which put a 15×-normal login storm into the 60 s ramp, and that
  backlog failed probes the steady state passed easily.
- **Session start** (app boot):
  - 75% are returning users with a saved token: `GET /api/user`.
  - 24.5% log in again: `POST /api/users/login`, a bcrypt verify.
  - 0.5% are new sign-ups: `POST /api/users`, a bcrypt hash. They then browse as that new user.

Each page view makes the API calls the real Conduit frontend makes for that page:

| Page (weight %) | API calls |
|---|---|
| Home, global feed (22) | `GET /api/tags`, `GET /api/articles?limit=10&offset=k` |
| Home, "your feed" (15) | `GET /api/tags`, `GET /api/articles/feed?limit=10&offset=k` |
| Tag page (8) | `GET /api/articles?tag=T&limit=10&offset=k` |
| Article page (35.3) | `GET /api/articles/:slug`, `GET /api/articles/:slug/comments` |
| Profile page (8) | `GET /api/profiles/:u`, `GET /api/articles?author=u&limit=5` |
| Profile "favorited" tab (2) | `GET /api/profiles/:u`, `GET /api/articles?favorited=u&limit=5` |
| Favorite toggle (4) | `POST` or `DELETE /api/articles/:slug/favorite` on the current article |
| Post a comment (2.5) | `POST /api/articles/:slug/comments` |
| Follow toggle (1.5) | `POST` or `DELETE /api/profiles/:u/follow` |
| Publish an article (0.8) | `POST /api/articles`, then the new article's page (`GET` article + comments) |
| Edit own article (0.3) | `GET /api/articles/:slug`, `PUT /api/articles/:slug` |
| Delete own comment (0.3) | `DELETE /api/articles/:slug/comments/:id` (a comment made this session) |
| Settings (0.3) | `GET /api/user`, `PUT /api/user` (new bio) |

**What a user clicks:**
- Pagination `offset`: page 1 70% of the time, otherwise pages 2–5.
- Articles: 80% are clicked from the last list the user saw; 20% are deep links, with popularity Zipf(0.9)
  skewed to recent articles.
- Tags: 50% from the current article's tags, 50% Zipf over all tags.
- Profiles: the author of the current article.
- "Edit own article" happens only if the user published one this session, and "delete own comment" only if they
  commented this session. Otherwise that page view becomes an article page view. A page that needs a current
  article when there isn't one (e.g. favorite) first opens an article page.

**What that adds up to:** ~1.8 API calls per page view, so ~0.25 requests/s per active user. About **5% of
requests are writes**, close to TPC-W's "browsing" mix (95/5). Social apps are even more read-heavy
(Facebook TAO: 99.8% reads).

## Load generator: `bench/cmd/loadgen` (custom Go)

Each virtual user is a goroutine with its own keep-alive connection, as a browser would have, costing tens of KB.
Several load boxes split the users, every box records latency in HdrHistograms per endpoint, and the results are
merged exactly. Each load box samples its own CPU every second. **A run where any load box goes above 75% CPU is
flagged "LG-limited" and never counted as your server's ceiling**: I add load capacity and run it again.

## Pass/fail and scoring

At a given number of concurrent users, measured over a steady window after ramp-up, a run passes when:
- **p95 < 500 ms**,
- **p99 < 1 s**, and
- **errors < 1%** (non-2xx/3xx responses, timeouts at 10 s, connection errors).

These are judged over all requests combined; per-endpoint figures are reported as well.

**Search**, from a fresh copy of the seeded database:
1. A 3-minute warm-up at a low load (JIT, caches, pools).
2. 2-minute probes (60 s ramp + 60 s measured): double until one fails, then bisect to a 5% resolution.
3. The **score** is one **5-minute hold** (60 s ramp + 5 min measured) at the highest passing probe, stepping
   down 5% until a hold passes.

**Tail-latency track** (`-track tail`, see README.md): the same virtual user at **5×** think time and session
length (so 5× the users per req/s, each holding a connection). A run passes when **p99 < 250 ms**, **p99.9 < 500 ms**,
**p99.99 < 1 s** and **errors < 0.1%**. Probes measure 3 minutes, the hold 10 minutes; everything else
is as above.

**Recorded per run:**
- users
- req/s
- p50/p95/p99 per endpoint (plus overall p99.9, p99.99 and p99.999)
- error breakdown
- target CPU split into nginx / app / postgres
- steal %
- RAM
- Postgres connections
- load-box CPU
