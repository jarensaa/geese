# Capabilities and the oldest page served

A flock agrees on two things clients need: which features are switched on across the deployment, and the oldest release whose clients it still serves. The server's half is a few lines with geese, over any transport ([transports](transports.md)). The client's half is a contract any client can keep, in any language or framework; this document is that contract. Its examples are HTTP; [transports](transports.md) says what each rule is in gRPC's terms.

## On the server

### Declare what a release promises

A feature that needs a newer schema, or that every process must serve before anyone uses it, is a capability:

```go
geese.Config{
	// ...
	Capabilities: []geese.Capability{
		{Name: "comments", MinimumSchema: 7},
		// Switched on only once its backfill is done.
		{Name: "search", MinimumSchema: 8, Ready: func(ctx context.Context, tx *sql.Tx) (bool, error) {
			var pending bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM documents WHERE search_vector IS NULL)`).Scan(&pending)
			return !pending, err
		}},
	},
}
```

The leader switches a capability on once its schema is applied, its `Ready` says so, and every process it hears serves it. It never switches off. When a baseline makes a capability unconditional, its name moves from `Capabilities` to `Retired`: deployed databases still list it, and the image accepts it without reporting it.

### Refuse what isn't on

A handler of the feature asks first, whatever its transport:

```go
if err := flock.Require(ctx, "comments"); err != nil {
	return notYet(err) // geese.ErrInactive while it's off; another error while what's on can't be read
}
```

and answers "not yet" in its transport's terms: over HTTP, `503` with `{"code": "feature_unavailable", "message": "..."}`, which `geesehttp.Require(flock, "comments", handler)` does for a route; over gRPC, `UNAVAILABLE` with the reason `FEATURE_UNAVAILABLE`. Never fake an answer for a feature that's off: clients read "not yet", and wait.

### Serve what's on

`flock.Capabilities(ctx)` is what clients read. Over HTTP:

```go
mux.Handle("GET /api/capabilities", geesehttp.Capabilities(flock))
```

It answers, never cached:

```json
{"capabilities": ["comments", "search"], "oldestPage": "8.0.0"}
```

`oldestPage` is absent until the flock has raised a floor. It needs no session: it says nothing a client couldn't learn by trying. Over gRPC it's an RPC of the service's own ([transports](transports.md#grpc)). An API whose handlers are generated from an OpenAPI contract calls `flock.Capabilities(ctx)` in its own handler and describes it with this fragment:

```yaml
paths:
  /api/capabilities:
    get:
      operationId: getCapabilities
      responses:
        '200':
          description: What the deployment has switched on, and the oldest release whose pages it serves
          headers:
            Cache-Control: { schema: { type: string, enum: [no-store] } }
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Capabilities' }
        '503':
          description: Unavailable for a moment (feature_unavailable)
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Problem' }
components:
  schemas:
    Capabilities:
      type: object
      additionalProperties: false
      required: [capabilities]
      properties:
        capabilities:
          type: array
          items: { type: string }
          description: Capabilities switched on across the deployment. Clients treat names they don't know as off.
        oldestPage:
          type: string
          pattern: '^(0|[1-9][0-9]*)\.0\.0$'
          description: The oldest release whose pages the deployment serves; absent until one is set.
    Problem:
      type: object
      required: [code, message]
      properties:
        code: { type: string }
        message: { type: string }
```

### Say which release this is

`Config.Release` is the image's SemVer release, set at build time (`-ldflags "-X main.version=$VERSION"`), and a client shipped with it, such as the web app the server serves, is built with the same release. A release that removes or changes what clients call is a major one: that is what raises the floor once every process runs it. Conventional Commits and semantic-release make this automatic (`feat!:`); a check such as [oasdiff](https://github.com/oasdiff/oasdiff) on the OpenAPI contract catches a breaking change that isn't marked.

## On the client

A client keeps these rules. They're the whole contract; its language, framework and transport are its own business. "The page" is any client shipped with a release: a web app, most of all one a service worker keeps, but also a mobile or desktop app.

1. **Read the capabilities when the page opens**, and again when it comes back into view (in a browser, window focus, or `visibilitychange` to visible), at most once a minute.
2. **A capability is off until it's read and listed.** A name the client doesn't know is ignored; a name it knows that isn't listed is off. A feature waits for its capability: its controls aren't shown, or say "coming soon".
3. **Keep what was known when a read fails** (offline, or unavailable). Don't switch features off because the network blinked.
4. **Before its first change, wait for the first read.** A call that changes something (over HTTP, anything but `GET` and `HEAD`) waits until the capabilities have been read once, so a page opened from an older release never sends one before it knows it may. Reads don't wait: opening stays fast.
5. **An answer it didn't expect means read again.** An answer saying the server doesn't have, or no longer understands, what was asked (HTTP `400`, `404`, `405` or `410`; gRPC `UNIMPLEMENTED`) may mean it's gone: read the capabilities again, at most every 30 seconds.
6. **"Not yet" means not yet.** A feature refused as unavailable (HTTP `503 feature_unavailable`, gRPC `UNAVAILABLE` with `FEATURE_UNAVAILABLE`) is read about again before it's tried again.
7. **Know the page's own release.** Build it in (Vite's `define`, an environment variable, the app's version). Its major is the SemVer major (`8` of `8.2.1+abc`); a build that isn't a release (`dev`, a pre-release) has none, and is never below the floor.
8. **Below the floor, stop and move on.** When the page's major is below `oldestPage`'s major:
   - Send no more API requests. Everything it would send fails at once, before it leaves the page.
   - Say so, in place of the page: "updated, loading the new version".
   - Load the current release. A web page with a service worker asks it to update, and reloads if the worker then keeps a different release (below); without a worker, it reloads. An installed app asks its person to update.
   - Reload at most once every 30 seconds, remembered across reloads (`sessionStorage`), so a page that comes back just as old, from a cache in between, waits instead of reloading on. While it can't move on, try again every 30 seconds and when the connection returns.

### The service worker

A service worker that keeps the app keeps one release, and answers which, so a page knows whether opening again would load a newer one:

```js
self.addEventListener('message', (event) => {
  if (event.data?.type === 'release') event.ports[0]?.postMessage({ release: BUILD_RELEASE })
})
```

A page asks with a `MessageChannel`, `worker.postMessage({type: 'release'}, [channel.port2])`, and treats no answer within a few seconds as "doesn't say". A worker that doesn't say leaves opening the current release to the server.

A complete client, in React with openapi-fetch and a service worker, is in [go-monolith-template](https://github.com/jarensaa/go-monolith-template): `web/src/lib/capabilities.ts`, `release-floor.ts` and `app-update.ts`, and `web/public/sw.js`.
