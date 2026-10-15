# The flock's protocol

Geese of different releases of the library fly together during a rollout: the old image's geese and the new image's share the flock's tables until the old ones land. What they write there, and what each may assume of the others, is the protocol. This is protocol **1**.

## The tables

Made by the first goose to join, if missing, outside the application's migrations.

### `geese_flock`: one row per goose

| Column | |
| --- | --- |
| `id` | The goose's own id, new for every process. |
| `name` | Who it is for people reading the table, such as its pod's UID. Nothing decides on it. |
| `protocol` | The protocol it speaks. |
| `serves_from`, `serves_to` | The schema versions its image serves. |
| `capabilities` | The capabilities it serves: its own and the retired ones it accepts. |
| `release_major` | Its release's major version; null for a build that isn't a release. |
| `gathered_for` | The schema version it has gathered for: it admits nothing until that version is applied. Null when it isn't gathered. |
| `heard_until` | When the flock stops counting it, unless it honks again: ten seconds after its last honk, by the database's clock. |
| `joined_at` | When it joined. |

A goose writes its row with every honk, every two seconds (every quarter second while the flock has a heading), and deletes it when it lands. Rows whose `heard_until` has passed are of geese gone; nothing reads them.

### `geese_flight`: one row, the flock's shared decisions

| Column | |
| --- | --- |
| `heading_for` | The schema version the leader is migrating to; null when the flock isn't migrating. |
| `departed_at` | When the leader set the heading. |
| `active_capabilities` | The capabilities switched on. Only ever grows, until a baseline retires them. |
| `oldest_page_major` | The oldest major release whose pages the flock serves; null until set. Never lowered. |
| `failed_for` | The schema version the last attempt to migrate to didn't reach; null once the flock has migrated. |
| `failures` | How many attempts at `failed_for` failed in a row. |
| `retry_at` | When a leader may try `failed_for` again, by the database's clock. |
| `failure` | Why the last attempt failed, for people. Nothing decides on it. |

## Protocol 0

The coordinator geese was extracted from, as `go-monolith-template` ran it, with `schema_instances` and `schema_upgrade_state`. Its pods never read the flock's tables. Protocol 1 keeps the flock safe from them: the first goose carries over what they decided (`active_capabilities`, `oldest_page_major`) when it makes `geese_flight`, no goose joins while they're migrating, and no leader decides anything while one of their pods is heard. See [adopting geese](docs/adopting.md).

## What every goose does

- **Honks** in a transaction holding the coordination lock (`pg_advisory_xact_lock(0x6765657365636f6f)`), after reading the schema version applied and `geese_flight`.
- **Grounds itself**, cancelling the work it admitted, when it can't serve the schema applied, the heading, an active capability, or the oldest page (its release is older, or isn't a release once a floor is set), and when its own deadline passes: eight seconds after the start of its last successful honk, by its own monotonic clock, so it stops before the flock stops counting it.
- **Gathers** when `heading_for` is ahead of the schema applied: holds new work, interrupts streams, lets admitted work finish (cancelling what runs past ten seconds), and writes `gathered_for` once nothing is running. It stops gathering when the heading is cleared.

## What the leader does

The leader holds `pg_try_advisory_lock(0x67656573656c6561)` on a connection of its own, and acts only in calm weather. Each decision is made in a transaction holding the coordination lock, and **only while every goose heard speaks the leader's protocol** and no pod of the coordinator geese came from is heard (a live row in `schema_instances`).

- **Migrating**: when every goose heard serves the next version, it sets `heading_for` to it, waits up to thirty seconds for every goose heard to have `gathered_for` equal to it, runs the migration, and clears the heading and `failed_for`. A heading equal to the schema applied is a migration its leader didn't live to finish: the next leader clears it.
- **An attempt that doesn't migrate**, because the flock didn't gather in time or the migration failed, leaves the schema as it was (the flock migrates only in a transaction). The leader clears the heading, so the flock goes on at the schema applied, and records the attempt in `failed_for`, `failures` and `retry_at`: no leader tries that version again before `retry_at`, thirty seconds after the first failure, twice as long after each one after, up to ten minutes.
- **Activating a capability**: when the flock has no heading, the capability's schema is applied, its prerequisite holds, and every goose heard lists it in `capabilities`.
- **Raising the oldest page**: when the flock has no heading, to the lowest `release_major` of any goose heard, if every goose heard has one and it's above the current floor.

## Changing the protocol

A new protocol is a new library release whose geese write something the old ones don't read, or read something the old ones don't write. It must keep to this:

1. **Add, never take away.** New columns are added if missing, as protocol 1 makes its tables, and old columns keep their meaning: old geese keep reading and writing them during a rollout.
2. **Follow before leading.** A goose of protocol N+1 joins a flock of protocol N and honks as N's geese expect. It leads nothing until every goose heard speaks N+1, and N's geese, which already lead nothing while they hear a goose of another protocol, wait for it in turn. During a rollout neither leads: migrations, activations and a raised floor wait until it's done.
3. **Say what it changes here**, with the protocol number.

A rollout from one protocol to the next is therefore a rollout without a migration. An image that brings a new protocol and a migration migrates once its rollout is done.
