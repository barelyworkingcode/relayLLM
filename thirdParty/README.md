# relayLLM / thirdParty

Glue scripts for launching and registering external services that relayLLM
talks to during development. Nothing in this directory is built by
`relayLLM/build.sh` — each subservice has its own register/run pair that is
invoked by hand when the dev setup changes.

## Why this exists

relayLLM supports OpenAI-compatible chat providers. We test that path against
[oMLX](https://github.com/jundot/omlx), with a local dev checkout at
`~/source/oMLX`. Running `~/source/oMLX/start.sh` by hand works, but we want
Relay to own the process lifecycle the same way it does for relayLLM, eve, and
relayScheduler — so `relay` start/stop also start/stop the oMLX dev instance.

## Conventions

For each third-party service `<name>`:

- `run.<name>.sh` — the long-running command Relay invokes. Activates the
  right environment and `exec`s the server. **Bootstrap logic from the
  upstream project's own start script is replicated inline, not `source`d or
  symlinked**, so this directory can be refreshed independently of the
  upstream checkout and stays self-contained.
- `register.<name>.sh` — one-shot. Calls
  `/Applications/Relay.app/Contents/MacOS/relay service register` pointing
  `--command` at the sibling `run.<name>.sh`, with `--url` set to whatever
  page the tray menu should open. Re-run any time registration metadata
  (name, URL, autostart) needs refreshing; `relay service register` upserts.

## Current services

### oMLX

- Upstream: https://github.com/jundot/omlx
- Dev checkout: `~/source/oMLX`
- Admin dashboard: http://127.0.0.1:8000/admin
- API endpoint: http://127.0.0.1:8000/v1
- Register: `./register.oMLX.sh`
- Runtime (invoked by Relay): `./run.oMLX.sh`
