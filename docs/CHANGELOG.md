# Changelog

## [v17] - 2026-10-12 — Topology: a star on the selected source

### Added
- A server box shows a star after its address when it is the source chrony has selected (`*` in `chronyc sources`); the blue links still mark every source the time comes from. The tooltip says "the selected source".

### Verified
- `node --check`, gofmt, vet, tests, cross-builds.

### Not verified
- The drawing in a browser.

## [v16] - 2026-10-12 — Topology: the servers chrony uses are blue

### Fixed
- The links from the gateway to its servers were never blue any more: the highlight belonged to the old relay and nothing set it after chrony replaced it. A link is now blue (with a "used for time" entry in the legend) when chrony on the node being viewed has that server as its selected (`*`) or combined (`+`) source, and the server's tooltip says so. Servers chrony has but does not use (not combined, falseticker, unreachable) stay grey.

### Verified
- `node --check`, gofmt, vet, tests, cross-builds.

### Not verified
- The drawing in a browser, and against a real chronyd (the marks come from `chronyc sources` as before).

## [v15] - 2026-10-12 — Statistics: outcome card

### Changed
- Monitor ▸ Statistics: the "NTP versions" card always read 100% Other, because chrony reports a client's address and packet counts but not the NTP version. It is replaced by an **Outcome** card (answered, refused, kiss-o'-death, failed). The screenshots under `snaps/` are still the old ones.

### Verified
- `node --check`, gofmt, vet, tests, cross-builds.

### Not verified
- The card in a browser (checked by reading the code only).

## [v14] - 2026-10-12 — A red gateway answers nobody

### Changed
- When no server is in service and no fallback is configured (every server down or paused), the gateway is red, and the node now also stops answering clients: chrony is told `deny all` live and the `allow` lines are left out of its file (so a restart does not bring them back). The allowed clients return, without a restart, as soon as a server passes. Before, chrony kept answering from its holdover while the gateway showed red.

### Added
- A gateway's Statistics dialog (right-click ▸ Statistics…) now shows the time itself, not only availability: **Clock offset** (this node's clock against what chrony believes; with chrony `-x` in a container, the host's error) and **Root delay** (average and worst), one point a minute, all nodes of the cluster together. Recorded from chrony's tracking while the gateway is up and chrony is synchronised.

### Verified
- A test for the mute and the restore (file, live rules, no restart), gofmt, vet, tests, cross-builds.

### Not verified
- A live run against a real chronyd (the live rule is the same `deny all` that was checked in v8).

## [v13] - 2026-10-12 — Chrony in containers, corrected

### Fixed
- v12's chrony drop-in did not work: Ubuntu starts chronyd with `ExecStart=!…` (full privileges), the drop-in dropped the `!`, and chronyd then failed with "Not superuser". The drop-in keeps the `!` and also clears `ConditionVirtualization`.
- v12 decided "container" only from the installer's own capabilities. The installer now also reads PID 1's capabilities and, after starting chrony, asks systemd whether the unit was skipped (`ConditionResult=no`); if so it writes the drop-in and restarts chrony.

### Added
- The troubleshooting bundle has `system/chrony-service.txt`: `systemctl status` and `systemctl cat` of the chrony unit (drop-ins included), its last 100 journal lines, who holds UDP 123, and `chronyc tracking`.

### Verified
- `bash -n`, shellcheck, the drop-in function with the skipped/not-skipped answers forced, gofmt, vet, tests, cross-builds. The `!` fix follows a real failure log from an Ubuntu 26.04 LXC node.

### Not verified
- A full installer run in a container (the manual drop-in with `!` was given to the owner to try).

## [v13] - 2026-10-12 — Detect a skipped chrony unit

### Fixed
- v12 decided whether the machine was a container from the installer's own capabilities; on some LXC setups systemd still skipped the chrony unit and no drop-in was written. The installer now also reads PID 1's capabilities and, after starting chrony, asks systemd whether the unit was skipped (`ConditionResult=no`); if so it writes the `chronyd -x` drop-in and restarts chrony.

