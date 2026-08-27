# debuginfod.pwndbg.re

A federating [debuginfod](https://sourceware.org/elfutils/Debuginfod.html) proxy that serves debug symbols from many Linux distributions behind a single URL.

Developed by the [Pwndbg](https://github.com/pwndbg/pwndbg) team and hosted at **https://debuginfod.pwndbg.re**. It is enabled by default in Pwndbg portable releases and in `gdb-for-pwndbg`, so debug symbols for system libraries are fetched automatically while you debug.

## Why?

Debuggers and binary tools (GDB, LLDB, elfutils, Valgrind, perf, ...) can download debugging resources on demand over the debuginfod protocol: DWARF debug info, stripped executables, and source files, all looked up by build-id. The catch is that every Linux distribution runs its own debuginfod server, so you either configure a long list of URLs:

```sh
export DEBUGINFOD_URLS="https://debuginfod.ubuntu.com https://debuginfod.debian.net https://debuginfod.archlinux.org ..."
```

...or you get no symbols for binaries that came from a different distro (containers, extracted CTF challenges, foreign rootfs, remote targets, etc.).

This proxy solves that: point your debugger at **one** URL and every build-id lookup is fanned out to the debuginfod servers of the major distributions. The first successful response is streamed back to the client.

## Upstream servers

| Name       | URL                                         | debuginfo | executable | source | Old build ids |
| ---------- | ------------------------------------------- | :-------: | :--------: | :----: | ------------- |
| systemtap  | https://debuginfod.systemtap.org            |     ✅     |     ❌      |   ✅    | kept          |
| opensuse   | https://debuginfod.opensuse.org             |     ✅     |     ✅      |   ✅    | expire       |
| fedora     | https://debuginfod.fedoraproject.org        |     ✅     |     ✅      |   ✅    | kept          |
| archlinux  | https://debuginfod.archlinux.org            |     ✅     |     ❌      |   ✅    | expire        |
| artixlinux | https://debuginfod.artixlinux.org           |     ✅     |     ❌      |   ✅    | expire       |
| cachyos    | https://debuginfod.cachyos.org              |     ✅     |     ❌      |   ✅    | expire       |
| centos     | https://debuginfod.centos.org               |     ✅     |     ✅      |   ✅    | kept          |
| debian     | https://debuginfod.debian.net               |     ✅     |     ❌      | ✅ ours |    expire     |
| nix        | our own debuginfod server for nix / nixpkgs |     ✅     |     ✅      |   ✅    | kept          |

❔ means we have not served enough of that endpoint from that upstream to say. Debian serves no
sources at all — that column is green because we serve them ourselves, see below.

**Old build ids** is measured from our own logs: build ids we served once and later could not.
*expire* means old builds do disappear upstream — around a month for archlinux, a few months for
debian — so a binary from an older release may have no symbols anywhere.

### Not currently queried

Listed so it is clear these are known and deliberate, rather than forgotten. None of them is
contacted, so a build id only they would have comes back as not found.

| Name      | URL                                | Why                                          |
| --------- | ---------------------------------- | -------------------------------------------- |
| ubuntu    | https://debuginfod.ubuntu.com      | disabled — requests hung for ~45 s and never completed |
| alpine    | https://debuginfod.achill.org      | disabled — server offline                    |
| elfutils  | https://debuginfod.elfutils.org    | disabled — server misbehaves                 |
| voidlinux | https://debuginfod.s.voidlinux.org | never configured                             |
| altlinux  | https://debuginfod.altlinux.org    | never configured                             |

## Debian sources

`debuginfod.debian.net` serves debug info and executables, but returns 404 for **every** source
path — so on Debian you get disassembly and symbol names, and no source lines.

We fill that gap ourselves: a Debian build id is resolved to the source package it came from, which
is fetched and unpacked **with the Debian patch series applied**, because the pristine upstream
sources do not line up with the debugger's line numbers. This is new — if a source path should
resolve and does not, please [open an issue](https://github.com/pwndbg/debuginfod.pwndbg.re/issues).

## Usage

### Pwndbg

Nothing to do — the [Pwndbg](https://github.com/pwndbg/pwndbg) portable releases and `gdb-for-pwndbg` ship with `DEBUGINFOD_URLS` already pointing at this server.

### GDB

```sh
export DEBUGINFOD_URLS=https://debuginfod.pwndbg.re
gdb ./your-binary
```

Or persist it in your `~/.gdbinit`:

```
set debuginfod enabled on
set debuginfod urls https://debuginfod.pwndbg.re
```

### LLDB and other clients

Any tool that speaks debuginfod works — LLDB, `debuginfod-find` (elfutils), Valgrind, perf, systemtap, binutils, and more:

```sh
export DEBUGINFOD_URLS=https://debuginfod.pwndbg.re
debuginfod-find debuginfo /usr/lib/libc.so.6
```

On macOS or for static builds, see [debuginfod-zig](https://github.com/pwndbg/debuginfod-zig) — a drop-in `libdebuginfod` replacement.

## HTTP API

The proxy implements the standard [debuginfod webapi](https://sourceware.org/elfutils/Debuginfod.html):

```
GET /buildid/<BUILDID>/debuginfo
GET /buildid/<BUILDID>/executable
GET /buildid/<BUILDID>/source/<SOURCE-PATH>
```

Beyond the protocol, **[/stats](https://debuginfod.pwndbg.re/stats)** is a public page of traffic charts.

## Related projects

- [pwndbg](https://github.com/pwndbg/pwndbg) — GDB & LLDB plugin for exploit dev and reverse engineering
- [debuginfod-zig](https://github.com/pwndbg/debuginfod-zig) — `libdebuginfod` rewritten in Zig
- [elfutils debuginfod](https://sourceware.org/elfutils/Debuginfod.html) — the reference server and protocol

## License

See [LICENSE](./LICENSE).
