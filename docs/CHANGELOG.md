# Changelog

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
- The IPv6 branch of the chrony block (the sandbox has no IPv6) and a real chrony package install and restart; only the configuration edit with a shim was run.
- The installer's Go download against a local server was not re-run for this release (that code is unchanged).
- Real multi-host behaviour: election on a real wire, ARP/NS steering to virtual MACs, an AFN answering through `lo` and `arp_ignore`, real public NTP servers, and relay behaviour behind rate-limiting upstreams. Upstream servers see the relay's address for every client.
- The certificate, history restore and users flows of the cluster were not exercised again in this release.
- Binaries cross-compiled with cgo off use the fail-closed PAM stub; only the native amd64 build was run with PAM.
