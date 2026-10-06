# pbctl

A command line client for [PocketBase](https://pocketbase.io), built for AI agents and still pleasant for people.

- Works with any PocketBase 0.23 or newer over its REST API. Nothing to install on the server.
- Small output by default, so it does not fill an agent's context.
- Read-only by default, with two stronger modes an agent cannot switch off.
- Covers the whole API: records, collections, files, logs, settings, backups, crons, SQL and auth.

## Get started

**1. Install**

```bash
go install github.com/Barney241/pocketbase-cli/cmd/pbctl@latest
```

**2. Add your instance**

```bash
pbctl profile add prod --url https://pb.example.com --identity you@example.com
```

It asks for the password. The profile is read-only unless you add `--writable`.

**3. Check the connection**

```bash
pbctl status
```

**4. Read something**

```bash
pbctl collections list
pbctl collections show posts
pbctl records list posts -f "status = 'open'" -s -created --fields id,title
pbctl logs list --level error --since 1h
```

**5. Tell your agent**

Put this in `CLAUDE.md`, `AGENTS.md` or your system prompt:

```
Use `pbctl` for anything PocketBase: data, schema, logs, files. Run `pbctl guide` once for the syntax.
Never work around a read-only refusal; ask me instead.
```

`pbctl guide` prints a one-page reference of every command and the filter syntax.

## Writing

Add a writable profile for an instance you are happy to change:

```bash
pbctl profile add local --url http://127.0.0.1:8090 --identity admin@example.com --writable
pbctl -p local records create posts title='Hello' views:=0
pbctl -p local records update posts <id> status=done
pbctl -p local records delete posts <id> --yes
```

`--dry-run` prints the request and sends nothing. Destructive commands need `--yes`.

## Read-only

| Level | What stops a write | Use it when |
|---|---|---|
| Client (default) | pbctl refuses to send it | you want to prevent mistakes |
| Gateway | a proxy that holds the password and forwards reads only | the agent has a shell and must not get the credential |
| Server guard | PocketBase itself refuses writes from that superuser | you can add a hook to the server |

The default level stops pbctl, not `curl`: the agent can still read the stored credential. For production, use the gateway or the server guard. Setup for each is in [docs/read-only.md](docs/read-only.md).

What works in read-only mode:

| Area | Works | Refused |
|---|---|---|
| `records` | `list` `get` `count` `watch` | `create` `update` `delete` `import` `batch` |
| `collections` | `list` `show` `export` `diff` `scaffolds` `dry-run-view` | `create` `update` `delete` `truncate` `import` |
| `files` | `list` `url` `download` | upload (`records create/update --file`) |
| `logs` | `list` `get` `stats` | `truncate` |
| `settings` | `get` | `update` `test-s3` `test-email` `apple-client-secret` |
| `backups` | `list` `download` | `create` `upload` `delete` `restore` |
| `crons` | `list` | `run` |
| `sql` | | every query, because the endpoint can write |
| `auth` | `methods` `login` `refresh` | OTP, verification, password reset, email change, `impersonate` |
| `api` | `GET` | `POST` `PATCH` `PUT` `DELETE` |

The gateway and the server guard also refuse `backups download` and `collections dry-run-view`, because both can expose the secrets that sign auth tokens.

What a writable profile adds:

| Area | Commands | What they do |
|---|---|---|
| `records` | `create` `update` `delete` `import` `batch` | change records one at a time, from a JSON Lines file, or as one transaction |
| `collections` | `create` `update` `delete` `truncate` `import` | change the schema and API rules, or empty a collection |
| `files` | `records create/update --file field=path` | upload a file into a record |
| `logs` | `truncate` | delete all logs |
| `settings` | `update` `test-s3` `test-email` `apple-client-secret` | change instance settings and test the storage and mail setup |
| `backups` | `create` `upload` `delete` `restore` | manage backups and restore one |
| `crons` | `run` | trigger a cron job now |
| `sql` | `sql "<query>"` | run raw SQL on the server |
| `auth` | `request-otp` `with-otp` `request-verification` `confirm-verification` `request-password-reset` `confirm-password-reset` `request-email-change` `confirm-email-change` `impersonate` | run the auth flows for a user |
| `api` | `POST` `PATCH` `PUT` `DELETE` | call any endpoint, including your own routes |

Every write accepts `--dry-run`. Destructive ones need `--yes`.

## More

- [docs/reference.md](docs/reference.md): every command, output formats, configuration and exit codes
- [docs/read-only.md](docs/read-only.md): the three read-only levels and their limits
- [AGENTS.md](AGENTS.md): rules for agents using pbctl or changing this repository
- [SECURITY.md](SECURITY.md): reporting a vulnerability

Development: `make test` for unit tests, `make integration` to run the suite against a real PocketBase.

The command surface follows [mabeldata/pocketbase-mcp](https://github.com/mabeldata/pocketbase-mcp) and [mrwyndham/pocketbase-mcp](https://github.com/mrwyndham/pocketbase-mcp).

## License

MIT
