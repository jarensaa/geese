geese runs schema migrations for Go services that run as many processes on one PostgreSQL database, while they serve: no downtime and no disruption, every step of expand and contract checked, and downtime only for a migration that asks for it.

Migration tools like [goose](https://github.com/pressly/goose) apply migrations; geese decides **when**. Each process says, in the database, which schema versions it can serve. One leads, and the flock runs a migration only once every process can make the journey, and only if it keeps the code they run working. It runs inside your application, with no Job, operator or sidecar.

```sh
go get github.com/jarensaa/geese
```

## Start here

| | |
| --- | --- |
| [Getting started](../../docs/getting-started.md) | Run a service as a flock of two processes, and ship a migration while they serve. |
| [How geese works](../../docs/concepts.md) | The flock, and a migration step by step. |
| [Adopting geese](../../docs/adopting.md) | A real service's checklist: background work, Kubernetes, capabilities and CI. |

## Reference

| | |
| --- | --- |
| [Capabilities](../../docs/capabilities.md) | Switching features on across a deployment, telling stale clients to reload, and the contract clients keep. |
| [Transports](../../docs/transports.md) | HTTP, gRPC, queues and jobs. |
| [The protocol](../../PROTOCOL.md) | What the processes write in the database, and assume of each other. |
| [Go reference](https://pkg.go.dev/github.com/jarensaa/geese) | Every package, type and function. |