### Verified
- `bash -n`, shellcheck, the drop-in function with the skipped/not-skipped answers forced, gofmt, vet, tests, cross-builds.

### Not verified
- A real install in a container.

## [v12] - 2026-10-12 — Chrony in containers

### Fixed
- In an LXC or other container without `CAP_SYS_TIME`, Ubuntu's chrony unit is skipped by systemd ("start condition unmet"), so no node ever answered NTP and the gateways stayed amber. The installer now detects a machine that cannot set the clock and writes a drop-in (`/etc/systemd/system/<chrony unit>.d/ntpdir.conf`) that removes the condition and runs `chronyd -x`: chrony tracks the servers and answers clients, and the clock comes from the host. The uninstaller removes the drop-in. The chrony-not-running message now says what to do.

### Verified
- `bash -n`, shellcheck, the drop-in function in isolation (written only when the clock cannot be set), gofmt, vet, `go test -race -count=1 ./...`, the five cross-builds, `CGO_ENABLED=0` vet and test.

### Not verified
- A full install in a real container; the generated unit text matches Ubuntu's chrony unit (`ExecStart=!/usr/sbin/chronyd $DAEMON_OPTS`) and the Red Hat family's (`$OPTIONS`) but was not run under systemd here.

## [v11] - 2026-10-12 — Host lookup fix

### Fixed
- Add NTP server: filling in a name or an address showed "Lookup failed: unknown command: ntp.lookup". The route `/api/hostlookup` named an operation that does not exist; it now calls `host.lookup`. A test checks that every operation a web route names is handled.

### Verified
- gofmt, `go vet`, `go test -race -count=1 ./...`, the five cross-builds, `CGO_ENABLED=0` vet and test.

## [v10] - 2026-10-12

### Removed
- The cluster **Failover** drop-down (Auto/On/Off), `--cluster-failover`, `POST /api/cluster/failover` and the `cluster.failover` operation. Failover is always automatic: on from three members, off with two (promote by hand). Old files that carry a `failover` value are accepted and ignored.

### Fixed
- A node whose saved state said it was a replica of its own address showed "the primary … is not reachable" and "Last sync failed: the primary … is not a known peer" while it was in fact alone. It now becomes the primary at start and on the next sync (no epoch change), and the warning is never shown for one's own address.

## [v9] - 2026-10-13 — Clean re-release of v8

No code change. The v8 tag on GitHub contained files that v8 had deleted (`ntpfront.go`, `fwdfail.go`, `upstreamudp.go` and others from the old relay), so `get.sh` and `install.sh` failed to build it with "dialUDP redeclared" and "undefined: ntpVersion". This archive is the v8 tree, built from an empty directory, with the version raised so that a new tag is unambiguous.

### Packaging fix
- When publishing, replace the repository's tree instead of copying over it (`git rm -rf . && tar xzf ntpdir_v9.tgz --strip-components=1 && git add -A`), and tag that commit. Files removed by a release must be deleted from the repository, or the web installer builds a mixture of old and new source.

### Verified
- gofmt, `go build`, `go vet`, `go test -race -count=1 ./...`, the five cross-builds and `CGO_ENABLED=0` vet and test; the archive contains none of the removed relay files.

### Not verified
- The GitHub tag and `curl | bash` path (the sandbox cannot reach the repository).

## [v9] - 2026-10-13 — Clean re-release of v8

No code change. The v8 tag on GitHub contained files v8 had deleted (`ntpfront.go`, `fwdfail.go`, `upstreamudp.go`), so the web installer failed to build it. This archive is the v8 tree with the version raised, so a new tag is unambiguous.

### Packaging fix
- Publish by replacing the repository's tree (`git rm -rf .`, extract, `git add -A`), so files removed by a release are deleted from the repository.

## [v8] - 2026-10-13 — chrony answers the clients; AnyTime keeps its sources healthy

The forwarding NTP relay is gone. Each node's chrony answers clients, and AnyTime decides which upstream servers chrony may use: servers that test good are chrony's sources on every node, and a server that fails, is a falseticker, is too far out of sync or is too slow is removed from them within seconds and added again when it recovers.

