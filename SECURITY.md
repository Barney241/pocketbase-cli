# Security

pbctl's read-only levels, and what each one does and does not protect against, are described in [docs/read-only.md](docs/read-only.md).

A bug counts as a vulnerability when:

- a request that changes data leaves pbctl while read-only is on,
- a request passes the gateway (`pbctl gateway`) or the server guard (`pbguard`, `pbctl guard hook`) and changes data on the upstream PocketBase, or returns an auth token, a file token, a backup, a password hash or token-signing material,
- a web page or another host can read through a gateway that listens on loopback.

Please report these privately through GitHub's "Report a vulnerability" on this repository instead of a public issue.
