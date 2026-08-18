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

| Name       | URL                                     |
| ---------- | --------------------------------------- |
| systemtap  | https://debuginfod.systemtap.org        |
| opensuse   | https://debuginfod.opensuse.org         |
| voidlinux  | https://debuginfod.s.voidlinux.org      |
| debian     | https://debuginfod.debian.net           |
| fedora     | https://debuginfod.fedoraproject.org    |
| altlinux   | https://debuginfod.altlinux.org         |
| archlinux  | https://debuginfod.archlinux.org        |
| artixlinux | https://debuginfod.artixlinux.org       |
| centos     | https://debuginfod.centos.org           |
| ubuntu     | https://debuginfod.ubuntu.com           |
| alpine     | https://debuginfod.achill.org           |
| nix        | our custom debuginfod server for nix/nixpkgs    |

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

## Related projects

- [pwndbg](https://github.com/pwndbg/pwndbg) — GDB & LLDB plugin for exploit dev and reverse engineering
- [debuginfod-zig](https://github.com/pwndbg/debuginfod-zig) — `libdebuginfod` rewritten in Zig
- [elfutils debuginfod](https://sourceware.org/elfutils/Debuginfod.html) — the reference server and protocol

## License

See [LICENSE](./LICENSE).