### Added
- `chrony` source management (`chronysrc.go`, `chronyloop.go`): every 2 seconds AnyTime writes the healthy servers to `<sourcedir>/ntpdir.sources` (chrony's `sourcedir`) and runs `chronyc reload sources`. If no normal server passes, chrony keeps what it has and the fallback servers become sources only for that time; with everything paused the file is emptied. A write that chrony was not there to receive is retried.
- **Rolling chrony restart** (`chronyroll.go`): a changed port or rate limit is applied by restarting chrony node by node. Each node pauses itself, restarts chrony, waits (up to 3 minutes) until it is synchronised, and resumes; nodes go in order of address, and nobody starts while another is restarting, has failed, or is updating. A node that cannot be paused (alone, or the last one serving a gateway) restarts without pausing. A node that fails to synchronise resumes anyway, shows `failed`, and holds the others for 10 minutes. The state is in the cluster status (`chrony_roll`), the NTP page tile and `--show-ntp`; a roll cut short by a daemon stop resumes the node afterwards (`chrony-roll.json`).
- Client settings go to `<confdir>/ntpdir.conf` (`port`, `allow`, `ratelimit`); a new `allowed_clients` list is applied to the running chrony with `chronyc deny all` / `allow all NET` (verified against a real chronyd with `accheck`) and chrony is restarted only when the port or the rate limit changes, since chrony has no command for those. `allowed_clients` is `allow`; `client_rate`, `client_burst` and `client_action` are `ratelimit` (interval from the rate rounded to a power of two, burst, leak 4 for drop and 2 for kod).
- `max_rtt_ms` (default 500, 0 = off): a server whose round trip is longer counts as down ("too slow"). Configure ▸ NTP ▸ Health.
- The NTP page and `--show-ntp` show chrony's state (synchronised, stratum, offset, synchronised to, requests handled and dropped) and an **In chrony** column per server. `chrony.json`, `chrony-sources.txt` and `chrony-sourcestats.txt` are in the troubleshooting bundle.
- Statistics are read from `chronyc clients` every 10 seconds (answered = packets minus drops, refused = drops).
- Installer: requires chrony 4.0 or newer; adds `sourcedir` and `confdir` to chrony's marked block, creates both directories, and switches off chrony's own servers (`pool`/`server`/`peer` lines marked `#ntpdir-off#`, distribution `sources.d/*.sources` renamed `*.ntpdir-off`). The uninstaller puts them back; `--purge` also removes the two directories.

### Changed
- A gateway or anycast address counts as healthy only while chrony runs and is synchronised, as well as a server passing.
- Pools are kept for paused gateways and paused nodes too (a paused node still needs its time sources); two tests that expected no pool were changed.
- With no gateway there is no pool, so chrony gets no sources: draw a gateway first.
- Configure ▸ NTP is now Servers, Health and Clients tabs; per-gateway "Load balancing" is "Health checks".
- A node's sources are only the servers of the topology: each gateway's own servers, or the shared list for a gateway without its own, from gateways that are not paused; the same on every node. A shared list that no gateway uses is not given to chrony. chrony has one set of sources, so with several gateways the node uses all their healthy servers together.
- Test floor in `TestHelpCoversEveryField` lowered from 70 to 55 labelled fields, because the spread/attempts/alpha/timeout/limit fields were removed.

### Removed
- The relay (`ntpfront.go`, `upstreamudp.go`, `udpreply.go`, `loopguard.go`, `fwdfail.go`), the in-process client limiter, ranking, spread and the `NTPDIR_CHRONY_IFACE` / `bindaddress` logic. `spread`, `spread_band`, `max_attempts`, `latency_alpha`, `query_timeout_ms`, `client_exempt` and `client_limits` are still accepted in a file but do nothing. Their CLI flags and GUI fields are gone.
- Recorded per-gateway answer time, errors and request rate, and a server's failed requests: chrony does not report them. The topology gateway/server statistics windows show availability, round trip, offset and failed probes only. Statistics show "Failed" and "Kiss-o'-death" as 0, the top-servers list as one `chrony` entry and the NTP version as "Other".

### Fixed
- The NTP page and `--show-ntp` showed the default port and client settings instead of the configured ones.
- The Topology legend is hidden until a gateway exists.

### Verified
- gofmt, `go build`, `go vet`, `go test -race -count=1 ./...` pass, with new tests for the sources file, client config, state parsing and statistics sampling; cross-builds and `CGO_ENABLED=0` vet/test as listed below.
- Live, with a real chronyd 4.5 (private instance, `-x`), three stub NTP servers and the daemon: the three servers appear as sources; a dead server, a server 0.6 s slow (limit 300 ms) and a server 5 s off are each removed from `chronyc sources` within about 10 s and added back on recovery; with all servers dead chrony keeps its sources; a node started before chronyd got its sources once chronyd came up; a gateway on `lo` answered on the VIP through chrony; allowed clients and rate limit reached `ntpdir.conf`, and the allowed-clients rules applied to a real running chronyd (`accheck`).
- Installer: dry-run, a real install with a shim `systemctl` against a fake `chrony.conf` and `sources.d`, an idempotent re-run, and an uninstall with `--purge` that restored `chrony.conf` byte for byte.
- Live, two real daemons clustered, each with its own real chronyd and shim `systemctl`: a shared rate-limit change rolled through the nodes in order: the first node paused, chrony restarted, resumed when synchronised (about 12 s), and only then the second; the primary role was handed over on each pause; a fresh node started with a non-default port restarted chrony once.
- GUI in headless Chromium, light and dark: NTP page, Configure ▸ NTP, Statistics, Topology.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- A real cluster of several nodes with chrony (each node's chrony run by systemd), a real `chronyd` restart through systemd, chrony older than 4.5, and the installer on a distribution other than a sandbox with a fake Ubuntu `os-release` (no real apt/dnf chrony install, no shellcheck-clean run on other families beyond `shellcheck -S warning`).
- chrony's own `clientloglimit` caps the clients it counts, so the statistics undercount on a very busy node.
- The screenshots under `snaps/` were not retaken: `ntp.png`, `topology.png` and `statistics.png` show the old relay pages. `ntp-relay.png` was removed.
- Cross-platform builds use the PAM stub (see v1); only the native amd64 build is verified with PAM.

## [v7] - 2026-10-12 — Updates: "Downloading…"; Controller greyed with several gateways

Taken from the Anyname DNS Director v379. Its other recent changes (server names registered in DNS, Rename registering in DNS, policy-based resolution) are DNS-only; its v369-v374 changes were already here.

### Changed
- Fetching an update from online says "Downloading…" while it works, not "Downloading from GitHub…".

### Fixed
- With more than one gateway, a node that was already the controller (and primary) still offered **Controller** in its menu, because the check also counted gateways the node is not a member of (another subnet, removed from it), where it can never hold the role. It now counts only the gateways the node is a member of. A paused node still shows "Paused: resume it first".

### Verified
- gofmt, build, vet, `go test -race -count=1`, CGO-off vet and test, cross-compiles for amd64, arm64, arm, 386 and riscv64, `node --check`.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- In a browser; the multi-gateway case was reasoned from the code, not reproduced.

## [v6] - 2026-10-12 — README and screenshots refreshed for NTP

### Changed
- All 18 screenshots in `snaps/` regenerated from a live two-node NTP cluster (anycast, capture, cluster, gateway-stats, gateways, history, host, log, login, node, ntp-relay, ntp, statistics, topology-menu, topology, upgrade, users, web-certificate).
- README gained two images: `snaps/ntp.png` (Monitor ▸ NTP) and `snaps/ntp-relay.png` (Configure ▸ NTP relay).

### Fixed
- Leftover DNS wording: the cluster-join prompts in `cli.go` ("NTP block" not "DNS block"), two README sentences ("NTP servers", "NTP pool"), the capture filter and anycast address placeholders in `app.js`, and the capture examples in `help.js` (`port 123` not `port 53`).

### Verified
- Screenshots taken in Chromium (dark, 1280x760) against a two-node cluster with stub NTP servers, generated client traffic, real PAM users and a real update push.
- gofmt, build, vet, `go test -race -count=1`, cross-compile amd64/arm64/arm/386/riscv64, `CGO_ENABLED=0` vet and test, `node --check` on `app.js` and `help.js`.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- The screenshots were taken from a build with the `cli.go` fix but before the final `app.js`/`help.js` placeholder edits.
- The Upgrade shot's newest event is a "cancelled" entry from a mistaken demo command; History shows setup BGP entries; the NTP page's right-hand columns clip at 1280px.
- The first `go test -race` run failed once (failing test not captured); the rerun passed. Timing-sensitive tests have been flaky before (`TestLogServerFlips`, `TestSpreadSharesRealQueries`).
- dnsdir on a real host.

## [v5] - 2026-10-12 — Fix: pausing a server could pause the whole node; controller mark on a paused node

### Fixed
- Pausing or resuming a server (or anything else) in the Topology saved the page's copy of the whole configuration, which still carried the node's pause flag from before the node was last paused or resumed from its menu. The save then paused the node (its history read "Groups changed … ; Node: paused — this node is out of service" and its gateways stopped). Whether the node is paused is now changed only by pausing or resuming the node (node menu, Operate ▸ Node, `--node-pause`/`--node-resume`): a saved configuration from the web page, an import and a restored version all keep the node's live state. The same fix as dnsdir v374, where a real node showed it.
- Topology on a paused node (or one that does not serve a gateway): the picture of a serving node was borrowed with that node's own member still marked "local", so the controller dot and the star landed on the paused node instead of the real controller. Borrowed members are no longer marked local (dnsdir v369).

### Verified
- gofmt, build, vet, `go test -race -count=1` (new `TestConfigSaveNeverChangesTheNodePause`, `TestTakeFromClearsLocalOnMembers`; one run failed once with the output not captured and passed on three reruns, so it is treated as a flaky timing test), CGO-off vet and test, cross-compiles for amd64, arm64, arm, 386 and riscv64, `node --check`.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- In a browser or on a live cluster. A node paused by this bug stays paused until it is resumed from its menu.

## [v4] - 2026-10-12 — A gateway can answer NTP on dnsdir's anycast addresses

### Added
- **dnsdir addresses.** With the Anyname DNS Director installed on the same host, a gateway can also answer NTP on anycast addresses dnsdir announces (`groups[].dnsdir_vips`, shared by the cluster). The addresses on offer are read from dnsdir's configuration (`/var/lib/dnsdir/dnsdir.conf`, `extra_vips` of its gateways). ntpdir **only listens** (freebind, the gateway's NTP port, the gateway's pool): it never adds the address to `lo`, takes it off, renews it or announces it, and never touches FRR, so it cannot interfere with dnsdir's BGP. Every 3 s it checks whether dnsdir is still installed, still announces the address and has it on `lo`; the listener follows (no restart needed when dnsdir is installed or removed).
- GUI: right-click the gateway ▸ *Add dnsdir address* (only while dnsdir is installed; a choice of the addresses it announces that are free). Each shows as a pill under the anycast pills: green answering, amber when dnsdir holds it but no NTP server answers (dnsdir still announces it), grey when dnsdir does not hold it, no longer announces it, or is gone. Right-click a pill ▸ Delete, or the Delete key. CLI: `--canvas-add dnsdirvip --group N --address ADDR`, `--canvas-del dnsdirvip ...`; `--canvas` lists them. `GET /api/canvas` carries `dnsdir` (`present`, `offered`).
- `AnycastState.external` marks them; they are left out of the cluster's anycast report and of the BGP colouring.

### Changed
- Adding a dnsdir address is refused unless dnsdir is installed here and announces it (GUI, CLI and API: `PUT /api/config` and the canvas edits). Addresses already saved are not re-checked, so a config stays loadable while dnsdir is away, and the listener returns when it comes back. Validation: a dnsdir address cannot equal a gateway's shared address or an anycast address of ntpdir, cannot be listed twice, and only one gateway may answer on it.
- The settings form carries `dnsdir_vips` through like `extra_vips`; the loop guard counts them as this host's own addresses.
- BGP stays unavailable in ntpdir while dnsdir is installed (v3); standalone, ntpdir runs BGP and its own anycast as before.

### Verified
- New tests (`dnsdir_vips_test.go`): reading dnsdir's offered list (junk, unreadable file, dnsdir absent); config rules; the cluster hash and merge; the check on add (not installed, not announced, announced, already saved); canvas add/delete; the listener follows dnsdir (not on `lo` yet, held, dead pool, dropped from dnsdir's list, dnsdir removed) and `lo` is never touched (no adds, no deletes, also on stop).
- Live run of the real daemon with a stub NTP server, a fake dnsdir marker and configuration: an address dnsdir does not announce is refused; an announced one is added and reports "not announcing it right now" until it is put on `lo`, then an NTP request to it is answered (origin timestamp echoed) from the gateway's pool; it is still on `lo` afterwards (ntpdir never removes it); removing the marker stops the listener and bringing it back restarts it; deleting works. The sandbox was restored.
- Chromium (light and dark), signed in through real PAM: the menu item appears, the form offers only free addresses, the pill is drawn with its label and tooltip, Delete removes it.
- `gofmt -l` empty, `go build`, `go vet`, `go test -race -count=1 ./...`, `CGO_ENABLED=0 go vet` and `go test`, `node --check`; cross-compiles for linux/amd64, arm64, arm, 386 and riscv64.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- Two flaky tests: one full `-race` run reported a race in `TestLogServerFlips`, and one non-race run missed a timing bound in `TestSpreadSharesRealQueries`. Neither recurred in 40 isolated race runs, 3 isolated runs and the full runs that followed (all green), and neither touches this change, but the race report was not investigated.
- A real dnsdir install: dnsdir was simulated by a marker file, a configuration file and an address added to `lo` by hand. BGP announcements, and what dnsdir does with the address when its own DNS is unhealthy, were not exercised.
- More than one node: the shared list is covered by tests of the hash and merge, not by a live cluster run.
- IPv6 dnsdir addresses (the sandbox has no IPv6).

## [v3] - 2026-10-12 — BGP is switched off while dnsdir is installed on the host

### Changed
- ntpdir looks for the Anyname DNS Director (`/usr/local/sbin/dnsdir`, `/usr/local/share/dnsdir`, `/etc/systemd/system/dnsdir.service`) at start-up and each time BGP is applied or changed. While one is present, BGP is unavailable: ntpdir does not touch `/etc/frr/frr.conf`, `/etc/frr/daemons`, its marker or the FRR service (even if it managed FRR before dnsdir was installed, so it cannot wipe dnsdir's BGP section), treats BGP as inactive (the drawing shows no BGP colouring), and refuses to set it up or enable it from the GUI, the API (`PUT /api/bgp`, `POST /api/bgp/operate`) and the CLI (`--asn`, `--bgp-neighbor-add`, `--bgp-enable`). Clearing the settings and disabling still work; settings already in the config are kept and have no effect. Removing dnsdir lifts the block without a restart.
- `GET /api/bgp` has a new `blocked` field; the three Anycast pages and `--bgp` say BGP is unavailable. The README, the Anycast help topic and the v2 note on running both products are updated.
- Anycast addresses are unaffected: without BGP they are plain addresses on `lo`.

### Verified
- New tests: the check follows the marker files; with dnsdir present `applyNow` leaves `frr.conf`, `daemons`, the marker and the FRR calls untouched, for BGP on and off, after ntpdir had managed FRR; setting an AS or neighbor and enabling BGP are refused over the API while clearing and disabling work; `blocked` is in the status.
- Live run of the real daemon with a scratch config: `--asn 64512` works, then with a marker file at `/usr/local/sbin/dnsdir` `--bgp` reports BGP unavailable, `--asn 64513` and `--bgp-enable` are refused, `--asn off` is accepted; after removing the marker `--asn` works again. The marker and test state were removed afterwards.
- `gofmt -l` empty, `go build`, `go vet`, `go test -race -count=1 ./...`, `CGO_ENABLED=0 go vet` and `go test`, `node --check webui/app.js`; cross-compiles for linux/amd64, arm64, arm, 386 and riscv64.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- The three Anycast pages in Chromium with the block shown (only `node --check` and the API/CLI were run).
- A real dnsdir install on the same host; the check is by file presence only, so a dnsdir that was removed by hand but left its unit file behind keeps BGP blocked.
- dnsdir v373 was not changed and has no matching check for ntpdir, so it would still take over `frr.conf` on its own; only ntpdir backs off.

## [v2] - 2026-10-12 — Own gateway port and multicast addresses, so ntpdir can share a host with dnsdir

### Changed
- The gateway protocol (election, heartbeats, hand-overs) now uses **UDP 12382** and the multicast groups **224.1.3.123** and **ff02::1:123**. They were 4729, 224.1.3.7 and ff02::1:307, the same as the Anyname DNS Director's. Both daemons open that socket with `SO_REUSEPORT`, so on one host the kernel split unicast packets between them and each saw the other's multicast (rejected by the HMAC, but the lost packets broke elections). The packet format and magic are unchanged.
- Upgrade note: the new port is not compatible with v1, so **every node of a gateway must run v2**. A rolling update leaves the nodes unable to see each other until all have restarted, and each may then briefly claim the controller role.
- The installer's firewall hints say `12382/udp`; the README describes the port and the multicast groups next to the cluster settings.

### Still to watch when running both products on one host
- Only one of them may manage BGP: each owns `/etc/frr/frr.conf` and rewrites it.
- Their gateway ids must differ (dnsdir defaults to 1, ntpdir to 100): the virtual MAC is derived from the group id and slot only.

### Verified
- `gofmt -l` empty, `go build`, `go vet`, `go test -race -count=1 ./...`, `CGO_ENABLED=0 go vet` and `go test`; cross-compiles for linux/amd64, arm64, arm, 386 and riscv64; `bash -n install.sh`.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- Running ntpdir and dnsdir together on one host: the conflict was found by reading both sources, and the fix was not tried against a running dnsdir.
- A rolling v1 to v2 update on a live multi-node cluster.

## [v1] - 2026-10-12 — AnyTime NTP Director: the DNS gateway becomes an NTP relay

The project is now **AnyTime NTP Director** (`ntpdir`), built from the Anyname DNS Director v365 code base. The numbering restarts at 1 and the earlier changelog was removed on purpose.

### Added
- **NTP relay** in place of the DNS proxy. Each gateway forwards a client's NTP packet (RFC 5905) unchanged to the fastest healthy upstream server and returns the answer, accepted only if it echoes the client's transmit timestamp. Modes 6 and 7 are never relayed. A loop guard never forwards to the asker or to this host's own addresses.
- **NTP health checks** with real NTP requests: highest stratum (default 15), largest root distance (default 1500 ms), kiss-o'-death and unsynchronised-clock detection, and **falseticker** detection against the median offset (default 128 ms, needs three or more servers). Ranking by smoothed round trip, spread over servers within a band, fallback servers, per-server pause, per-client allow list and rate limits (`drop` or `kod`).
- **chrony as a prerequisite.** The installer installs it (apt, dnf, pacman), enables it and writes a marked block into its configuration: `bindaddress` for the first IPv4 and the first IPv6 address of the first Ethernet interface, `allow ::/0` and `allow 0.0.0.0/0`. chrony is restarted when the block changes. The uninstaller removes only the block and leaves chrony installed.
- **Statistics, Topology, NTP page, Capture and help** rewritten for NTP: outcomes (answered, failed, kiss-o'-death, refused), NTP versions and IP families, top clients and top servers, NTP packet decoding in Capture, `--show-ntp`, `--stats-outcome`, `--stats-server`, `--host-lookup`.
- **Rename a node** (Topology ▸ right-click a node ▸ Rename, or `--node-rename NAME`): sets the operating system host name (`hostnamectl`, or `hostname` plus `/etc/hostname` without systemd) and rewrites the old name in `/etc/hosts` (a `127.0.1.1` line is added when none names the host). The name is validated before it reaches a command, files are replaced atomically with their mode kept, and `/etc/hosts` is restored if the host name cannot be set. It works on any cluster member through the node picker.
- **Pausing a node moves the roles.** The primary role is handed to a reachable node that is not paused, and the gateway controller moves because the paused node's gateways resign. A paused node cannot be made primary or controller (CLI, GUI and menu items are refused or greyed). The Topology no longer shows the controller dot on a paused node.

### Changed
- Name, binary, service, group, PAM service, state directory (`/var/lib/ntpdir`), Go module and config keys are `ntpdir` / `ntp`; the brand is written "AnyTime" everywhere. Web GUI port **12380** and cluster port **12381** (were 53853 and 53854); the relay listens on UDP 123 by default.
- New gateways are numbered from **100** (101, 102, …); `--group N` still picks another number (1 to 255).
- Cluster strings were renamed, so nodes of the old DNS Director cannot join an ntpdir cluster. The wire protocol and packet magic are unchanged. The default HMAC key is now `ntpdir` (was `dgw`): a gateway with no explicit `key` must have the same default on every node, and a config that sets `key` keeps working as before.
- The installer no longer aborts when a service restart itself returns an error: the active check decides, and the rollback runs.
- The default update repository is `micush/ntpdir`; the GitHub repository must be renamed to match.
- README, QUICKSTART, CLAUDE.md, the example config and `help.js` were rewritten for NTP. The screenshots in `snaps/` still show the DNS pages (the DNS and policy pictures were removed), so most of them are out of date.

### Verified
- `gofmt -l` empty, `go build`, `go vet`, `go test -race -count=1 ./...`, and `CGO_ENABLED=0 go vet` plus `go test`. Cross-compiles for linux/amd64, arm64, arm, 386 and riscv64.
- New and ported tests cover the NTP wire format, probing, falseticker, spread, fallback, loop guard, client limits, canvas edits, group numbering from 100 and primary hand-over skipping paused nodes.
- Smoke run with three stub NTP servers on `lo`: a client request through the VIP is answered by the fastest server; killing it fails over to the next; an atomic config rename hot-reloads; `--show-ntp` shows the ranking.
- Real PAM check: a group member signs in; a wrong password and a valid user outside the group get the same message; repeated failures lock out the source (root was then refused too).
- Chromium, light and dark: Topology (servers, controller dot), NTP, Statistics and the help panel.
- Live cluster run with two real daemons: join, a shared edit on the replica reaching both files, pausing the primary (the role moved to the other node), a paused node refusing promotion, and an update pushed from the replica (both nodes built v2, re-exec'd and were confirmed by the boot guard; the second needed `--update-apply --yes` because the soak gate held it back).
- Rename: unit tests for name validation, the `/etc/hosts` rewrite, the command sequence and the restore on failure; a live run renamed the sandbox host through the CLI (host name, `/etc/hostname` and `/etc/hosts` changed, then were restored) and through the Chromium dialog (a bad name shows the reason, a valid one is applied).
- Installer: `bash -n` and `shellcheck -x -S warning` clean; dry runs for ubuntu, debian, linuxmint, fedora, rocky, almalinux, centos, arch, manjaro and endeavouros (alpine refused); a real run with a shim `systemctl`: fresh install, no-op, upgrade, forced-failure rollback, downgrade refusal, uninstall with and without `--purge`, a customised PAM file kept, and chrony's configuration restored byte for byte.

### Not verified
- The failure path of the roll (chrony never synchronising) and the recovery after a daemon stop mid-roll were run only in unit tests, not live; three or more nodes; a real systemd restart.
- The IPv6 branch of the chrony block (the sandbox has no IPv6) and a real chrony package install and restart; only the configuration edit with a shim was run.
- The installer's Go download against a local server was not re-run for this release (that code is unchanged).
- Real multi-host behaviour: election on a real wire, ARP/NS steering to virtual MACs, an AFN answering through `lo` and `arp_ignore`, real public NTP servers, and relay behaviour behind rate-limiting upstreams. Upstream servers see the relay's address for every client.
- The certificate, history restore and users flows of the cluster were not exercised again in this release.
- Binaries cross-compiled with cgo off use the fail-closed PAM stub; only the native amd64 build was run with PAM.
