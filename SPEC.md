# API spec

The [RealWorld "Conduit"](https://github.com/gothinkster/realworld) backend: a Medium clone with 19 endpoints
(auth, profiles, follows, articles, a personal feed, tags, comments and favorites), with the deviations listed at the
end. `bench/cmd/conformance` (197 steps) checks everything on this page, and when the two disagree, the suite wins.

## Runtime contract

Your server reads its config from these environment variables:

| Variable | Value on the benchmark box |
|---|---|
| `DATABASE_URL` | `postgres://conduit:conduit@127.0.0.1:5432/conduit`: a fresh copy of the seed database |
| `JWT_SECRET` | `multicore-server-challenge` |
| `HOST`, `PORT` | `127.0.0.1`, `3000` behind Nginx; `0.0.0.0`, `80` if you serve directly |
| `CONDUIT_TRACK` | `standard`, or `tail` on the tail-latency track (README.md); optional to read |

Listen on `HOST:PORT` and speak HTTP/1.1 with keep-alive. Compression is optional (Nginx gzips JSON over 1 KB if you use it).

- **Postgres** is PostgreSQL 18 on the same box, configured by [`box/pg-conf.sh`](box/pg-conf.sh) (pgtune "web
  application" for its cores and RAM) with **`max_connections = 100`**. Keep your total connections, across every
  process, at 95 or fewer. The role `conduit` can read and write rows and use the sequences, and nothing else.
- **Behind Nginx** ([`box/nginx.conf`](box/nginx.conf), [`box/site.conf`](box/site.conf)): Nginx keeps up to 256
  idle connections per worker to you, and its upstream keep-alive is 60 s. Keep idle connections open for at least
  65 s so Nginx never reuses a socket you just closed.
- **Direct**: every virtual user holds its own keep-alive connection (as a browser does), so expect tens of thousands
  of open connections. Your service gets `LimitNOFILE=200000` and `CAP_NET_BIND_SERVICE` (it doesn't run as root).
- `GET /api/tags` returning 200 is the health check: it must answer within 60 s of `start.sh`.

## Authentication

- Header `Authorization: Token <jwt>` (RealWorld's scheme, not `Bearer`).
- JWT HS256, secret `JWT_SECRET`. Claims: `{"sub": "<user id as a decimal string>", "iat": <now>, "exp": <now + 30 days>}`.
- Each request verifies the signature and `exp`, and rejects any `alg` other than HS256. There is **no verification cache**.
- **Auth required** endpoints: a missing or invalid token gives 401.
- **Auth optional** endpoints: no header means anonymous (`viewer = 0`). A header that is present but invalid gives 401.
- `viewer` is the token's user id, or `0` when anonymous. It is passed to SQL as a bigint. No user has id 0.

## Formats

- Timestamps are RFC 3339 in UTC with exactly 3 fractional digits: `"2025-04-01T12:00:00.000Z"` (truncate, don't round).
- `bio` and `image` are JSON `null` when the column is NULL.
- `favoritesCount` is a JSON integer.
- `tagList` is an array of strings, in the order SQL returns it (sorted by name).
- Errors (401/403/404/422) return `{"errors": {"body": ["<message>"]}}`. The message text is free. Conformance
  checks the status and the shape only.
- Response field order doesn't matter. `Content-Type: application/json` on every JSON response.
- `204` responses have an empty body.

Objects:

```
User     {"email", "token", "username", "bio", "image"}
Profile  {"username", "bio", "image", "following"}
Article  {"slug", "title", "description", "body", "tagList", "createdAt", "updatedAt",
          "favorited", "favoritesCount", "author": Profile}
         (list endpoints omit "body", as in the current RealWorld spec)
Comment  {"id", "createdAt", "updatedAt", "body", "author": Profile}
```

## SQL

The statements below are the **reference SQL**: what the four examples run, in this order, and what defines the
right answer (ordering, slugs, counts, the 1,000 cap). You can use any SQL you like, as long as the responses come
out the same. `$n` placeholders become your driver's syntax.

### Shared SQL fragments

`ARTICLE_LIST_SELECT` (`$1` = viewer):

```sql
SELECT a.id, a.slug, a.title, a.description, a.created_at, a.updated_at,
       ARRAY(SELECT t.name FROM article_tags at JOIN tags t ON t.id = at.tag_id
              WHERE at.article_id = a.id ORDER BY t.name) AS tag_list,
       (SELECT count(*) FROM favorites f WHERE f.article_id = a.id) AS favorites_count,
       EXISTS (SELECT 1 FROM favorites f WHERE f.article_id = a.id AND f.user_id = $1) AS favorited,
       u.username, u.bio, u.image,
       EXISTS (SELECT 1 FROM follows fo WHERE fo.follower_id = $1 AND fo.followee_id = a.author_id) AS following
  FROM articles a
  JOIN users u ON u.id = a.author_id
```

`ARTICLE_FULL_SELECT` is the same query with `a.body` added after `a.description`.

`SLUGIFY(x)` is shorthand for `trim(both '-' from lower(regexp_replace(x, '[^A-Za-z0-9]+', '-', 'g')))`.
Computing the slug outside SQL is fine if it comes out byte-identical.

`ORDER_PAGE` is `ORDER BY a.id DESC LIMIT $2 OFFSET $3`. Lists are newest-first by id: ids come from an identity
sequence in insertion order, so id order is creation order, and every list can walk an index and stop after one page.

## Endpoints

Query parameters `limit` (default 20, valid 1–100) and `offset` (default 0, ≥ 0) must be integers in range,
otherwise the response is 422. Each endpoint runs the statements listed, in order. "→ 404" means: if that
statement returns no row, respond 404 and stop.

### Users and auth

**POST /api/users**: register (no auth). Body `{"user": {"username", "email", "password"}}`, all non-empty
strings, otherwise 422. The password is hashed with bcrypt cost 10.
```sql
INSERT INTO users (username, email, password_hash) VALUES ($1, $2, $3)
RETURNING id, username, email, bio, image
```
A unique violation (SQLSTATE 23505) gives 422. Success is **201** `{"user": User}` with a fresh token.

**POST /api/users/login** (no auth). Body `{"user": {"email", "password"}}`, both non-empty, otherwise 422.
```sql
SELECT id, username, email, bio, image, password_hash FROM users WHERE email = $1
```
No row, or bcrypt mismatch, gives 401. Success is 200 `{"user": User}` with a fresh token.

**GET /api/user** (auth required).
```sql
SELECT id, username, email, bio, image FROM users WHERE id = $1
```
No row gives 401. Returns 200 `{"user": User}`; `token` is the token the request presented.

**PUT /api/user** (auth required). Body `{"user": {...}}` with any of `email`, `username`, `password`, `bio`,
`image`. Fields that are absent or `null` are unchanged. A new password is hashed with bcrypt cost 10.
```sql
UPDATE users SET email = COALESCE($2, email), username = COALESCE($3, username),
                 password_hash = COALESCE($4, password_hash), bio = COALESCE($5, bio), image = COALESCE($6, image)
 WHERE id = $1
RETURNING id, username, email, bio, image
```
A unique violation gives 422; no row gives 401. Returns 200 `{"user": User}` with the presented token.

### Profiles

**GET /api/profiles/:username** (auth optional).
```sql
SELECT u.id, u.username, u.bio, u.image,
       EXISTS (SELECT 1 FROM follows fo WHERE fo.follower_id = $1 AND fo.followee_id = u.id) AS following
  FROM users u WHERE u.username = $2                                                   → 404
```
Returns 200 `{"profile": Profile}`.

**POST /api/profiles/:username/follow** and **DELETE …/follow** (auth required).
```sql
SELECT id, username, bio, image FROM users WHERE username = $1                         → 404
-- POST:
INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING
-- DELETE:
DELETE FROM follows WHERE follower_id = $1 AND followee_id = $2
```
Returns 200 `{"profile": Profile}` with `following` true after POST and false after DELETE.

### Articles

**GET /api/articles** (auth optional). Filters `tag`, `author` and `favorited`. If more than one is given, only
the first in that order applies. Each request runs two statements: the page, then the capped count.

| Filter | Page | Count |
|---|---|---|
| none | `ARTICLE_LIST_SELECT ORDER_PAGE` | `SELECT count(*) FROM (SELECT 1 FROM articles a LIMIT 1000) s` |
| tag = $4 | `ARTICLE_LIST_SELECT WHERE a.id IN (SELECT at.article_id FROM article_tags at WHERE at.tag_id = (SELECT t.id FROM tags t WHERE t.name = $4) ORDER BY at.article_id DESC LIMIT $2 OFFSET $3) ORDER BY a.id DESC` | `SELECT count(*) FROM (SELECT 1 FROM article_tags at WHERE at.tag_id = (SELECT t.id FROM tags t WHERE t.name = $1) LIMIT 1000) s` |
| author = $4 | `ARTICLE_LIST_SELECT WHERE a.author_id = (SELECT au.id FROM users au WHERE au.username = $4) ORDER_PAGE` | `SELECT count(*) FROM (SELECT 1 FROM articles a WHERE a.author_id = (SELECT u.id FROM users u WHERE u.username = $1) LIMIT 1000) s` |
| favorited = $4 | `ARTICLE_LIST_SELECT WHERE a.id IN (SELECT f.article_id FROM favorites f WHERE f.user_id = (SELECT fu.id FROM users fu WHERE fu.username = $4) ORDER BY f.article_id DESC LIMIT $2 OFFSET $3) ORDER BY a.id DESC` | `SELECT count(*) FROM (SELECT 1 FROM favorites f WHERE f.user_id = (SELECT fu.id FROM users fu WHERE fu.username = $1) LIMIT 1000) s` |

Returns 200 `{"articles": [Article without body], "articlesCount": <count>}`. An unknown tag or user gives an empty list, not 404.

**GET /api/articles/feed** (auth required).
```sql
ARTICLE_LIST_SELECT WHERE a.id IN (SELECT x.id FROM follows fo CROSS JOIN LATERAL
    (SELECT a2.id FROM articles a2 WHERE a2.author_id = fo.followee_id ORDER BY a2.id DESC LIMIT ($2::int + $3::int)) x
  WHERE fo.follower_id = $1 ORDER BY x.id DESC LIMIT $2 OFFSET $3) ORDER BY a.id DESC
SELECT count(*) FROM (SELECT 1 FROM articles a WHERE a.author_id IN (SELECT fo.followee_id FROM follows fo WHERE fo.follower_id = $1) LIMIT 1000) s
```
Returns 200, in the same shape as the list.

**GET /api/articles/:slug** (auth optional).
```sql
ARTICLE_FULL_SELECT WHERE a.slug = $2                                                  → 404
```
Returns 200 `{"article": Article}`.

**POST /api/articles** (auth required). Body `{"article": {"title", "description", "body", "tagList"?}}`. Title,
description and body must be non-empty strings, otherwise 422. `tagList` is optional, an array of non-empty strings.
The insert runs **in one transaction**:
```sql
BEGIN
INSERT INTO articles (id, slug, author_id, title, description, body)
SELECT n.id, SLUGIFY($2) || '-' || n.id, $1, $2, $3, $4
  FROM (SELECT nextval(pg_get_serial_sequence('articles', 'id')) AS id) n
RETURNING id
-- only if tagList is non-empty ($1 = tagList as text[]):
INSERT INTO tags (name) SELECT DISTINCT unnest($1::text[]) ON CONFLICT (name) DO NOTHING
INSERT INTO article_tags (article_id, tag_id) SELECT $1, t.id FROM tags t WHERE t.name = ANY($2::text[])
COMMIT
```
After the commit, it runs `ARTICLE_FULL_SELECT WHERE a.id = $2` and returns **201** `{"article": Article}`.

**PUT /api/articles/:slug** (auth required). Body `{"article": {...}}` with any of `title`, `description` and `body`.
Fields that are absent or `null` are unchanged.
```sql
SELECT id, author_id FROM articles WHERE slug = $1                                     → 404; author_id ≠ viewer → 403
UPDATE articles SET title = COALESCE($2, title), description = COALESCE($3, description), body = COALESCE($4, body),
       slug = CASE WHEN $2::text IS NULL THEN slug ELSE SLUGIFY($2) || '-' || id END,
       updated_at = now()
 WHERE id = $1
ARTICLE_FULL_SELECT WHERE a.id = $2
```
Returns 200 `{"article": Article}`.

**DELETE /api/articles/:slug** (auth required).
```sql
SELECT id, author_id FROM articles WHERE slug = $1                                     → 404; author_id ≠ viewer → 403
DELETE FROM articles WHERE id = $1
```
Returns **204**.

### Favorites

**POST /api/articles/:slug/favorite** and **DELETE …/favorite** (auth required).
```sql
SELECT id FROM articles WHERE slug = $1                                                → 404
-- POST:
INSERT INTO favorites (user_id, article_id) VALUES ($1, $2) ON CONFLICT DO NOTHING
-- DELETE:
DELETE FROM favorites WHERE user_id = $1 AND article_id = $2
ARTICLE_FULL_SELECT WHERE a.id = $2
```
Returns 200 `{"article": Article}`.

### Comments

**GET /api/articles/:slug/comments** (auth optional).
```sql
SELECT id FROM articles WHERE slug = $1                                                → 404
SELECT c.id, c.body, c.created_at, c.updated_at, u.username, u.bio, u.image,
       EXISTS (SELECT 1 FROM follows fo WHERE fo.follower_id = $1 AND fo.followee_id = c.author_id) AS following
  FROM comments c JOIN users u ON u.id = c.author_id
 WHERE c.article_id = $2
 ORDER BY c.created_at DESC, c.id DESC
 LIMIT 100
```
Returns 200 `{"comments": [Comment]}`: the newest 100 comments (the seed caps comments per article at 200, mean 5).

**POST /api/articles/:slug/comments** (auth required). Body `{"comment": {"body"}}`, non-empty, otherwise 422.
```sql
SELECT id FROM articles WHERE slug = $1                                                → 404
WITH c AS (INSERT INTO comments (article_id, author_id, body) VALUES ($1, $2, $3)
           RETURNING id, body, created_at, updated_at, author_id)
SELECT c.id, c.body, c.created_at, c.updated_at, u.username, u.bio, u.image
  FROM c JOIN users u ON u.id = c.author_id
```
Returns **201** `{"comment": Comment}`, with `author.following` = false.

**DELETE /api/articles/:slug/comments/:id** (auth required). A non-integer `:id` gives 404.
```sql
SELECT c.author_id FROM comments c JOIN articles a ON a.id = c.article_id
 WHERE c.id = $1 AND a.slug = $2                                                       → 404; author_id ≠ viewer → 403
DELETE FROM comments WHERE id = $1
```
Returns **204**.

### Tags

**GET /api/tags** (no auth).
```sql
SELECT name FROM tags ORDER BY name
```
Returns 200 `{"tags": [...]}`.

## Deviations from the RealWorld spec

- `articlesCount` is **capped at 1,000** (`LIMIT 1000` inside the count). Real apps cap or estimate pagination totals.
- `GET /api/tags` returns all tags sorted by name, not "popular" tags. The spec doesn't define "popular".
- Creates return 201 and deletes return 204. Error bodies use RealWorld's `errors.body` shape for every error.
- Only one list filter applies at a time.
- **Comments are capped at the newest 100.** Real apps paginate comment threads, and under load the hottest
  article's thread would otherwise grow all run long.
- Lists page **newest-first by id**, so every list can walk an index and stop after one page.
